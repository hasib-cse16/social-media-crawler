package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

// Repository tests. They run against a real PostgreSQL because the behaviour
// under test — SKIP LOCKED, LATERAL, partition routing, citext uniqueness,
// ON CONFLICT — is exactly what a mock would only assert back at us.

// migrated returns a DB with an empty, fully migrated schema.
func migrated(t *testing.T) (*DB, context.Context) {
	t.Helper()
	db := testDB(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db, ctx
}

func makeUser(t *testing.T, db *DB, ctx context.Context, email string) *domain.User {
	t.Helper()
	u, err := db.Users().Create(ctx, domain.NewUser{
		Email:        email,
		PasswordHash: "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA",
		DisplayName:  "Test User",
	})
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return u
}

func makeVideo(t *testing.T, db *DB, ctx context.Context, platform domain.Platform, id string) *domain.Video {
	t.Helper()
	v, err := db.Videos().Upsert(ctx, domain.NewVideo{
		Platform:        platform,
		PlatformVideoID: id,
		CanonicalURL:    "https://example.test/" + id,
	})
	if err != nil {
		t.Fatalf("upsert video %s: %v", id, err)
	}
	return v
}

// okOutcome builds a successful fetch result for a video.
func okOutcome(at time.Time, views uint64) domain.FetchOutcome {
	next := at.Add(6 * time.Hour)
	return domain.FetchOutcome{
		Stats: &domain.VideoStats{
			Platform:     domain.PlatformYouTube,
			VideoID:      "abc",
			CanonicalURL: "https://youtu.be/abc",
			Title:        "A video",
			ChannelID:    "UC123",
			ChannelTitle: "A channel",
			ViewCount:    domain.U64(views),
			LikeCount:    domain.U64(views / 10),
			FetchedAt:    at,
		},
		Status:        domain.FetchOK,
		AttemptStatus: domain.AttemptOK,
		StartedAt:     at,
		Duration:      120 * time.Millisecond,
		NextFetchAt:   &next,
	}
}

// ---------- users ----------

func TestUserCreateAndLookup(t *testing.T) {
	db, ctx := migrated(t)

	created := makeUser(t, db, ctx, "Alice@Example.com")
	if created.ID == 0 || created.PublicID == "" {
		t.Fatalf("created user has no ids: %+v", created)
	}
	if created.Email != "alice@example.com" {
		t.Errorf("email = %q, want it normalised to lower case", created.Email)
	}
	if created.Timezone != "UTC" {
		t.Errorf("timezone = %q, want the UTC default", created.Timezone)
	}
	if created.Status != domain.UserActive || !created.Active() {
		t.Errorf("status = %q, want active", created.Status)
	}

	for _, tc := range []struct {
		name string
		get  func() (*domain.User, error)
	}{
		{"by id", func() (*domain.User, error) { return db.Users().ByID(ctx, created.ID) }},
		{"by public id", func() (*domain.User, error) { return db.Users().ByPublicID(ctx, created.PublicID) }},
		{"by email", func() (*domain.User, error) { return db.Users().ByEmail(ctx, "alice@example.com") }},
		// citext is what makes this work; a plain text column would miss it.
		{"by email, different case", func() (*domain.User, error) { return db.Users().ByEmail(ctx, "ALICE@EXAMPLE.COM") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.get()
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if got.ID != created.ID {
				t.Errorf("got user %d, want %d", got.ID, created.ID)
			}
		})
	}
}

func TestUserEmailIsUniqueRegardlessOfCase(t *testing.T) {
	db, ctx := migrated(t)
	makeUser(t, db, ctx, "alice@example.com")

	_, err := db.Users().Create(ctx, domain.NewUser{
		Email:        "ALICE@example.com",
		PasswordHash: "hash",
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("error = %v, want ErrConflict for a case-different duplicate", err)
	}
}

func TestUserMissingIsRecordNotFound(t *testing.T) {
	db, ctx := migrated(t)

	if _, err := db.Users().ByEmail(ctx, "nobody@example.com"); !errors.Is(err, domain.ErrRecordNotFound) {
		t.Errorf("error = %v, want ErrRecordNotFound", err)
	}
	if _, err := db.Users().ByID(ctx, 999999); !errors.Is(err, domain.ErrRecordNotFound) {
		t.Errorf("error = %v, want ErrRecordNotFound", err)
	}
}

func TestCredentialsAreFetchedSeparatelyFromTheUser(t *testing.T) {
	db, ctx := migrated(t)
	const hash = "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA"

	created, err := db.Users().Create(ctx, domain.NewUser{Email: "bob@example.com", PasswordHash: hash})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	creds, err := db.Users().Credentials(ctx, "bob@example.com")
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if creds.UserID != created.ID || creds.PasswordHash != hash {
		t.Errorf("credentials = %+v, want user %d with the stored hash", creds, created.ID)
	}

	if err := db.Users().UpdatePasswordHash(ctx, created.ID, "newhash"); err != nil {
		t.Fatalf("UpdatePasswordHash: %v", err)
	}
	creds, _ = db.Users().Credentials(ctx, "bob@example.com")
	if creds.PasswordHash != "newhash" {
		t.Errorf("hash = %q after update", creds.PasswordHash)
	}
}

func TestUserProfileAndStatus(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "carol@example.com")

	updated, err := db.Users().UpdateProfile(ctx, u.ID, "Carol Danvers", "Europe/London")
	if err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if updated.DisplayName != "Carol Danvers" || updated.Timezone != "Europe/London" {
		t.Errorf("profile = %+v", updated)
	}

	// An empty timezone must not blank the stored one.
	again, err := db.Users().UpdateProfile(ctx, u.ID, "Carol", "")
	if err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if again.Timezone != "Europe/London" {
		t.Errorf("timezone = %q after an empty update, want it preserved", again.Timezone)
	}

	if err := db.Users().SetStatus(ctx, u.ID, domain.UserSuspended); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	got, _ := db.Users().ByID(ctx, u.ID)
	if got.Active() {
		t.Error("user is still active after being suspended")
	}
}

// ---------- sessions ----------

func hash32(seed byte) []byte {
	h := make([]byte, 32)
	for i := range h {
		h[i] = seed + byte(i)
	}
	return h
}

func TestSessionLifecycle(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "dave@example.com")
	const idle = time.Hour

	token := hash32(1)
	created, err := db.Sessions().Create(ctx, domain.NewSession{
		TokenHash: token,
		UserID:    u.ID,
		ExpiresAt: time.Now().Add(24 * time.Hour),
		UserAgent: "test-agent",
		IP:        "192.0.2.10",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.IP != "192.0.2.10" || created.UserAgent != "test-agent" {
		t.Errorf("session = %+v", created)
	}

	// Lookup returns the user in the same round trip, which is what the auth
	// middleware needs on every request.
	sess, user, err := db.Sessions().Lookup(ctx, token, idle)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if sess.UserID != u.ID || user.Email != "dave@example.com" {
		t.Errorf("lookup = %+v / %+v", sess, user)
	}

	if err := db.Sessions().Delete(ctx, token); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := db.Sessions().Lookup(ctx, token, idle); !errors.Is(err, domain.ErrRecordNotFound) {
		t.Errorf("lookup after delete = %v, want ErrRecordNotFound", err)
	}

	// Deleting an already-gone session is not an error: logging out twice is
	// not a failure.
	if err := db.Sessions().Delete(ctx, token); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

// Expiry is enforced in SQL so that an unusable session comes back as
// "not found" rather than as a valid-looking row every handler must remember
// to check.
func TestSessionLookupEnforcesBothExpiries(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "erin@example.com")

	t.Run("absolute expiry", func(t *testing.T) {
		token := hash32(2)
		_, err := db.Sessions().Create(ctx, domain.NewSession{
			TokenHash: token, UserID: u.ID, ExpiresAt: time.Now().Add(-time.Minute),
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, _, err := db.Sessions().Lookup(ctx, token, time.Hour); !errors.Is(err, domain.ErrRecordNotFound) {
			t.Errorf("expired session resolved: %v", err)
		}
	})

	t.Run("idle timeout", func(t *testing.T) {
		token := hash32(3)
		if _, err := db.Sessions().Create(ctx, domain.NewSession{
			TokenHash: token, UserID: u.ID, ExpiresAt: time.Now().Add(24 * time.Hour),
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Age last_seen_at past the idle window.
		if _, err := db.Pool.Exec(ctx,
			`UPDATE sessions SET last_seen_at = now() - interval '2 hours' WHERE token_hash = $1`, token); err != nil {
			t.Fatalf("age session: %v", err)
		}
		if _, _, err := db.Sessions().Lookup(ctx, token, time.Hour); !errors.Is(err, domain.ErrRecordNotFound) {
			t.Errorf("idle session resolved: %v", err)
		}
	})

	t.Run("suspended account", func(t *testing.T) {
		token := hash32(4)
		if _, err := db.Sessions().Create(ctx, domain.NewSession{
			TokenHash: token, UserID: u.ID, ExpiresAt: time.Now().Add(24 * time.Hour),
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := db.Users().SetStatus(ctx, u.ID, domain.UserSuspended); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
		if _, _, err := db.Sessions().Lookup(ctx, token, time.Hour); !errors.Is(err, domain.ErrRecordNotFound) {
			t.Errorf("suspended user's session resolved: %v", err)
		}
	})
}

// Touch is throttled so that authenticating a request does not write a row
// every time; without the threshold the sessions table becomes the busiest
// object in the database for no benefit.
func TestSessionTouchIsThrottled(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "frank@example.com")

	token := hash32(5)
	if _, err := db.Sessions().Create(ctx, domain.NewSession{
		TokenHash: token, UserID: u.ID, ExpiresAt: time.Now().Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	wrote, err := db.Sessions().Touch(ctx, token, time.Minute)
	if err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if wrote {
		t.Error("Touch wrote for a session seen a moment ago")
	}

	if _, err := db.Pool.Exec(ctx,
		`UPDATE sessions SET last_seen_at = now() - interval '5 minutes' WHERE token_hash = $1`, token); err != nil {
		t.Fatalf("age: %v", err)
	}

	wrote, err = db.Sessions().Touch(ctx, token, time.Minute)
	if err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if !wrote {
		t.Error("Touch did not write for a session past the threshold")
	}
}

func TestSessionReaping(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "grace@example.com")

	live := hash32(6)
	dead := hash32(7)
	if _, err := db.Sessions().Create(ctx, domain.NewSession{
		TokenHash: live, UserID: u.ID, ExpiresAt: time.Now().Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("create live: %v", err)
	}
	if _, err := db.Sessions().Create(ctx, domain.NewSession{
		TokenHash: dead, UserID: u.ID, ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create dead: %v", err)
	}

	n, err := db.Sessions().DeleteExpired(ctx, time.Hour)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if n != 1 {
		t.Errorf("reaped %d sessions, want 1", n)
	}

	count, err := db.Sessions().CountForUser(ctx, u.ID, time.Hour)
	if err != nil {
		t.Fatalf("CountForUser: %v", err)
	}
	if count != 1 {
		t.Errorf("live sessions = %d, want 1", count)
	}

	// "Log out everywhere" — the capability a stateless token would have cost.
	if _, err := db.Sessions().DeleteAllForUser(ctx, u.ID); err != nil {
		t.Fatalf("DeleteAllForUser: %v", err)
	}
	count, _ = db.Sessions().CountForUser(ctx, u.ID, time.Hour)
	if count != 0 {
		t.Errorf("sessions = %d after logging out everywhere", count)
	}
}

func TestDeletingAUserCascadesToSessions(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "heidi@example.com")

	if _, err := db.Sessions().Create(ctx, domain.NewSession{
		TokenHash: hash32(8), UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := db.Users().Delete(ctx, u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d sessions survived the user being deleted", n)
	}
}

// ---------- videos ----------

// The natural key is the deduplication point: two users pasting different URL
// forms of one video must land on one row, or they get two histories of it.
func TestVideoUpsertDeduplicatesOnTheNaturalKey(t *testing.T) {
	db, ctx := migrated(t)

	first, err := db.Videos().Upsert(ctx, domain.NewVideo{
		Platform: domain.PlatformYouTube, PlatformVideoID: "dQw4w9WgXcQ",
		CanonicalURL: "https://youtu.be/dQw4w9WgXcQ",
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	second, err := db.Videos().Upsert(ctx, domain.NewVideo{
		Platform: domain.PlatformYouTube, PlatformVideoID: "dQw4w9WgXcQ",
		CanonicalURL: "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("upsert created two rows (%d and %d) for one video", first.ID, second.ID)
	}
	if second.CanonicalURL != "https://www.youtube.com/watch?v=dQw4w9WgXcQ" {
		t.Errorf("canonical url = %q, want the newer form", second.CanonicalURL)
	}

	// The same id on a different platform is a different video.
	other, err := db.Videos().Upsert(ctx, domain.NewVideo{
		Platform: domain.PlatformTikTok, PlatformVideoID: "dQw4w9WgXcQ",
		CanonicalURL: "https://tiktok.test/x",
	})
	if err != nil {
		t.Fatalf("cross-platform upsert: %v", err)
	}
	if other.ID == first.ID {
		t.Error("the same id on two platforms collapsed into one row")
	}
}

func TestVideoDefaultsAndLookups(t *testing.T) {
	db, ctx := migrated(t)
	v := makeVideo(t, db, ctx, domain.PlatformTikTok, "7249376077976472833")

	if v.Schedule.LastFetchStatus != domain.FetchPending {
		t.Errorf("status = %q, want pending", v.Schedule.LastFetchStatus)
	}
	if v.Schedule.Interval != 6*time.Hour {
		t.Errorf("interval = %v, want the 6h default", v.Schedule.Interval)
	}
	if v.Schedule.NextFetchAt != nil {
		t.Error("an untracked video should not be scheduled")
	}
	if v.Latest.ViewCount != nil {
		t.Error("a never-fetched video should have no view count, not zero")
	}

	byPublic, err := db.Videos().ByPublicID(ctx, v.PublicID)
	if err != nil || byPublic.ID != v.ID {
		t.Fatalf("ByPublicID = %v, %v", byPublic, err)
	}
	byNatural, err := db.Videos().ByPlatformID(ctx, domain.PlatformTikTok, "7249376077976472833")
	if err != nil || byNatural.ID != v.ID {
		t.Fatalf("ByPlatformID = %v, %v", byNatural, err)
	}
}

func TestVideoRejectsAnUnknownPlatform(t *testing.T) {
	db, ctx := migrated(t)

	_, err := db.Videos().Upsert(ctx, domain.NewVideo{
		Platform: "myspace", PlatformVideoID: "1", CanonicalURL: "https://x.test",
	})
	if !errors.Is(err, domain.ErrStorage) {
		t.Errorf("error = %v, want ErrStorage from the platform check constraint", err)
	}
}

// ---------- recording a fetch ----------

func TestRecordWritesSnapshotLatestAndAuditTogether(t *testing.T) {
	db, ctx := migrated(t)
	v := makeVideo(t, db, ctx, domain.PlatformYouTube, "abc")

	at := time.Now().UTC().Truncate(time.Millisecond)
	if err := db.Videos().Record(ctx, v.ID, okOutcome(at, 220500)); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := db.Videos().ByID(ctx, v.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if got.Latest.ViewCount == nil || *got.Latest.ViewCount != 220500 {
		t.Errorf("latest view count = %v, want 220500", got.Latest.ViewCount)
	}
	if got.Title != "A video" || got.ChannelTitle != "A channel" {
		t.Errorf("metadata not applied: %+v", got)
	}
	if got.Schedule.LastFetchStatus != domain.FetchOK {
		t.Errorf("status = %q, want ok", got.Schedule.LastFetchStatus)
	}
	if got.Schedule.LockedUntil != nil {
		t.Error("Record should release the claim it recorded against")
	}

	history, err := db.Metrics().History(ctx, v.ID, at.Add(-time.Hour), at.Add(time.Hour), BucketRaw)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 1 || history[0].ViewCount == nil || *history[0].ViewCount != 220500 {
		t.Fatalf("history = %+v, want one snapshot of 220500", history)
	}

	attempts, err := db.Metrics().Attempts(ctx, v.ID, 10)
	if err != nil {
		t.Fatalf("Attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Status != domain.AttemptOK {
		t.Fatalf("attempts = %+v, want one ok", attempts)
	}
	if attempts[0].Platform != domain.PlatformYouTube {
		t.Errorf("attempt platform = %q, want it taken from the video", attempts[0].Platform)
	}
}

// A failed fetch still records the attempt and the new schedule. "We tried at
// 14:00 and Meta served a login wall" is exactly what the audit trail is for.
func TestRecordOnFailureKeepsTheLastGoodReading(t *testing.T) {
	db, ctx := migrated(t)
	v := makeVideo(t, db, ctx, domain.PlatformMeta, "999")

	at := time.Now().UTC()
	if err := db.Videos().Record(ctx, v.ID, okOutcome(at.Add(-time.Hour), 1000)); err != nil {
		t.Fatalf("seed Record: %v", err)
	}

	next := at.Add(2 * time.Hour)
	err := db.Videos().Record(ctx, v.ID, domain.FetchOutcome{
		Status:              domain.FetchBlocked,
		AttemptStatus:       domain.AttemptBlocked,
		ErrorCode:           "upstream_blocked",
		ErrorDetail:         "facebook served a login wall",
		StartedAt:           at,
		Duration:            3 * time.Second,
		ConsecutiveFailures: 1,
		NextFetchAt:         &next,
	})
	if err != nil {
		t.Fatalf("Record failure: %v", err)
	}

	got, _ := db.Videos().ByID(ctx, v.ID)
	if got.Latest.ViewCount == nil || *got.Latest.ViewCount != 1000 {
		t.Errorf("latest = %v; a failed fetch must not erase the last good reading", got.Latest.ViewCount)
	}
	if got.Schedule.LastFetchStatus != domain.FetchBlocked || got.Schedule.ConsecutiveFailures != 1 {
		t.Errorf("schedule = %+v", got.Schedule)
	}
	if !strings.Contains(got.Schedule.LastFetchError, "login wall") {
		t.Errorf("last error = %q", got.Schedule.LastFetchError)
	}

	// Only the successful fetch produced a snapshot.
	history, _ := db.Metrics().History(ctx, v.ID, at.Add(-24*time.Hour), at.Add(time.Hour), BucketRaw)
	if len(history) != 1 {
		t.Errorf("%d snapshots, want 1: a failed fetch must not write one", len(history))
	}

	// Both attempts were logged.
	attempts, _ := db.Metrics().Attempts(ctx, v.ID, 10)
	if len(attempts) != 2 {
		t.Errorf("%d attempts logged, want 2", len(attempts))
	}
	if attempts[0].Status != domain.AttemptBlocked || attempts[0].ErrorCode != "upstream_blocked" {
		t.Errorf("newest attempt = %+v", attempts[0])
	}
}

// A blocked video must never be retired: a login wall is our problem, not
// evidence the video is gone. Only an explicit UnavailableSince retires one.
func TestRetiringAVideoStopsPollingButKeepsHistory(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "ivan@example.com")
	v := makeVideo(t, db, ctx, domain.PlatformYouTube, "gone")

	if _, err := db.Tracking().Track(ctx, u.ID, v.ID, ""); err != nil {
		t.Fatalf("Track: %v", err)
	}
	at := time.Now().UTC()
	if err := db.Videos().Record(ctx, v.ID, okOutcome(at.Add(-time.Hour), 500)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	retired := at
	err := db.Videos().Record(ctx, v.ID, domain.FetchOutcome{
		Status: domain.FetchNotFound, AttemptStatus: domain.AttemptNotFound,
		ErrorCode: "not_found", StartedAt: at, ConsecutiveFailures: 3,
		NextFetchAt: nil, UnavailableSince: &retired,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, _ := db.Videos().ByID(ctx, v.ID)
	if !got.Schedule.Retired() {
		t.Error("video was not retired")
	}
	if got.Schedule.NextFetchAt != nil {
		t.Error("a retired video is still scheduled")
	}
	if got.Latest.ViewCount == nil || *got.Latest.ViewCount != 500 {
		t.Error("retiring a video discarded its last known figures")
	}

	claimed, err := db.Videos().ClaimDue(ctx, domain.PlatformYouTube, 10, time.Minute)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("claimed %d retired videos, want 0", len(claimed))
	}
}

// ---------- claiming ----------

func TestClaimDueRespectsScheduleAndLocks(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "judy@example.com")

	due := makeVideo(t, db, ctx, domain.PlatformYouTube, "due")
	notYet := makeVideo(t, db, ctx, domain.PlatformYouTube, "later")
	untracked := makeVideo(t, db, ctx, domain.PlatformYouTube, "untracked")
	otherPlatform := makeVideo(t, db, ctx, domain.PlatformTikTok, "tiktok")

	for _, v := range []*domain.Video{due, notYet, otherPlatform} {
		if _, err := db.Tracking().Track(ctx, u.ID, v.ID, ""); err != nil {
			t.Fatalf("Track: %v", err)
		}
	}
	_ = untracked

	if _, err := db.Pool.Exec(ctx,
		`UPDATE videos SET next_fetch_at = now() + interval '1 hour' WHERE id = $1`, notYet.ID); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	claimed, err := db.Videos().ClaimDue(ctx, domain.PlatformYouTube, 10, 5*time.Minute)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != due.ID {
		t.Fatalf("claimed %d videos (%v), want just the due one", len(claimed), claimed)
	}
	if claimed[0].Schedule.LockedUntil == nil {
		t.Error("claimed video is not locked")
	}

	// A locked video is not handed out twice.
	again, err := db.Videos().ClaimDue(ctx, domain.PlatformYouTube, 10, 5*time.Minute)
	if err != nil {
		t.Fatalf("second ClaimDue: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("claimed %d already-locked videos, want 0", len(again))
	}

	// Releasing puts it back without changing when it is next due.
	if err := db.Videos().Release(ctx, due.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	third, _ := db.Videos().ClaimDue(ctx, domain.PlatformYouTube, 10, 5*time.Minute)
	if len(third) != 1 {
		t.Errorf("released video was not reclaimable")
	}
}

// An expired lock is what makes a worker dying mid-fetch recoverable, with no
// liveness tracking of workers anywhere.
func TestClaimDueReclaimsAfterALockExpires(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "ken@example.com")
	v := makeVideo(t, db, ctx, domain.PlatformYouTube, "orphan")
	if _, err := db.Tracking().Track(ctx, u.ID, v.ID, ""); err != nil {
		t.Fatalf("Track: %v", err)
	}

	if _, err := db.Videos().ClaimDue(ctx, domain.PlatformYouTube, 1, time.Hour); err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	// The worker dies: simulate its lock ageing out.
	if _, err := db.Pool.Exec(ctx,
		`UPDATE videos SET locked_until = now() - interval '1 minute' WHERE id = $1`, v.ID); err != nil {
		t.Fatalf("expire lock: %v", err)
	}

	reclaimed, err := db.Videos().ClaimDue(ctx, domain.PlatformYouTube, 1, time.Hour)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(reclaimed) != 1 {
		t.Error("an abandoned video was never reclaimed")
	}
}

// SKIP LOCKED is the whole scaling story: several workers must divide the work
// with no coordination and no video fetched twice.
func TestConcurrentWorkersDivideTheWork(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "workers@example.com")

	const videos = 30
	for i := range videos {
		v := makeVideo(t, db, ctx, domain.PlatformYouTube, fmt.Sprintf("v%02d", i))
		if _, err := db.Tracking().Track(ctx, u.ID, v.ID, ""); err != nil {
			t.Fatalf("Track: %v", err)
		}
	}

	const workers = 6
	results := make(chan []*domain.Video, workers)
	for range workers {
		go func() {
			claimed, err := db.Videos().ClaimDue(ctx, domain.PlatformYouTube, 10, time.Minute)
			if err != nil {
				t.Errorf("ClaimDue: %v", err)
			}
			results <- claimed
		}()
	}

	seen := map[int64]int{}
	total := 0
	for range workers {
		for _, v := range <-results {
			seen[v.ID]++
			total++
		}
	}

	if total != videos {
		t.Errorf("claimed %d videos in total, want %d", total, videos)
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("video %d was claimed by %d workers", id, n)
		}
	}
}

func TestBackoffPlatformMovesTheWholePlatform(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "limit@example.com")

	for i := range 3 {
		v := makeVideo(t, db, ctx, domain.PlatformTikTok, fmt.Sprintf("tt%d", i))
		if _, err := db.Tracking().Track(ctx, u.ID, v.ID, ""); err != nil {
			t.Fatalf("Track: %v", err)
		}
	}
	yt := makeVideo(t, db, ctx, domain.PlatformYouTube, "yt")
	if _, err := db.Tracking().Track(ctx, u.ID, yt.ID, ""); err != nil {
		t.Fatalf("Track: %v", err)
	}

	// A rate limit belongs to the platform, not the video that happened to hit
	// it: backing off one id sends the next tick into the same limit.
	n, err := db.Videos().BackoffPlatform(ctx, domain.PlatformTikTok, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("BackoffPlatform: %v", err)
	}
	if n != 3 {
		t.Errorf("backed off %d videos, want 3", n)
	}

	if claimed, _ := db.Videos().ClaimDue(ctx, domain.PlatformTikTok, 10, time.Minute); len(claimed) != 0 {
		t.Errorf("claimed %d tiktok videos after backing the platform off", len(claimed))
	}
	if claimed, _ := db.Videos().ClaimDue(ctx, domain.PlatformYouTube, 10, time.Minute); len(claimed) != 1 {
		t.Error("backing off tiktok also stopped youtube")
	}
}
