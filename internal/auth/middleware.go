package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

// contextKey is unexported so nothing outside this package can write the
// authenticated user into a request context. If any handler could put a user
// there, "is this request authenticated?" would stop being a question only this
// package answers.
type contextKey struct{ name string }

var (
	userKey = &contextKey{"user"}
	csrfKey = &contextKey{"csrf"}
)

// UserFrom returns the authenticated user, or nil when the request is
// anonymous. Handlers behind RequireUser can rely on it being non-nil.
func UserFrom(ctx context.Context) *domain.User {
	user, _ := ctx.Value(userKey).(*domain.User)
	return user
}

// CSRFTokenFrom returns the token a template should echo into its forms.
func CSRFTokenFrom(ctx context.Context) string {
	token, _ := ctx.Value(csrfKey).(string)
	return token
}

// ErrorResponder lets this package report failures in the transport's own error
// format, without importing the transport package and creating a cycle.
type ErrorResponder func(w http.ResponseWriter, r *http.Request, err error)

// Middleware wires the auth service into the HTTP stack.
type Middleware struct {
	svc        *Service
	respond    ErrorResponder
	trustProxy bool
}

// NewMiddleware builds the auth middleware.
func NewMiddleware(svc *Service, respond ErrorResponder, trustProxy bool) *Middleware {
	return &Middleware{svc: svc, respond: respond, trustProxy: trustProxy}
}

// Optional attaches the user to the request context when a valid session is
// present, and does nothing when it is not.
//
// It never rejects. Pages that render differently for a signed-in visitor need
// to know who they are without making anonymous access an error, and putting
// the lookup here rather than in each handler means no handler can forget it.
func (m *Middleware) Optional(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _ := TokenFromRequest(r, m.svc.cfg.Cookie.Name)
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}

		user, err := m.svc.Authenticate(r.Context(), token)
		if err != nil {
			if !errors.Is(err, domain.ErrUnauthenticated) {
				// A storage failure is not the same as a bad session, and
				// silently treating it as anonymous would hide an outage
				// behind a login page.
				m.svc.log.ErrorContext(r.Context(), "session lookup failed", "error", err)
			}
			next.ServeHTTP(w, r)
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
	})
}

// Require rejects requests that are not authenticated.
//
// It re-authenticates rather than reading what Optional may have left in the
// context, so it is safe on its own: a route protected by Require is protected
// whether or not anyone remembered to also wrap it in Optional.
func (m *Middleware) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := UserFrom(r.Context()); user != nil {
			next.ServeHTTP(w, r)
			return
		}

		token, _ := TokenFromRequest(r, m.svc.cfg.Cookie.Name)
		if token == "" {
			m.respond(w, r, domain.ErrUnauthenticated)
			return
		}

		user, err := m.svc.Authenticate(r.Context(), token)
		if err != nil {
			m.respond(w, r, err)
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
	})
}

// safeMethods do not change state, so CSRF does not apply to them.
var safeMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
	http.MethodTrace:   true,
}

// CSRF enforces the double-submit check on state-changing requests.
//
// The mechanism: a random token is written to a readable cookie, and the client
// must echo it back in a header or form field. A cross-origin attacker can make
// a browser *send* our cookies, but the same-origin policy stops them reading
// one — so they cannot produce the matching echo.
//
// Two cases are exempt, for the same underlying reason:
//
//   - Safe methods, which change nothing.
//   - Requests authenticated by an Authorization header rather than a cookie.
//     CSRF exists because browsers attach cookies to cross-site requests
//     automatically; they do not attach that header, so a script or a curl
//     command using a bearer token is not forgeable and does not need a token
//     it would have to be told to fetch first.
func (m *Middleware) CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if safeMethods[r.Method] {
			next.ServeHTTP(w, r)
			return
		}

		_, fromCookie := TokenFromRequest(r, m.svc.cfg.Cookie.Name)
		if !fromCookie {
			next.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie(m.svc.cfg.Cookie.csrfCookieName())
		if err != nil || cookie.Value == "" {
			m.respond(w, r, fmt.Errorf("%w: no csrf cookie on the request", domain.ErrCSRF))
			return
		}

		submitted := r.Header.Get("X-CSRF-Token")
		if submitted == "" {
			// Form posts cannot set headers, so the token also travels as a
			// field. ParseForm is bounded by the body limit the router applies.
			if err := r.ParseForm(); err == nil {
				submitted = r.PostFormValue("csrf_token")
			}
		}
		if submitted == "" {
			m.respond(w, r, fmt.Errorf("%w: no csrf token submitted", domain.ErrCSRF))
			return
		}
		if !sameToken(submitted, cookie.Value) {
			m.respond(w, r, fmt.Errorf("%w: csrf token does not match", domain.ErrCSRF))
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), csrfKey, cookie.Value)))
	})
}

// IssueCSRF makes sure a browser session has a CSRF cookie, minting one when it
// is missing, and puts the value in the context for templates to render.
//
// It runs on safe requests so that the token is already in place before the
// page that needs it is rendered: a form that renders without one would fail
// its first submission and look like a bug.
func (m *Middleware) IssueCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := m.svc.cfg.Cookie.csrfCookieName()

		token := ""
		if cookie, err := r.Cookie(name); err == nil {
			token = cookie.Value
		}
		if token == "" {
			minted, err := newCSRFToken()
			if err != nil {
				m.svc.log.ErrorContext(r.Context(), "could not mint a csrf token", "error", err)
				next.ServeHTTP(w, r)
				return
			}
			token = minted
			m.svc.cfg.Cookie.WriteCSRF(w, token, time.Now().Add(m.svc.cfg.TTL))
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), csrfKey, token)))
	})
}

// ClientIP is the caller's address as this middleware resolves it.
func (m *Middleware) ClientIP(r *http.Request) string { return ClientIP(r, m.trustProxy) }
