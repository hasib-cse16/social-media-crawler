package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/storage/postgres"
)

// The stores this package needs, declared here rather than exported from
// storage: the consumer defines the interface, so the Postgres repositories
// satisfy it implicitly and the import arrow keeps pointing inward.

// UserStore is the account persistence auth requires.
type UserStore interface {
	Create(ctx context.Context, in domain.NewUser) (*domain.User, error)
	ByID(ctx context.Context, id int64) (*domain.User, error)
	Credentials(ctx context.Context, email string) (*domain.Credentials, error)
	UpdatePasswordHash(ctx context.Context, userID int64, hash string) error
	TouchLastLogin(ctx context.Context, userID int64, at time.Time) error
}

// SessionStore is the session persistence auth requires.
type SessionStore interface {
	Create(ctx context.Context, in domain.NewSession) (*domain.Session, error)
	Lookup(ctx context.Context, tokenHash []byte, idleTTL time.Duration) (*domain.Session, *domain.User, error)
	Touch(ctx context.Context, tokenHash []byte, minInterval time.Duration) (bool, error)
	Delete(ctx context.Context, tokenHash []byte) error
	DeleteAllForUser(ctx context.Context, userID int64) (int64, error)
	DeleteExpired(ctx context.Context, idleTTL time.Duration) (int64, error)
}

// Limiter is the rate limiter auth requires.
type Limiter interface {
	Take(ctx context.Context, scope, subject string, capacity int, window time.Duration) (postgres.Decision, error)
	Reset(ctx context.Context, scope, subject string) error
}

// Config tunes session lifetimes and registration.
type Config struct {
	// TTL is the absolute session lifetime: a session dies at this age however
	// active it has been.
	TTL time.Duration

	// IdleTTL expires a session that has gone quiet, so a forgotten sign-in on
	// a shared machine does not stay usable for the full TTL.
	IdleTTL time.Duration

	// TouchInterval is how stale last_seen_at must get before activity writes
	// it again. Without it, every authenticated request writes a row.
	TouchInterval time.Duration

	// RegistrationOpen allows self-service sign-up.
	RegistrationOpen bool

	// LoginAttempts and LoginWindow are the per-email and per-IP burst
	// allowance for failed sign-ins.
	LoginAttempts int
	LoginWindow   time.Duration

	Cookie CookieConfig
}

// Service is the authentication application layer.
type Service struct {
	users    UserStore
	sessions SessionStore
	limiter  Limiter
	hasher   *Hasher
	cfg      Config
	log      *slog.Logger

	// now is injectable so expiry can be tested without sleeping.
	now func() time.Time
}

// NewService builds the auth service and pays the one-off cost of the decoy
// hash used to keep unknown-account logins from being faster than real ones.
func NewService(users UserStore, sessions SessionStore, limiter Limiter, hasher *Hasher, cfg Config, log *slog.Logger) (*Service, error) {
	if err := hasher.InitDummyHash(); err != nil {
		return nil, fmt.Errorf("initialise password hasher: %w", err)
	}
	return &Service{
		users:    users,
		sessions: sessions,
		limiter:  limiter,
		hasher:   hasher,
		cfg:      cfg,
		log:      log.With("component", "auth"),
		now:      time.Now,
	}, nil
}

// Cookies exposes the cookie configuration for handlers that need to set them.
func (s *Service) Cookies() CookieConfig { return s.cfg.Cookie }

// Issued is a newly created session, returned to whatever needs to hand the
// token to the client.
type Issued struct {
	Token     Token
	CSRFToken string
	ExpiresAt time.Time
	User      *domain.User
}

// RegisterInput is a sign-up request.
type RegisterInput struct {
	Email       string
	Password    string
	DisplayName string
	Timezone    string
	UserAgent   string
	IP          string
}

// Register creates an account and signs it in.
//
// A duplicate email surfaces as domain.ErrConflict, which does disclose that an
// address already has an account. Avoiding that requires accepting the
// registration silently and sending mail to the address instead, and there is
// no mail delivery in this service — so the disclosure is stated here rather
// than papered over with a message that says nothing while the status code says
// everything.
func (s *Service) Register(ctx context.Context, in RegisterInput) (*Issued, error) {
	if !s.cfg.RegistrationOpen {
		return nil, domain.ErrRegistrationClosed
	}

	email := strings.ToLower(strings.TrimSpace(in.Email))
	if !plausibleEmail(email) {
		return nil, fmt.Errorf("%w: that does not look like an email address", domain.ErrInvalidURL)
	}
	if err := ValidatePassword(in.Password, email); err != nil {
		return nil, err
	}

	hash, err := s.hasher.Hash(in.Password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	user, err := s.users.Create(ctx, domain.NewUser{
		Email:        email,
		PasswordHash: hash,
		DisplayName:  in.DisplayName,
		Timezone:     in.Timezone,
	})
	if err != nil {
		return nil, err
	}

	s.log.InfoContext(ctx, "account registered", "user_id", user.ID)
	return s.issue(ctx, user, in.UserAgent, in.IP)
}

// LoginInput is a sign-in request.
type LoginInput struct {
	Email     string
	Password  string
	UserAgent string
	IP        string
}

// Login verifies a password and issues a session.
//
// Every failure returns the same domain.ErrInvalidCredentials, and every path
// through it does the same amount of work, so the endpoint reveals neither
// which addresses have accounts nor which half of the pair was wrong.
func (s *Service) Login(ctx context.Context, in LoginInput) (*Issued, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))

	// Limited by address and by source independently. Per-address alone lets
	// one host work through a list of accounts unimpeded; per-IP alone lets a
	// botnet grind a single account. Neither is sufficient on its own.
	if err := s.checkLoginLimit(ctx, "login_email", email); err != nil {
		return nil, err
	}
	if ip := normaliseIP(in.IP); ip != "" {
		if err := s.checkLoginLimit(ctx, "login_ip", ip); err != nil {
			return nil, err
		}
	}

	creds, err := s.users.Credentials(ctx, email)
	if err != nil {
		if errors.Is(err, domain.ErrRecordNotFound) {
			// Do the work anyway. Returning here without hashing makes an
			// unknown address answer in microseconds while a real one takes
			// the full argon2 cost, and that timing difference is an
			// account-existence oracle no amount of identical wording hides.
			s.hasher.SpendTimeOnAMissingAccount(in.Password)
			return nil, domain.ErrInvalidCredentials
		}
		return nil, err
	}

	ok, err := s.hasher.Verify(in.Password, creds.PasswordHash)
	if err != nil {
		// A stored hash we cannot parse is a corrupted row, not a wrong
		// password. Saying so in the log is the only way anyone finds out.
		s.log.ErrorContext(ctx, "stored password hash is unreadable", "user_id", creds.UserID, "error", err)
		return nil, domain.ErrInvalidCredentials
	}
	if !ok {
		return nil, domain.ErrInvalidCredentials
	}
	if creds.Status != domain.UserActive {
		// Suspended accounts fail identically to a wrong password, so the
		// endpoint does not become a way to enumerate suspended users.
		s.log.WarnContext(ctx, "sign-in attempt on a suspended account", "user_id", creds.UserID)
		return nil, domain.ErrInvalidCredentials
	}

	user, err := s.users.ByID(ctx, creds.UserID)
	if err != nil {
		return nil, err
	}

	// This is the only moment the plaintext exists, so it is the only moment an
	// out-of-date hash can be upgraded.
	if s.hasher.NeedsRehash(creds.PasswordHash) {
		if newHash, err := s.hasher.Hash(in.Password); err == nil {
			if err := s.users.UpdatePasswordHash(ctx, user.ID, newHash); err != nil {
				// Not fatal: the sign-in succeeded and the old hash still works.
				s.log.WarnContext(ctx, "could not upgrade password hash", "user_id", user.ID, "error", err)
			} else {
				s.log.InfoContext(ctx, "password hash upgraded to current parameters", "user_id", user.ID)
			}
		}
	}

	// Getting it right clears the counter, so a few typos do not keep counting
	// against someone who then signed in successfully.
	s.resetLoginLimit(ctx, "login_email", email)
	if ip := normaliseIP(in.IP); ip != "" {
		s.resetLoginLimit(ctx, "login_ip", ip)
	}

	// The user was read before this write, so the in-memory copy is updated to
	// match rather than being returned a version behind the row it came from.
	loginAt := s.now()
	if err := s.users.TouchLastLogin(ctx, user.ID, loginAt); err != nil {
		s.log.WarnContext(ctx, "could not record last login", "user_id", user.ID, "error", err)
	} else {
		user.LastLoginAt = &loginAt
	}

	return s.issue(ctx, user, in.UserAgent, in.IP)
}

// issue mints a session for an authenticated user.
func (s *Service) issue(ctx context.Context, user *domain.User, userAgent, ip string) (*Issued, error) {
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	csrf, err := newCSRFToken()
	if err != nil {
		return nil, err
	}

	expiresAt := s.now().Add(s.cfg.TTL)
	if _, err := s.sessions.Create(ctx, domain.NewSession{
		TokenHash: TokenHash(token),
		UserID:    user.ID,
		ExpiresAt: expiresAt,
		UserAgent: truncate(userAgent, 512),
		IP:        normaliseIP(ip),
	}); err != nil {
		return nil, err
	}

	return &Issued{Token: token, CSRFToken: csrf, ExpiresAt: expiresAt, User: user}, nil
}

// Authenticate resolves a token to its user, and refreshes the session's last
// activity when it has gone stale enough to be worth a write.
func (s *Service) Authenticate(ctx context.Context, token Token) (*domain.User, error) {
	if !plausibleToken(token) {
		// Rejected before it becomes a database lookup. A scanner spraying
		// junk cookies should cost us nothing.
		return nil, domain.ErrUnauthenticated
	}

	hash := TokenHash(token)
	_, user, err := s.sessions.Lookup(ctx, hash, s.cfg.IdleTTL)
	if err != nil {
		if errors.Is(err, domain.ErrRecordNotFound) {
			// Expired, idle, revoked, suspended or never existed — the store
			// applies all of those rules, and none of them is worth
			// distinguishing to the caller.
			return nil, domain.ErrUnauthenticated
		}
		return nil, err
	}

	if _, err := s.sessions.Touch(ctx, hash, s.cfg.TouchInterval); err != nil {
		// A failed touch shortens the idle window slightly; it is not a reason
		// to reject a request that is otherwise authenticated.
		s.log.WarnContext(ctx, "could not refresh session activity", "user_id", user.ID, "error", err)
	}
	return user, nil
}

// Logout revokes one session.
func (s *Service) Logout(ctx context.Context, token Token) error {
	if !plausibleToken(token) {
		return nil
	}
	return s.sessions.Delete(ctx, TokenHash(token))
}

// LogoutEverywhere revokes every session an account has.
//
// Server-side sessions give this for free. It is the capability a stateless
// signed token would have cost, and it is the one you want on the day a laptop
// goes missing.
func (s *Service) LogoutEverywhere(ctx context.Context, userID int64) (int64, error) {
	return s.sessions.DeleteAllForUser(ctx, userID)
}

// ChangePassword verifies the current password and replaces it.
//
// Every other session is revoked, because the usual reason for changing a
// password is believing someone else has it, and leaving their sessions signed
// in would make the change ceremonial.
func (s *Service) ChangePassword(ctx context.Context, user *domain.User, current, next string) error {
	creds, err := s.users.Credentials(ctx, user.Email)
	if err != nil {
		return err
	}

	ok, err := s.hasher.Verify(current, creds.PasswordHash)
	if err != nil || !ok {
		return domain.ErrInvalidCredentials
	}
	if err := ValidatePassword(next, user.Email); err != nil {
		return err
	}

	hash, err := s.hasher.Hash(next)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	if err := s.users.UpdatePasswordHash(ctx, user.ID, hash); err != nil {
		return err
	}

	revoked, err := s.sessions.DeleteAllForUser(ctx, user.ID)
	if err != nil {
		s.log.WarnContext(ctx, "could not revoke sessions after a password change", "user_id", user.ID, "error", err)
	}
	s.log.InfoContext(ctx, "password changed", "user_id", user.ID, "sessions_revoked", revoked)
	return nil
}

// ReapSessions deletes sessions past either expiry bound.
func (s *Service) ReapSessions(ctx context.Context) (int64, error) {
	return s.sessions.DeleteExpired(ctx, s.cfg.IdleTTL)
}

// checkLoginLimit spends a token, translating exhaustion into a domain error
// that carries how long to wait.
func (s *Service) checkLoginLimit(ctx context.Context, scope, subject string) error {
	if subject == "" {
		return nil
	}

	decision, err := s.limiter.Take(ctx, scope, subject, s.cfg.LoginAttempts, s.cfg.LoginWindow)
	if err != nil {
		// Fail open. A rate limiter that locks everyone out when the database
		// hiccups has turned a availability problem into an outage, and the
		// login path already has to be up for the database to matter.
		s.log.ErrorContext(ctx, "rate limiter unavailable, allowing the attempt", "scope", scope, "error", err)
		return nil
	}
	if !decision.Allowed {
		s.log.WarnContext(ctx, "login rate limit hit", "scope", scope, "retry_after", decision.RetryAfter)
		return &TooManyAttemptsError{RetryAfter: decision.RetryAfter}
	}
	return nil
}

func (s *Service) resetLoginLimit(ctx context.Context, scope, subject string) {
	if err := s.limiter.Reset(ctx, scope, subject); err != nil {
		s.log.WarnContext(ctx, "could not clear a rate limit bucket", "scope", scope, "error", err)
	}
}

// TooManyAttemptsError carries how long the caller should wait, so the handler
// can send a truthful Retry-After instead of a guess.
type TooManyAttemptsError struct {
	RetryAfter time.Duration
}

func (e *TooManyAttemptsError) Error() string {
	return fmt.Sprintf("%s: retry in %s", domain.ErrTooManyAttempts, e.RetryAfter.Round(time.Second))
}

func (e *TooManyAttemptsError) Unwrap() error { return domain.ErrTooManyAttempts }

// ClientIP extracts the caller's address, honouring a proxy header only when
// the deployment says one is in front.
//
// trustProxy is not a default: with it on and no proxy present, any client can
// set X-Forwarded-For and pick which rate limit bucket to spend — which is to
// say, turn the per-IP limit off.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			// Leftmost is the original client; the rest were added by proxies.
			first, _, _ := strings.Cut(forwarded, ",")
			if ip := normaliseIP(strings.TrimSpace(first)); ip != "" {
				return ip
			}
		}
		if real := r.Header.Get("X-Real-Ip"); real != "" {
			if ip := normaliseIP(strings.TrimSpace(real)); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return normaliseIP(r.RemoteAddr)
	}
	return normaliseIP(host)
}

// normaliseIP validates and canonicalises an address, returning "" for anything
// unparseable so it is never stored in an inet column or used as a bucket key.
func normaliseIP(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	addr := net.ParseIP(raw)
	if addr == nil {
		return ""
	}
	return addr.String()
}

// plausibleEmail is a shape check, not validation. The only way to know an
// address works is to send to it, so anything stricter rejects valid addresses
// while still not proving anything about the ones it accepts.
func plausibleEmail(email string) bool {
	if len(email) < 3 || len(email) > 320 || strings.ContainsAny(email, " \t\r\n") {
		return false
	}
	local, domainPart, ok := strings.Cut(email, "@")
	return ok && local != "" && strings.Contains(domainPart, ".") && !strings.HasSuffix(domainPart, ".")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
