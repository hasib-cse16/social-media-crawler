package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/foodibd/socialstats/internal/config"
	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/storage/postgres"
	"github.com/foodibd/socialstats/internal/storage/postgres/pgtest"
)

// These run against a real PostgreSQL: the rate limiter is a SQL function whose
// whole job is to be atomic under concurrency, and session expiry is enforced
// in SQL. A mock would only assert our own assumptions back at us.

func testService(t *testing.T, mutate func(*Config)) (*Service, *postgres.DB, context.Context) {
	t.Helper()

	ctx := context.Background()
	db, err := postgres.Connect(ctx, config.DatabaseConfig{
		URL: pgtest.URL(t), MaxConns: 8, MinConns: 1,
		MaxConnLifetime: time.Minute, ConnectTimeout: 5 * time.Second,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := Config{
		TTL:              24 * time.Hour,
		IdleTTL:          time.Hour,
		TouchInterval:    time.Minute,
		RegistrationOpen: true,
		LoginAttempts:    5,
		LoginWindow:      15 * time.Minute,
		Cookie:           CookieConfig{Name: "ss_session", Secure: false, TTL: 24 * time.Hour},
	}
	if mutate != nil {
		mutate(&cfg)
	}

	svc, err := NewService(db.Users(), db.Sessions(), db.RateLimits(), testHasher(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, db, ctx
}

const goodPassword = "seven blue mountains rise"

func register(t *testing.T, svc *Service, ctx context.Context, email string) *Issued {
	t.Helper()
	issued, err := svc.Register(ctx, RegisterInput{
		Email: email, Password: goodPassword, DisplayName: "Test", IP: "192.0.2.1",
	})
	if err != nil {
		t.Fatalf("Register(%s): %v", email, err)
	}
	return issued
}

func TestRegisterThenAuthenticate(t *testing.T) {
	svc, _, ctx := testService(t, nil)

	issued := register(t, svc, ctx, "Alice@Example.com")
	if issued.User.Email != "alice@example.com" {
		t.Errorf("email = %q, want it normalised", issued.User.Email)
	}
	if issued.Token == "" || issued.CSRFToken == "" {
		t.Fatal("register did not issue a session")
	}
	if !issued.ExpiresAt.After(time.Now()) {
		t.Errorf("expires at %v, already in the past", issued.ExpiresAt)
	}

	user, err := svc.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if user.ID != issued.User.ID {
		t.Errorf("authenticated as %d, want %d", user.ID, issued.User.ID)
	}
}

func TestRegisterRejects(t *testing.T) {
	svc, _, ctx := testService(t, nil)
	register(t, svc, ctx, "taken@example.com")

	tests := []struct {
		name  string
		input RegisterInput
		want  error
	}{
		{"duplicate email", RegisterInput{Email: "taken@example.com", Password: goodPassword}, domain.ErrConflict},
		{"duplicate, different case", RegisterInput{Email: "TAKEN@example.com", Password: goodPassword}, domain.ErrConflict},
		{"weak password", RegisterInput{Email: "new@example.com", Password: "short"}, domain.ErrWeakPassword},
		{"not an email", RegisterInput{Email: "not-an-email", Password: goodPassword}, domain.ErrInvalidURL},
		{"no domain dot", RegisterInput{Email: "a@localhost", Password: goodPassword}, domain.ErrInvalidURL},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Register(ctx, tc.input); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRegistrationCanBeClosed(t *testing.T) {
	svc, _, ctx := testService(t, func(c *Config) { c.RegistrationOpen = false })

	_, err := svc.Register(ctx, RegisterInput{Email: "nope@example.com", Password: goodPassword})
	if !errors.Is(err, domain.ErrRegistrationClosed) {
		t.Errorf("error = %v, want ErrRegistrationClosed", err)
	}
}

func TestLogin(t *testing.T) {
	svc, _, ctx := testService(t, nil)
	registered := register(t, svc, ctx, "bob@example.com")

	issued, err := svc.Login(ctx, LoginInput{Email: "bob@example.com", Password: goodPassword, IP: "192.0.2.2"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if issued.User.ID != registered.User.ID {
		t.Errorf("logged in as %d, want %d", issued.User.ID, registered.User.ID)
	}
	// A fresh session, not the registration one.
	if issued.Token == registered.Token {
		t.Error("login reused the registration token")
	}
	// Both remain valid: signing in on a second device must not sign out the first.
	for name, token := range map[string]Token{"registration": registered.Token, "login": issued.Token} {
		if _, err := svc.Authenticate(ctx, token); err != nil {
			t.Errorf("%s session is not valid: %v", name, err)
		}
	}

	if issued.User.LastLoginAt == nil {
		t.Error("last login was not recorded")
	}
}

// Both halves of a failed sign-in return the same error, so the endpoint cannot
// be used to find out which addresses have accounts.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	svc, _, ctx := testService(t, nil)
	register(t, svc, ctx, "carol@example.com")

	tests := []struct {
		name  string
		email string
		pass  string
	}{
		{"wrong password", "carol@example.com", "wrong password entirely"},
		{"no such account", "nobody@example.com", goodPassword},
		{"neither", "nobody@example.com", "also wrong"},
	}

	var messages []string
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Login(ctx, LoginInput{Email: tc.email, Password: tc.pass})
			if !errors.Is(err, domain.ErrInvalidCredentials) {
				t.Fatalf("error = %v, want ErrInvalidCredentials", err)
			}
			messages = append(messages, err.Error())
		})
	}
	for i := 1; i < len(messages); i++ {
		if messages[i] != messages[0] {
			t.Errorf("error messages differ between cases: %q vs %q", messages[0], messages[i])
		}
	}
}

// A suspended account must fail exactly like a wrong password, or the endpoint
// becomes a way to enumerate suspended users.
func TestSuspendedAccountsCannotSignInOrStaySignedIn(t *testing.T) {
	svc, db, ctx := testService(t, nil)
	issued := register(t, svc, ctx, "dave@example.com")

	if err := db.Users().SetStatus(ctx, issued.User.ID, domain.UserSuspended); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	_, err := svc.Login(ctx, LoginInput{Email: "dave@example.com", Password: goodPassword})
	if !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Errorf("login error = %v, want ErrInvalidCredentials", err)
	}

	// The session they already had stops working too.
	if _, err := svc.Authenticate(ctx, issued.Token); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("existing session error = %v, want ErrUnauthenticated", err)
	}
}

// The plaintext exists only during a successful login, so that is the only
// moment an out-of-date hash can be upgraded.
func TestLoginUpgradesAnOutdatedHash(t *testing.T) {
	svc, db, ctx := testService(t, nil)
	issued := register(t, svc, ctx, "erin@example.com")

	// Replace the stored hash with one made under weaker parameters.
	weak := NewHasher(HashParams{Time: 1, Memory: 8 * 1024, Threads: 1, SaltLength: 16, KeyLength: 16}, 1)
	weakHash, err := weak.Hash(goodPassword)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := db.Users().UpdatePasswordHash(ctx, issued.User.ID, weakHash); err != nil {
		t.Fatalf("UpdatePasswordHash: %v", err)
	}

	if _, err := svc.Login(ctx, LoginInput{Email: "erin@example.com", Password: goodPassword}); err != nil {
		t.Fatalf("Login: %v", err)
	}

	creds, err := db.Users().Credentials(ctx, "erin@example.com")
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if creds.PasswordHash == weakHash {
		t.Error("the outdated hash was not upgraded on a successful login")
	}
	if svc.hasher.NeedsRehash(creds.PasswordHash) {
		t.Error("the upgraded hash still needs a rehash")
	}
	// And the password still works afterwards.
	if _, err := svc.Login(ctx, LoginInput{Email: "erin@example.com", Password: goodPassword}); err != nil {
		t.Errorf("login after the upgrade failed: %v", err)
	}
}

func TestLogoutRevokesOnlyThatSession(t *testing.T) {
	svc, _, ctx := testService(t, nil)
	first := register(t, svc, ctx, "frank@example.com")

	second, err := svc.Login(ctx, LoginInput{Email: "frank@example.com", Password: goodPassword})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := svc.Logout(ctx, first.Token); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := svc.Authenticate(ctx, first.Token); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("the logged-out session still works: %v", err)
	}
	if _, err := svc.Authenticate(ctx, second.Token); err != nil {
		t.Errorf("logging out one session killed another: %v", err)
	}

	// Signing out twice is not a failure.
	if err := svc.Logout(ctx, first.Token); err != nil {
		t.Errorf("second Logout: %v", err)
	}
}

// The capability a stateless signed token would have cost, and the one you want
// on the day a laptop goes missing.
func TestLogoutEverywhere(t *testing.T) {
	svc, _, ctx := testService(t, nil)
	first := register(t, svc, ctx, "grace@example.com")
	second, _ := svc.Login(ctx, LoginInput{Email: "grace@example.com", Password: goodPassword})

	revoked, err := svc.LogoutEverywhere(ctx, first.User.ID)
	if err != nil {
		t.Fatalf("LogoutEverywhere: %v", err)
	}
	if revoked != 2 {
		t.Errorf("revoked %d sessions, want 2", revoked)
	}
	for name, token := range map[string]Token{"first": first.Token, "second": second.Token} {
		if _, err := svc.Authenticate(ctx, token); !errors.Is(err, domain.ErrUnauthenticated) {
			t.Errorf("%s session survived: %v", name, err)
		}
	}
}

// Changing a password revokes every session, because the usual reason to change
// one is believing somebody else has it.
func TestChangePasswordRevokesEverySession(t *testing.T) {
	svc, _, ctx := testService(t, nil)
	issued := register(t, svc, ctx, "heidi@example.com")
	const newPassword = "a completely different phrase"

	if err := svc.ChangePassword(ctx, issued.User, "the wrong current password", newPassword); !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Errorf("error = %v, want ErrInvalidCredentials when the current password is wrong", err)
	}
	if err := svc.ChangePassword(ctx, issued.User, goodPassword, "short"); !errors.Is(err, domain.ErrWeakPassword) {
		t.Errorf("error = %v, want ErrWeakPassword", err)
	}

	if err := svc.ChangePassword(ctx, issued.User, goodPassword, newPassword); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if _, err := svc.Authenticate(ctx, issued.Token); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Error("the session survived a password change")
	}
	if _, err := svc.Login(ctx, LoginInput{Email: "heidi@example.com", Password: goodPassword}); !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Error("the old password still works")
	}
	if _, err := svc.Login(ctx, LoginInput{Email: "heidi@example.com", Password: newPassword}); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
}

func TestSessionExpiry(t *testing.T) {
	t.Run("absolute", func(t *testing.T) {
		svc, _, ctx := testService(t, func(c *Config) { c.TTL = -time.Minute })
		issued, err := svc.Register(ctx, RegisterInput{Email: "expired@example.com", Password: goodPassword})
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		if _, err := svc.Authenticate(ctx, issued.Token); !errors.Is(err, domain.ErrUnauthenticated) {
			t.Errorf("an already-expired session authenticated: %v", err)
		}
	})

	t.Run("idle", func(t *testing.T) {
		svc, db, ctx := testService(t, nil)
		issued := register(t, svc, ctx, "idle@example.com")

		if _, err := db.Pool.Exec(ctx,
			`UPDATE sessions SET last_seen_at = now() - interval '2 hours'`); err != nil {
			t.Fatalf("age session: %v", err)
		}
		if _, err := svc.Authenticate(ctx, issued.Token); !errors.Is(err, domain.ErrUnauthenticated) {
			t.Errorf("an idle session authenticated: %v", err)
		}
	})
}

func TestAuthenticateRejectsJunkWithoutAQuery(t *testing.T) {
	svc, _, ctx := testService(t, nil)

	for _, token := range []Token{"", "nope", "../../etc/passwd", Token(make([]byte, 10))} {
		if _, err := svc.Authenticate(ctx, token); !errors.Is(err, domain.ErrUnauthenticated) {
			t.Errorf("Authenticate(%q) = %v, want ErrUnauthenticated", token, err)
		}
	}
}

// ---------- rate limiting ----------

func TestLoginRateLimitByEmail(t *testing.T) {
	svc, _, ctx := testService(t, func(c *Config) { c.LoginAttempts = 3 })
	register(t, svc, ctx, "target@example.com")

	// Vary the source address so only the per-email limit can fire.
	for i := range 3 {
		_, err := svc.Login(ctx, LoginInput{
			Email: "target@example.com", Password: "wrong", IP: ipForAttempt(i),
		})
		if !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: error = %v, want ErrInvalidCredentials", i, err)
		}
	}

	_, err := svc.Login(ctx, LoginInput{Email: "target@example.com", Password: "wrong", IP: ipForAttempt(9)})
	if !errors.Is(err, domain.ErrTooManyAttempts) {
		t.Fatalf("error = %v, want ErrTooManyAttempts once the bucket is empty", err)
	}

	var tooMany *TooManyAttemptsError
	if !errors.As(err, &tooMany) {
		t.Fatal("the error does not carry a retry hint")
	}
	if tooMany.RetryAfter <= 0 {
		t.Errorf("retry after = %v, want a positive wait", tooMany.RetryAfter)
	}

	// The limit is per subject: a different account is unaffected.
	register(t, svc, ctx, "bystander@example.com")
	if _, err := svc.Login(ctx, LoginInput{
		Email: "bystander@example.com", Password: goodPassword, IP: ipForAttempt(9),
	}); err != nil {
		t.Errorf("an unrelated account was locked out: %v", err)
	}
}

// Per-address alone would let one host work through a list of accounts
// unimpeded, so the source is limited independently.
func TestLoginRateLimitByIP(t *testing.T) {
	svc, _, ctx := testService(t, func(c *Config) { c.LoginAttempts = 3 })

	const attacker = "198.51.100.7"
	for i := range 3 {
		_, err := svc.Login(ctx, LoginInput{
			Email: emailForAttempt(i), Password: "wrong", IP: attacker,
		})
		if !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}

	// A fourth address from the same host is refused even though that address
	// has never been tried.
	_, err := svc.Login(ctx, LoginInput{Email: emailForAttempt(99), Password: "wrong", IP: attacker})
	if !errors.Is(err, domain.ErrTooManyAttempts) {
		t.Errorf("error = %v, want the per-IP limit to fire", err)
	}

	// Another source is unaffected.
	if _, err := svc.Login(ctx, LoginInput{
		Email: emailForAttempt(99), Password: "wrong", IP: "203.0.113.5",
	}); !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Errorf("a different source was blocked: %v", err)
	}
}

// A few typos must not keep counting against someone who then gets it right.
func TestSuccessfulLoginClearsTheLimit(t *testing.T) {
	svc, _, ctx := testService(t, func(c *Config) { c.LoginAttempts = 3 })
	register(t, svc, ctx, "typo@example.com")

	const from = "192.0.2.50"
	for range 2 {
		if _, err := svc.Login(ctx, LoginInput{Email: "typo@example.com", Password: "wrong", IP: from}); err == nil {
			t.Fatal("a wrong password succeeded")
		}
	}
	if _, err := svc.Login(ctx, LoginInput{Email: "typo@example.com", Password: goodPassword, IP: from}); err != nil {
		t.Fatalf("Login: %v", err)
	}

	// The bucket is reset, so a full allowance is available again.
	for i := range 3 {
		_, err := svc.Login(ctx, LoginInput{Email: "typo@example.com", Password: "wrong", IP: from})
		if !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Fatalf("attempt %d after a success: error = %v, want the counter to have reset", i, err)
		}
	}
}

// The bucket is spent inside one SQL statement so that concurrent attempts
// cannot both read "one token left" and both spend it — the race is exactly the
// burst an attacker produces.
func TestRateLimitHoldsUnderConcurrency(t *testing.T) {
	svc, db, ctx := testService(t, nil)
	register(t, svc, ctx, "burst@example.com")

	const capacity = 5
	const attempts = 40

	allowed := make(chan bool, attempts)
	for range attempts {
		go func() {
			d, err := db.RateLimits().Take(ctx, "test_burst", "one-subject", capacity, 15*time.Minute)
			if err != nil {
				t.Errorf("Take: %v", err)
				allowed <- false
				return
			}
			allowed <- d.Allowed
		}()
	}

	granted := 0
	for range attempts {
		if <-allowed {
			granted++
		}
	}
	if granted != capacity {
		t.Errorf("%d of %d concurrent attempts were allowed, want exactly %d", granted, attempts, capacity)
	}
}

// A rate limiter that locks everyone out when the database hiccups has turned a
// degraded dependency into an outage.
func TestRateLimiterFailsOpen(t *testing.T) {
	svc, _, ctx := testService(t, nil)
	register(t, svc, ctx, "failopen@example.com")

	svc.limiter = brokenLimiter{}

	if _, err := svc.Login(ctx, LoginInput{
		Email: "failopen@example.com", Password: goodPassword, IP: "192.0.2.99",
	}); err != nil {
		t.Errorf("login failed while the limiter was down: %v", err)
	}
}

type brokenLimiter struct{}

func (brokenLimiter) Take(context.Context, string, string, int, time.Duration) (postgres.Decision, error) {
	return postgres.Decision{}, errors.New("database is on fire")
}
func (brokenLimiter) Reset(context.Context, string, string) error {
	return errors.New("database is on fire")
}

func ipForAttempt(i int) string { return "203.0.113." + string(rune('1'+i)) }
func emailForAttempt(i int) string {
	return "user" + string(rune('a'+i%26)) + "@example.com"
}

// ---------- client address ----------

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		forwarded  string
		trustProxy bool
		want       string
	}{
		{"direct", "192.0.2.10:54321", "", false, "192.0.2.10"},
		{"ipv6", "[2001:db8::1]:443", "", false, "2001:db8::1"},
		{"no port", "192.0.2.10", "", false, "192.0.2.10"},
		// Without a trusted proxy the header is ignored, because any caller can
		// set it and thereby choose which bucket to spend.
		{"spoofed header, untrusted", "192.0.2.10:1", "198.51.100.1", false, "192.0.2.10"},
		{"header, trusted", "10.0.0.1:1", "198.51.100.1", true, "198.51.100.1"},
		{"chain, trusted", "10.0.0.1:1", "198.51.100.1, 10.0.0.5", true, "198.51.100.1"},
		{"garbage header, trusted", "192.0.2.10:1", "not-an-ip", true, "192.0.2.10"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.forwarded != "" {
				r.Header.Set("X-Forwarded-For", tc.forwarded)
			}
			if got := ClientIP(r, tc.trustProxy); got != tc.want {
				t.Errorf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
