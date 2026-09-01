package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

// respondJSON is a stand-in for the transport's error renderer.
func respondJSON(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	switch {
	case strings.Contains(err.Error(), domain.ErrUnauthenticated.Error()):
		status = http.StatusUnauthorized
	case strings.Contains(err.Error(), domain.ErrCSRF.Error()):
		status = http.StatusForbidden
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// whoami reports the authenticated user, or "anonymous".
var whoami = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	if user := UserFrom(r.Context()); user != nil {
		_, _ = w.Write([]byte(user.Email))
		return
	}
	_, _ = w.Write([]byte("anonymous"))
})

func testMiddleware(t *testing.T) (*Middleware, *Service, context.Context) {
	t.Helper()
	svc, _, ctx := testService(t, nil)
	return NewMiddleware(svc, respondJSON, false), svc, ctx
}

func TestRequireRejectsAnonymousRequests(t *testing.T) {
	mw, _, _ := testMiddleware(t)
	handler := mw.Require(whoami)

	tests := []struct {
		name  string
		setup func(*http.Request)
	}{
		{"no credentials", func(*http.Request) {}},
		{"junk cookie", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "ss_session", Value: "nonsense"})
		}},
		{"junk bearer", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer nonsense")
		}},
		{"well-formed but unknown token", func(r *http.Request) {
			token, _ := newToken()
			r.Header.Set("Authorization", "Bearer "+string(token))
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			tc.setup(req)
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 (body %s)", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestRequireAcceptsBothCookieAndBearer(t *testing.T) {
	mw, svc, ctx := testMiddleware(t)
	issued := register(t, svc, ctx, "mw@example.com")
	handler := mw.Require(whoami)

	t.Run("cookie", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(&http.Cookie{Name: "ss_session", Value: string(issued.Token)})
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK || rr.Body.String() != "mw@example.com" {
			t.Errorf("status %d, body %q", rr.Code, rr.Body.String())
		}
	})

	t.Run("bearer", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+string(issued.Token))
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK || rr.Body.String() != "mw@example.com" {
			t.Errorf("status %d, body %q", rr.Code, rr.Body.String())
		}
	})
}

// Optional never rejects: a page that renders differently when signed in still
// has to render when nobody is.
func TestOptionalNeverRejects(t *testing.T) {
	mw, svc, ctx := testMiddleware(t)
	issued := register(t, svc, ctx, "optional@example.com")
	handler := mw.Optional(whoami)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "anonymous" {
		t.Errorf("anonymous: status %d, body %q", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "ss_session", Value: "garbage"})
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != "anonymous" {
		t.Errorf("bad cookie: status %d, body %q", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "ss_session", Value: string(issued.Token)})
	handler.ServeHTTP(rr, req)
	if rr.Body.String() != "optional@example.com" {
		t.Errorf("signed in: body %q", rr.Body.String())
	}
}

// Require must protect a route on its own, whether or not anyone remembered to
// also wrap it in Optional.
func TestRequireWorksWithoutOptional(t *testing.T) {
	mw, svc, ctx := testMiddleware(t)
	issued := register(t, svc, ctx, "standalone@example.com")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+string(issued.Token))
	mw.Require(whoami).ServeHTTP(rr, req)

	if rr.Body.String() != "standalone@example.com" {
		t.Errorf("body = %q", rr.Body.String())
	}
}

// ---------- CSRF ----------

func csrfRequest(method, csrfCookie, submitted string, sessionCookie bool) *http.Request {
	req := httptest.NewRequest(method, "/", strings.NewReader(""))
	if sessionCookie {
		req.AddCookie(&http.Cookie{Name: "ss_session", Value: "a-session"})
	}
	if csrfCookie != "" {
		req.AddCookie(&http.Cookie{Name: "csrf", Value: csrfCookie})
	}
	if submitted != "" {
		req.Header.Set("X-CSRF-Token", submitted)
	}
	return req
}

func TestCSRF(t *testing.T) {
	mw, _, _ := testMiddleware(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("done")) })
	handler := mw.CSRF(ok)

	tests := []struct {
		name string
		req  *http.Request
		want int
	}{
		// Safe methods change nothing, so the check does not apply.
		{"GET with no token", csrfRequest(http.MethodGet, "", "", true), http.StatusOK},
		{"HEAD with no token", csrfRequest(http.MethodHead, "", "", true), http.StatusOK},

		{"matching tokens", csrfRequest(http.MethodPost, "abc123", "abc123", true), http.StatusOK},
		{"mismatched tokens", csrfRequest(http.MethodPost, "abc123", "different", true), http.StatusForbidden},
		{"no cookie", csrfRequest(http.MethodPost, "", "abc123", true), http.StatusForbidden},
		{"nothing submitted", csrfRequest(http.MethodPost, "abc123", "", true), http.StatusForbidden},

		// Sign-in and sign-up have no session yet, and are checked anyway:
		// leaving them out allows login CSRF, where an attacker signs the
		// victim into an account the attacker controls.
		{"form post with no session", csrfRequest(http.MethodPost, "abc123", "", false), http.StatusForbidden},
		{"form post with no session, valid token", csrfRequest(http.MethodPost, "abc123", "abc123", false), http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, tc.req)
			if rr.Code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

// Two shapes of request prove they were not produced by a cross-site form, and
// both must pass without a token — otherwise the JSON API becomes unusable from
// any client that has not first fetched one.
func TestCSRFExemptsRequestsThatCannotBeForged(t *testing.T) {
	mw, _, _ := testMiddleware(t)
	handler := mw.CSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("done"))
	}))

	tests := []struct {
		name  string
		build func() *http.Request
	}{
		{
			// Browsers do not attach this header on a third party's behalf.
			name: "bearer token",
			build: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
				req.Header.Set("Authorization", "Bearer some-token")
				return req
			},
		},
		{
			// An HTML form can only send urlencoded, multipart or text/plain;
			// application/json requires fetch, which is preflighted.
			name: "json body",
			build: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(&http.Cookie{Name: "ss_session", Value: "a-session"})
				return req
			},
		},
		{
			name: "json body with a charset parameter",
			build: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
				req.Header.Set("Content-Type", "application/json; charset=utf-8")
				return req
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, tc.build())
			if rr.Code != http.StatusOK {
				t.Errorf("status = %d, want the check skipped (body %s)", rr.Code, rr.Body.String())
			}
		})
	}
}

// The exemptions must not be a way around the check: a form-encoded post is
// checked whatever else it carries.
func TestCSRFStillChecksFormPosts(t *testing.T) {
	mw, _, _ := testMiddleware(t)
	handler := mw.CSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("done"))
	}))

	for _, contentType := range []string{
		"application/x-www-form-urlencoded",
		"multipart/form-data; boundary=x",
		"text/plain",
		"",
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		req.AddCookie(&http.Cookie{Name: "ss_session", Value: "a-session"})
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Errorf("content-type %q: status = %d, want 403", contentType, rr.Code)
		}
	}
}

// Form posts cannot set headers, so the token also travels as a field.
func TestCSRFAcceptsAFormField(t *testing.T) {
	mw, _, _ := testMiddleware(t)
	handler := mw.CSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("done"))
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("csrf_token=abc123&email=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "ss_session", Value: "a-session"})
	req.AddCookie(&http.Cookie{Name: "csrf", Value: "abc123"})
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
}

// The token must be in place before the page that needs it renders; a form that
// renders without one fails its first submission and looks like a bug.
func TestIssueCSRFMintsAndReusesAToken(t *testing.T) {
	mw, _, _ := testMiddleware(t)

	var seen string
	handler := mw.IssueCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = CSRFTokenFrom(r.Context())
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Fatal("no csrf token was put in the context")
	}
	var minted string
	for _, c := range rr.Result().Cookies() {
		if c.Name == "csrf" {
			minted = c.Value
		}
	}
	if minted != seen {
		t.Errorf("cookie %q does not match the context value %q", minted, seen)
	}
	if minted == "" {
		t.Fatal("no csrf cookie was set")
	}

	// A request that already has one keeps it, so the token does not rotate out
	// from under a form the user is still filling in.
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "csrf", Value: minted})
	handler.ServeHTTP(rr, req)

	if seen != minted {
		t.Errorf("an existing token was replaced: %q became %q", minted, seen)
	}
	if len(rr.Result().Cookies()) != 0 {
		t.Error("a cookie was rewritten when one was already present")
	}
}

// ---------- cookies ----------

func TestSessionCookieAttributes(t *testing.T) {
	cfg := CookieConfig{Name: "ss_session", Secure: true, TTL: time.Hour}

	rr := httptest.NewRecorder()
	cfg.WriteSession(rr, "the-token", time.Now().Add(time.Hour))

	cookies := rr.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("%d cookies set, want 1", len(cookies))
	}
	c := cookies[0]

	if !c.HttpOnly {
		t.Error("session cookie is not HttpOnly; an XSS bug could read the session")
	}
	if !c.Secure {
		t.Error("session cookie is not Secure")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want /", c.Path)
	}
	if c.Value != "the-token" {
		t.Errorf("value = %q", c.Value)
	}
}

// The CSRF cookie is deliberately readable: the page's own script has to echo
// it back. It is not a credential on its own.
func TestCSRFCookieIsReadableAndPrefixedWhenSecure(t *testing.T) {
	secure := CookieConfig{Name: "ss_session", Secure: true, TTL: time.Hour}
	rr := httptest.NewRecorder()
	secure.WriteCSRF(rr, "token", time.Now().Add(time.Hour))

	c := rr.Result().Cookies()[0]
	if c.HttpOnly {
		t.Error("the csrf cookie is HttpOnly; the page cannot read it to echo it back")
	}
	if c.Name != "__Host-csrf" {
		t.Errorf("name = %q, want the __Host- prefix on a secure cookie", c.Name)
	}

	// The prefix is only legal on secure cookies, so development falls back.
	insecure := CookieConfig{Name: "ss_session", Secure: false, TTL: time.Hour}
	rr = httptest.NewRecorder()
	insecure.WriteCSRF(rr, "token", time.Now().Add(time.Hour))
	if name := rr.Result().Cookies()[0].Name; name != "csrf" {
		t.Errorf("insecure csrf cookie name = %q, want csrf", name)
	}
}

// Clearing must match the attributes the cookie was set with, or the browser
// treats it as a different cookie and the real session survives a "sign out".
func TestClearSessionMatchesTheSetAttributes(t *testing.T) {
	cfg := CookieConfig{Name: "ss_session", Secure: true, Domain: "example.com", TTL: time.Hour}

	set := httptest.NewRecorder()
	cfg.WriteSession(set, "x", time.Now().Add(time.Hour))
	clear := httptest.NewRecorder()
	cfg.ClearSession(clear)

	a, b := set.Result().Cookies()[0], clear.Result().Cookies()[0]
	if a.Name != b.Name || a.Path != b.Path || a.Domain != b.Domain ||
		a.Secure != b.Secure || a.SameSite != b.SameSite {
		t.Errorf("clear attributes do not match set:\n set   %+v\n clear %+v", a, b)
	}
	if b.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want a negative value to expire it", b.MaxAge)
	}
	if b.Value != "" {
		t.Errorf("cleared cookie still carries %q", b.Value)
	}
}

func TestTokenFromRequestPrefersTheHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "ss_session", Value: "from-cookie"})
	req.Header.Set("Authorization", "Bearer from-header")

	token, fromCookie := TokenFromRequest(req, "ss_session")
	if token != "from-header" || fromCookie {
		t.Errorf("token = %q, fromCookie = %v; want the header to win", token, fromCookie)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "ss_session", Value: "from-cookie"})
	token, fromCookie = TokenFromRequest(req, "ss_session")
	if token != "from-cookie" || !fromCookie {
		t.Errorf("token = %q, fromCookie = %v", token, fromCookie)
	}

	// A non-Bearer scheme is not a session token.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	if token, _ := TokenFromRequest(req, "ss_session"); token != "" {
		t.Errorf("token = %q, want empty for a Basic header", token)
	}
}
