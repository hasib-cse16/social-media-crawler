package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

func partitionNames(t *testing.T, db *DB, ctx context.Context) []string {
	t.Helper()
	// Scoped to this test's own schema. Without the filter it would also count
	// partitions belonging to another schema's metric_snapshots, and the count
	// would depend on what else happens to be in the database.
	rows, err := db.Pool.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_inherits i ON i.inhrelid = c.oid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'metric_snapshots'
		  AND n.nspname = current_schema()
		ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("list partitions: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, name)
	}
	return out
}

func TestMigrationCreatesPartitionsAhead(t *testing.T) {
	db, ctx := migrated(t)

	// The current month plus two, so a deployment that never runs the
	// housekeeping job still has somewhere to write for a while.
	if names := partitionNames(t, db, ctx); len(names) != 3 {
		t.Errorf("partitions after migrating = %v, want 3", names)
	}
}

func TestEnsureSnapshotPartitionIsIdempotent(t *testing.T) {
	db, ctx := migrated(t)

	far := time.Date(2030, 3, 15, 12, 0, 0, 0, time.UTC)
	name, err := db.Metrics().EnsureSnapshotPartition(ctx, far)
	if err != nil {
		t.Fatalf("EnsureSnapshotPartition: %v", err)
	}
	if name != "metric_snapshots_2030_03" {
		t.Errorf("partition name = %q", name)
	}

	// Calling again must not fail; two workers crossing a month boundary at the
	// same moment do exactly this.
	again, err := db.Metrics().EnsureSnapshotPartition(ctx, far.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("second EnsureSnapshotPartition: %v", err)
	}
	if again != name {
		t.Errorf("second call named %q, want %q", again, name)
	}

	created, err := db.Metrics().EnsureSnapshotPartitions(ctx, far, 2)
	if err != nil {
		t.Fatalf("EnsureSnapshotPartitions: %v", err)
	}
	if len(created) != 3 || created[2] != "metric_snapshots_2030_05" {
		t.Errorf("created = %v", created)
	}
}

// Snapshots must land in the partition for their own month, not wherever the
// most recent one went.
func TestSnapshotsRouteToTheRightPartition(t *testing.T) {
	db, ctx := migrated(t)
	v := makeVideo(t, db, ctx, domain.PlatformYouTube, "routing")

	now := time.Now().UTC()
	thisMonth := now.Add(-time.Hour)
	nextMonth := now.AddDate(0, 1, 0)

	seedSnapshot(t, db, ctx, v.ID, thisMonth, 100)
	seedSnapshot(t, db, ctx, v.ID, nextMonth, 200)

	for _, tc := range []struct {
		partition string
		at        time.Time
	}{
		{"metric_snapshots_" + thisMonth.Format("2006_01"), thisMonth},
		{"metric_snapshots_" + nextMonth.Format("2006_01"), nextMonth},
	} {
		var n int
		if err := db.Pool.QueryRow(ctx,
			`SELECT count(*) FROM `+pgQuoteIdent(tc.partition)+` WHERE video_id = $1`, v.ID).Scan(&n); err != nil {
			t.Fatalf("count in %s: %v", tc.partition, err)
		}
		if n != 1 {
			t.Errorf("%s holds %d rows for the video, want 1", tc.partition, n)
		}
	}

	// Reads go through the parent and see both.
	history, err := db.Metrics().History(ctx, v.ID, now.AddDate(0, -1, 0), now.AddDate(0, 2, 0), BucketRaw)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 2 {
		t.Errorf("history = %d rows, want both partitions' worth", len(history))
	}
}

// The writer's own safety net: if the housekeeping job has not created the
// partition a reading belongs in, Record creates it and retries rather than
// losing the measurement. This is why the schema has no DEFAULT partition.
func TestRecordCreatesAMissingPartitionAndRetries(t *testing.T) {
	db, ctx := migrated(t)
	v := makeVideo(t, db, ctx, domain.PlatformYouTube, "future")

	// A month far enough out that migration 0004 did not pre-create it.
	future := time.Now().UTC().AddDate(0, 6, 0)
	before := partitionNames(t, db, ctx)

	if err := db.Videos().Record(ctx, v.ID, okOutcome(future, 12_345)); err != nil {
		t.Fatalf("Record: %v", err)
	}

	after := partitionNames(t, db, ctx)
	if len(after) != len(before)+1 {
		t.Errorf("partitions went from %v to %v, want one created on demand", before, after)
	}

	history, err := db.Metrics().History(ctx, v.ID, future.Add(-time.Hour), future.Add(time.Hour), BucketRaw)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 1 || *history[0].ViewCount != 12_345 {
		t.Errorf("the reading was lost: %+v", history)
	}
}

// Bucketing takes the last reading in each bucket, not an average: these are
// cumulative counters, and the mean of two of them is not a number anything
// ever had.
func TestHistoryBucketingTakesTheLastReading(t *testing.T) {
	db, ctx := migrated(t)
	v := makeVideo(t, db, ctx, domain.PlatformYouTube, "buckets")

	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-48 * time.Hour)
	for i, views := range []uint64{100, 150, 190} {
		seedSnapshot(t, db, ctx, v.ID, day.Add(time.Duration(i)*time.Hour), views)
	}
	for i, views := range []uint64{300, 380} {
		seedSnapshot(t, db, ctx, v.ID, day.Add(24*time.Hour+time.Duration(i)*time.Hour), views)
	}

	from, to := day.Add(-time.Hour), day.Add(72*time.Hour)

	raw, err := db.Metrics().History(ctx, v.ID, from, to, BucketRaw)
	if err != nil {
		t.Fatalf("History raw: %v", err)
	}
	if len(raw) != 5 {
		t.Errorf("raw history = %d rows, want 5", len(raw))
	}

	daily, err := db.Metrics().History(ctx, v.ID, from, to, BucketDay)
	if err != nil {
		t.Fatalf("History day: %v", err)
	}
	if len(daily) != 2 {
		t.Fatalf("daily buckets = %d, want 2", len(daily))
	}
	if *daily[0].ViewCount != 190 || *daily[1].ViewCount != 380 {
		t.Errorf("buckets = %d, %d; want the last reading in each day (190, 380)",
			*daily[0].ViewCount, *daily[1].ViewCount)
	}
	// Chronological, despite DISTINCT ON ordering by the bucket expression.
	if !daily[0].CapturedAt.Before(daily[1].CapturedAt) {
		t.Error("bucketed history is not in chronological order")
	}

	hourly, err := db.Metrics().History(ctx, v.ID, from, to, BucketHour)
	if err != nil {
		t.Fatalf("History hour: %v", err)
	}
	if len(hourly) != 5 {
		t.Errorf("hourly buckets = %d, want 5", len(hourly))
	}
}

func TestRecentReturnsOldestFirst(t *testing.T) {
	db, ctx := migrated(t)
	v := makeVideo(t, db, ctx, domain.PlatformYouTube, "spark")

	now := time.Now().UTC()
	for i := range 10 {
		seedSnapshot(t, db, ctx, v.ID, now.Add(-time.Duration(i)*time.Hour), uint64(100-i))
	}

	recent, err := db.Metrics().Recent(ctx, v.ID, 3)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recent) != 3 {
		t.Fatalf("%d rows, want 3", len(recent))
	}
	// A sparkline draws left to right, so it wants oldest first.
	if !recent[0].CapturedAt.Before(recent[2].CapturedAt) {
		t.Error("Recent returned newest first")
	}
	if *recent[2].ViewCount != 100 {
		t.Errorf("newest view count = %d, want 100", *recent[2].ViewCount)
	}
}

func TestRollupIsIdempotentAndSigned(t *testing.T) {
	db, ctx := migrated(t)
	v := makeVideo(t, db, ctx, domain.PlatformYouTube, "rollup")

	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	for i, views := range []uint64{1_000, 1_400, 1_900} {
		seedSnapshot(t, db, ctx, v.ID, day.Add(time.Duration(i*6)*time.Hour), views)
	}

	n, err := db.Metrics().Rollup(ctx, day)
	if err != nil {
		t.Fatalf("Rollup: %v", err)
	}
	if n != 1 {
		t.Errorf("rolled up %d rows, want 1", n)
	}

	daily, err := db.Metrics().Daily(ctx, v.ID, day, day)
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	if len(daily) != 1 {
		t.Fatalf("%d daily rows, want 1", len(daily))
	}
	d := daily[0]
	if *d.FirstViewCount != 1_000 || *d.LastViewCount != 1_900 {
		t.Errorf("first/last = %d/%d, want 1000/1900", *d.FirstViewCount, *d.LastViewCount)
	}
	if d.ViewDelta == nil || *d.ViewDelta != 900 {
		t.Errorf("delta = %v, want 900", d.ViewDelta)
	}
	if d.SampleCount != 3 {
		t.Errorf("sample count = %d, want 3", d.SampleCount)
	}

	// Running it again repairs rather than duplicates, so a missed hourly run
	// costs nothing.
	seedSnapshot(t, db, ctx, v.ID, day.Add(20*time.Hour), 2_500)
	if _, err := db.Metrics().Rollup(ctx, day); err != nil {
		t.Fatalf("second Rollup: %v", err)
	}
	daily, _ = db.Metrics().Daily(ctx, v.ID, day, day)
	if len(daily) != 1 {
		t.Fatalf("%d daily rows after a re-run, want 1", len(daily))
	}
	if *daily[0].LastViewCount != 2_500 || *daily[0].ViewDelta != 1_500 {
		t.Errorf("re-run did not update: %+v", daily[0])
	}
}

func TestRollupRecordsANegativeDelta(t *testing.T) {
	db, ctx := migrated(t)
	v := makeVideo(t, db, ctx, domain.PlatformTikTok, "revised")

	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	seedSnapshot(t, db, ctx, v.ID, day.Add(time.Hour), 500_000)
	seedSnapshot(t, db, ctx, v.ID, day.Add(20*time.Hour), 450_000)

	if _, err := db.Metrics().Rollup(ctx, day); err != nil {
		t.Fatalf("Rollup: %v", err)
	}
	daily, _ := db.Metrics().Daily(ctx, v.ID, day, day)
	if len(daily) != 1 || daily[0].ViewDelta == nil {
		t.Fatalf("daily = %+v", daily)
	}
	if *daily[0].ViewDelta != -50_000 {
		t.Errorf("delta = %d, want -50000; a downward revision is a real measurement",
			*daily[0].ViewDelta)
	}
}

// Retention drops whole partitions, and only ones entirely past the cutoff:
// losing 29 days of data to reclaim one is not a trade worth making.
func TestDropSnapshotPartitionsOnlyWhenFullyExpired(t *testing.T) {
	db, ctx := migrated(t)

	base := time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC)
	for i := range 3 {
		if _, err := db.Metrics().EnsureSnapshotPartition(ctx, base.AddDate(0, i, 0)); err != nil {
			t.Fatalf("ensure: %v", err)
		}
	}

	// Cutoff inside March: January and February are entirely behind it, March
	// is not.
	dropped, err := db.Metrics().DropSnapshotPartitionsBefore(ctx, time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("DropSnapshotPartitionsBefore: %v", err)
	}
	if len(dropped) != 2 {
		t.Fatalf("dropped %v, want January and February", dropped)
	}

	remaining := partitionNames(t, db, ctx)
	for _, name := range remaining {
		if name == "metric_snapshots_2025_01" || name == "metric_snapshots_2025_02" {
			t.Errorf("%s survived", name)
		}
	}
	var found bool
	for _, name := range remaining {
		if name == "metric_snapshots_2025_03" {
			found = true
		}
	}
	if !found {
		t.Error("the partially expired March partition was dropped")
	}
}

func TestAttemptRetentionAndHealth(t *testing.T) {
	db, ctx := migrated(t)
	v := makeVideo(t, db, ctx, domain.PlatformTikTok, "health")

	now := time.Now().UTC()
	statuses := []domain.AttemptStatus{
		domain.AttemptOK, domain.AttemptOK, domain.AttemptOK,
		domain.AttemptBlocked, domain.AttemptError,
	}
	for i, st := range statuses {
		_, err := db.Pool.Exec(ctx, `
			INSERT INTO fetch_attempts (video_id, platform, started_at, duration_ms, status)
			VALUES ($1, 'tiktok', $2, $3, $4)`,
			v.ID, now.Add(-time.Duration(i)*time.Minute), 100+i*50, st)
		if err != nil {
			t.Fatalf("insert attempt: %v", err)
		}
	}

	health, err := db.Metrics().Health(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if len(health) != 1 {
		t.Fatalf("health = %+v, want one platform", health)
	}
	h := health[0]
	if h.Attempts != 5 || h.Succeeded != 3 || h.Blocked != 1 || h.Errored != 1 {
		t.Errorf("health = %+v", h)
	}
	if got := h.SuccessRate(); got < 0.59 || got > 0.61 {
		t.Errorf("success rate = %.2f, want 0.60", got)
	}
	if h.P95MS < h.P50MS {
		t.Errorf("p95 (%d) below p50 (%d)", h.P95MS, h.P50MS)
	}

	// Old rows are pruned; recent ones are kept.
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO fetch_attempts (video_id, platform, started_at, duration_ms, status)
		 VALUES ($1, 'tiktok', now() - interval '30 days', 100, 'ok')`, v.ID); err != nil {
		t.Fatalf("insert old attempt: %v", err)
	}
	n, err := db.Metrics().DeleteAttemptsBefore(ctx, now.Add(-14*24*time.Hour))
	if err != nil {
		t.Fatalf("DeleteAttemptsBefore: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d attempts, want 1", n)
	}
	attempts, _ := db.Metrics().Attempts(ctx, v.ID, 100)
	if len(attempts) != 5 {
		t.Errorf("%d attempts survived, want 5", len(attempts))
	}
}

// A counter the platform does not report must stay absent, all the way from the
// domain through the column and back. Writing 0 would make an Instagram photo
// post look like a video nobody watched.
func TestAbsentCountersStayAbsent(t *testing.T) {
	db, ctx := migrated(t)
	v := makeVideo(t, db, ctx, domain.PlatformMeta, "photo")

	at := time.Now().UTC()
	next := at.Add(time.Hour)
	err := db.Videos().Record(ctx, v.ID, domain.FetchOutcome{
		Stats: &domain.VideoStats{
			Platform:  domain.PlatformMeta,
			VideoID:   "photo",
			Title:     "A carousel",
			LikeCount: domain.U64(8_702),
			// No view count: a photo post has none.
			FetchedAt: at,
		},
		Status: domain.FetchOK, AttemptStatus: domain.AttemptOK,
		StartedAt: at, NextFetchAt: &next,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, _ := db.Videos().ByID(ctx, v.ID)
	if got.Latest.ViewCount != nil {
		t.Errorf("view count = %d, want nil for a post with no views", *got.Latest.ViewCount)
	}
	if got.Latest.LikeCount == nil || *got.Latest.LikeCount != 8_702 {
		t.Errorf("like count = %v, want 8702", got.Latest.LikeCount)
	}

	history, _ := db.Metrics().History(ctx, v.ID, at.Add(-time.Hour), at.Add(time.Hour), BucketRaw)
	if len(history) != 1 || history[0].ViewCount != nil {
		t.Errorf("snapshot view count = %+v, want nil", history)
	}
}

// Zero is a real measurement and must not be confused with absent.
func TestZeroCountersAreStoredAsZero(t *testing.T) {
	db, ctx := migrated(t)
	v := makeVideo(t, db, ctx, domain.PlatformYouTube, "zero")

	at := time.Now().UTC()
	next := at.Add(time.Hour)
	err := db.Videos().Record(ctx, v.ID, domain.FetchOutcome{
		Stats: &domain.VideoStats{
			Platform: domain.PlatformYouTube, VideoID: "zero",
			ViewCount: domain.U64(0), FetchedAt: at,
		},
		Status: domain.FetchOK, AttemptStatus: domain.AttemptOK,
		StartedAt: at, NextFetchAt: &next,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, _ := db.Videos().ByID(ctx, v.ID)
	if got.Latest.ViewCount == nil {
		t.Fatal("a genuine zero came back as absent")
	}
	if *got.Latest.ViewCount != 0 {
		t.Errorf("view count = %d, want 0", *got.Latest.ViewCount)
	}
}
