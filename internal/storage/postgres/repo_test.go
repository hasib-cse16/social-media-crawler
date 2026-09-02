package postgres

import (
	"context"
	"errors"
	"fmt"
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

// makeStats builds a provider result to record.
func makeStats(platform domain.Platform, id string, views uint64) *domain.VideoStats {
	return &domain.VideoStats{
		Platform:           platform,
		VideoID:            id,
		CanonicalURL:       "https://example.test/" + id,
		Title:              "A video",
		ChannelID:          "UC123",
		ChannelTitle:       "A channel",
		ChannelURL:         "https://www.youtube.com/@achannel",
		ChannelEmail:       "hello@achannel.test",
		ChannelDescription: "Business: hello@achannel.test",
		ViewCount:          domain.U64(views),
		LikeCount:          domain.U64(views / 10),
		FetchedAt:          time.Now().UTC().Truncate(time.Millisecond),
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

// ---------- lookups ----------

func TestLookupRoundTripsEveryField(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "erin@example.com")

	in := domain.NewLookup(u.ID, makeStats(domain.PlatformYouTube, "abc123", 4200))
	created, err := db.Lookups().Create(ctx, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == 0 || created.PublicID == "" {
		t.Fatalf("created lookup has no ids: %+v", created)
	}

	got, err := db.Lookups().ByPublicID(ctx, u.ID, created.PublicID)
	if err != nil {
		t.Fatalf("ByPublicID: %v", err)
	}
	if got.ChannelEmail != "hello@achannel.test" {
		t.Errorf("channel email = %q, want it persisted", got.ChannelEmail)
	}
	if got.ViewCount == nil || *got.ViewCount != 4200 {
		t.Errorf("view count = %v, want 4200", got.ViewCount)
	}
	// A counter the platform did not report must stay absent rather than
	// becoming zero, which is a different fact.
	if got.ShareCount != nil {
		t.Errorf("share count = %v, want nil for a platform that does not report it", *got.ShareCount)
	}
}

func TestLookupsAreScopedToTheirUser(t *testing.T) {
	db, ctx := migrated(t)
	mine := makeUser(t, db, ctx, "mine@example.com")
	theirs := makeUser(t, db, ctx, "theirs@example.com")

	created, err := db.Lookups().Create(ctx, domain.NewLookup(mine.ID, makeStats(domain.PlatformTikTok, "vid1", 10)))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Another user's public id must be indistinguishable from one that does not
	// exist, or the endpoint becomes a way to probe for other people's rows.
	if _, err := db.Lookups().ByPublicID(ctx, theirs.ID, created.PublicID); !errors.Is(err, domain.ErrRecordNotFound) {
		t.Errorf("cross-user read = %v, want ErrRecordNotFound", err)
	}
	if err := db.Lookups().Delete(ctx, theirs.ID, created.PublicID); !errors.Is(err, domain.ErrRecordNotFound) {
		t.Errorf("cross-user delete = %v, want ErrRecordNotFound", err)
	}

	if _, err := db.Lookups().ByPublicID(ctx, mine.ID, created.PublicID); err != nil {
		t.Errorf("owner read after a failed cross-user delete: %v", err)
	}
}

func TestLookingUpTheSameURLAppendsRatherThanReplaces(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "frank@example.com")

	first := makeStats(domain.PlatformYouTube, "same", 100)
	if _, err := db.Lookups().Create(ctx, domain.NewLookup(u.ID, first)); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	second := makeStats(domain.PlatformYouTube, "same", 250)
	second.FetchedAt = first.FetchedAt.Add(time.Hour)
	if _, err := db.Lookups().Create(ctx, domain.NewLookup(u.ID, second)); err != nil {
		t.Fatalf("second Create: %v", err)
	}

	recent, err := db.Lookups().Recent(ctx, u.ID, 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("got %d rows, want 2 — a re-check is history, not an update", len(recent))
	}
	// Newest first, so the dashboard's top row is the latest answer.
	if *recent[0].ViewCount != 250 {
		t.Errorf("first row has %d views, want the newest reading (250)", *recent[0].ViewCount)
	}
}

func TestRecentHonoursItsLimit(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "grace@example.com")

	base := time.Now().UTC()
	for i := range 5 {
		s := makeStats(domain.PlatformMeta, fmt.Sprintf("v%d", i), uint64(i))
		s.FetchedAt = base.Add(time.Duration(i) * time.Minute)
		if _, err := db.Lookups().Create(ctx, domain.NewLookup(u.ID, s)); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	recent, err := db.Lookups().Recent(ctx, u.ID, 3)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recent) != 3 {
		t.Fatalf("got %d rows, want 3", len(recent))
	}
	if recent[0].VideoID != "v4" {
		t.Errorf("newest row = %q, want v4", recent[0].VideoID)
	}
}

func TestDeletingAUserCascadesToLookups(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "heidi@example.com")

	if _, err := db.Lookups().Create(ctx, domain.NewLookup(u.ID, makeStats(domain.PlatformYouTube, "x", 1))); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := db.Users().Delete(ctx, u.ID); err != nil {
		t.Fatalf("Delete user: %v", err)
	}

	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM lookups WHERE user_id = $1`, u.ID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("%d lookups survived the account being deleted", count)
	}
}

func TestLookupRejectsAnUnknownPlatform(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "ivan@example.com")

	in := domain.NewLookup(u.ID, makeStats("myspace", "x", 1))
	if _, err := db.Lookups().Create(ctx, in); err == nil {
		t.Error("an unknown platform was accepted; the CHECK constraint is not doing its job")
	}
}
