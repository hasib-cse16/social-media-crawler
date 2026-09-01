package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/foodibd/socialstats/internal/api"
	"github.com/foodibd/socialstats/internal/auth"
	"github.com/foodibd/socialstats/internal/config"
	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/stats"
	"github.com/foodibd/socialstats/internal/storage/postgres"
	"github.com/foodibd/socialstats/internal/storage/postgres/pgtest"
	"github.com/foodibd/socialstats/internal/tracking"
)

// Template parsing happens in New, so a template with a typo in it fails here
// rather than on the one page nobody opened before release. This test is what
// makes that guarantee real in CI.
func TestTemplatesParse(t *testing.T) {
	s, err := New(nil, nil, Config{Version: "test"}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, page := range pages {
		if s.templates[page] == nil {
			t.Errorf("template %s was not parsed", page)
		}
	}
	if !strings.HasPrefix(s.assetPath, "/static/") || len(s.assetPath) != len("/static/")+12 {
		t.Errorf("asset path = %q, want a hashed prefix", s.assetPath)
	}
}

// The asset path must change when a file does, or a deploy serves a stale
// stylesheet from a cache that was told to hold it for a year.
func TestAssetHashIsStable(t *testing.T) {
	first, err := hashAssets()
	if err != nil {
		t.Fatalf("hashAssets: %v", err)
	}
	second, _ := hashAssets()
	if first != second {
		t.Errorf("hash is not deterministic: %q then %q", first, second)
	}
}

// An open redirect is a phishing primitive: a link to /login?next=https://evil
// would take somebody through a real sign-in and out to an attacker's page.
func TestSafeNextRejectsOffsiteTargets(t *testing.T) {
	rejected := []string{
		"https://evil.example",
		"//evil.example",
		"/\\evil.example",
		"http://evil.example/path",
		"javascript:alert(1)",
		"evil.example",
		"",
	}
	for _, next := range rejected {
		if got := safeNext(next); got != "" {
			t.Errorf("safeNext(%q) = %q, want it rejected", next, got)
		}
	}

	accepted := []string{"/", "/videos/abc", "/?sort=views"}
	for _, next := range accepted {
		if got := safeNext(next); got != next {
			t.Errorf("safeNext(%q) = %q, want it kept", next, got)
		}
	}
}

// One middleware protects both surfaces; only the presentation differs. A
// browser navigating should land on the sign-in form, while a script wants a
// status it can branch on — a redirect to an HTML page is useless to it.
func TestPrefersHTML(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"browser navigation", map[string]string{
			"Accept": "text/html,application/xhtml+xml", "Sec-Fetch-Mode": "navigate"}, true},
		{"fetch from a page", map[string]string{
			"Accept": "*/*", "Sec-Fetch-Mode": "cors"}, false},
		{"explicit json wins", map[string]string{
			"Accept": "application/json", "Sec-Fetch-Mode": "navigate"}, false},
		{"xhr header", map[string]string{
			"Accept": "text/html", "X-Requested-With": "XMLHttpRequest"}, false},
		{"old browser, accept only", map[string]string{"Accept": "text/html"}, true},
		{"curl", map[string]string{"Accept": "*/*"}, false},
		{"no headers", map[string]string{}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := prefersHTML(r); got != tc.want {
				t.Errorf("prefersHTML = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------- integration ----------

type harness struct {
	handler http.Handler
	db      *postgres.DB
	ctx     context.Context
	auth    *auth.Service
}

func newHarness(t *testing.T) *harness {
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

	log := slog.New(slog.DiscardHandler)
	hasher := auth.NewHasher(auth.HashParams{Time: 1, Memory: 8 * 1024, Threads: 1, SaltLength: 16, KeyLength: 32}, 2)

	authSvc, err := auth.NewService(db.Users(), db.Sessions(), db.RateLimits(), hasher, auth.Config{
		TTL: time.Hour, IdleTTL: time.Hour, TouchInterval: time.Minute,
		RegistrationOpen: true, LoginAttempts: 20, LoginWindow: time.Minute,
		Cookie: auth.CookieConfig{Name: "ss_session", TTL: time.Hour},
	}, log)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}

	registry := stubResolver{}
	trackingSvc := tracking.NewService(db.Videos(), db.Tracking(), db.Metrics(), registry,
		tracking.Config{MaxTrackedPerUser: 10}, log)

	web, err := New(trackingSvc, authSvc, Config{Version: "test", RegistrationOpen: true}, log)
	if err != nil {
		t.Fatalf("web: %v", err)
	}

	mw := auth.NewMiddleware(authSvc, web.ErrorResponder(api.RespondError), false)
	handler := api.NewRouter(
		api.NewHandler(stats.NewService(registry, stats.NewCache(0), log), log, "test", db),
		log,
		api.RouterConfig{
			HandlerTimeout: 10 * time.Second,
			Middleware:     mw,
			Web:            web,
			// The JSON surface is wired up too, so these tests exercise the
			// same router the binary builds — the whole point of the content
			// negotiation is that both are mounted on one mux.
			Auth:     api.NewAuthHandler(authSvc, mw),
			Tracking: api.NewTrackingHandler(trackingSvc),
		},
	)

	return &harness{handler: handler, db: db, ctx: ctx, auth: authSvc}
}

// stubResolver stands in for the provider registry; these tests are about
// rendering, not fetching.
type stubResolver struct{}

func (stubResolver) For(string) (domain.Provider, error) { return nil, domain.ErrUnsupported }
func (stubResolver) Platforms() []domain.Platform {
	return []domain.Platform{domain.PlatformYouTube, domain.PlatformTikTok, domain.PlatformMeta}
}

func browserRequest(method, target string, body url.Values) *http.Request {
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Header.Set("Accept", "text/html,application/xhtml+xml")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	return r
}

// signIn creates an account and returns the cookies a browser would hold.
func (h *harness) signIn(t *testing.T, email string) []*http.Cookie {
	t.Helper()

	issued, err := h.auth.Register(h.ctx, auth.RegisterInput{
		Email: email, Password: "seven blue mountains rise",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	return []*http.Cookie{
		{Name: "ss_session", Value: string(issued.Token)},
		{Name: "csrf", Value: issued.CSRFToken},
	}
}

func (h *harness) do(r *http.Request, cookies []*http.Cookie) *httptest.ResponseRecorder {
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, r)
	return rr
}

func TestAnonymousBrowserIsSentToSignIn(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{"/", "/settings", "/videos/00000000-0000-0000-0000-000000000000"} {
		rr := h.do(browserRequest(http.MethodGet, path, nil), nil)

		if rr.Code != http.StatusSeeOther {
			t.Errorf("%s: status = %d, want a redirect", path, rr.Code)
			continue
		}
		location := rr.Header().Get("Location")
		if !strings.HasPrefix(location, "/login") {
			t.Errorf("%s: redirected to %q", path, location)
		}
		// Where they were going is remembered, so they land there rather than
		// on a generic dashboard.
		if path != "/" && !strings.Contains(location, "next=") {
			t.Errorf("%s: redirect lost the destination: %q", path, location)
		}
	}
}

// The same URL, called by a script, must answer with a status rather than a
// redirect to a page it cannot use.
func TestApiClientGetsAStatusNotARedirect(t *testing.T) {
	h := newHarness(t)

	r := httptest.NewRequest(http.MethodGet, "/v1/videos", nil)
	r.Header.Set("Accept", "application/json")
	rr := h.do(r, nil)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"unauthenticated"`) {
		t.Errorf("body = %s", rr.Body.String())
	}
}

func TestSignedInDashboardRenders(t *testing.T) {
	h := newHarness(t)
	cookies := h.signIn(t, "dash@example.com")

	rr := h.do(browserRequest(http.MethodGet, "/", nil), cookies)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}

	body := rr.Body.String()
	for _, want := range []string{
		"<title>Dashboard · socialstats</title>",
		`action="/videos"`,    // the add form
		`name="csrf_token"`,   // and its token
		"Nothing tracked yet", // the empty state, distinct from "no matches"
		"Sign out",            // signed-in chrome
		`aria-current="page"`, // current nav item
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard is missing %q", want)
		}
	}

	// Pages are per-user and must not be cached by a shared proxy.
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// A form rendered without a token fails its first submission and looks like a
// bug, so the token has to be in place before the page that needs it.
func TestSignInFormCarriesACSRFToken(t *testing.T) {
	h := newHarness(t)

	rr := h.do(browserRequest(http.MethodGet, "/login", nil), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}

	token := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(rr.Body.String())
	if token == nil {
		t.Fatal("no csrf token in the sign-in form")
	}

	var cookie string
	for _, c := range rr.Result().Cookies() {
		if c.Name == "csrf" {
			cookie = c.Value
		}
	}
	if cookie == "" || cookie != token[1] {
		t.Errorf("cookie %q does not match the form field %q", cookie, token[1])
	}
}

// Sign-in has no session to protect yet and is checked anyway: without it, an
// attacker can sign a victim into an account the attacker controls.
func TestSignInFormIsCSRFProtected(t *testing.T) {
	h := newHarness(t)
	h.signIn(t, "victim@example.com")

	form := url.Values{"email": {"victim@example.com"}, "password": {"seven blue mountains rise"}}
	rr := h.do(browserRequest(http.MethodPost, "/login", form),
		[]*http.Cookie{{Name: "csrf", Value: "a-token-the-attacker-cannot-read"}})

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 without a matching token", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "That form has expired") {
		t.Errorf("body did not explain the refusal: %s", truncate(rr.Body.String(), 200))
	}
}

func TestSignInAndOut(t *testing.T) {
	h := newHarness(t)
	h.signIn(t, "flow@example.com")

	// A fresh browser: fetch the form to get a token.
	rr := h.do(browserRequest(http.MethodGet, "/login", nil), nil)
	token := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(rr.Body.String())[1]
	csrf := &http.Cookie{Name: "csrf", Value: token}

	form := url.Values{
		"csrf_token": {token},
		"email":      {"flow@example.com"},
		"password":   {"seven blue mountains rise"},
	}
	rr = h.do(browserRequest(http.MethodPost, "/login", form), []*http.Cookie{csrf})

	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("status = %d, location = %q", rr.Code, rr.Header().Get("Location"))
	}
	var session *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "ss_session" && c.Value != "" {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie was set")
	}
	if !session.HttpOnly {
		t.Error("the session cookie is readable by scripts")
	}

	// And out again.
	rr = h.do(browserRequest(http.MethodPost, "/logout", url.Values{"csrf_token": {token}}),
		[]*http.Cookie{session, csrf})
	if rr.Code != http.StatusSeeOther {
		t.Errorf("logout status = %d", rr.Code)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "ss_session" && c.MaxAge >= 0 {
			t.Error("the session cookie was not cleared")
		}
	}
}

// A wrong password re-renders the form with the email intact, rather than
// redirecting and making the person type it again to find out what happened.
func TestFailedSignInKeepsTheTypedEmail(t *testing.T) {
	h := newHarness(t)
	h.signIn(t, "typo@example.com")

	rr := h.do(browserRequest(http.MethodGet, "/login", nil), nil)
	token := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(rr.Body.String())[1]

	form := url.Values{
		"csrf_token": {token}, "email": {"typo@example.com"}, "password": {"wrong password here"},
	}
	rr = h.do(browserRequest(http.MethodPost, "/login", form),
		[]*http.Cookie{{Name: "csrf", Value: token}})

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `value="typo@example.com"`) {
		t.Error("the typed email was not preserved")
	}
	// Identical wording for a wrong password and an unknown address.
	if !strings.Contains(body, "do not match an account") {
		t.Errorf("no explanation shown: %s", truncate(body, 300))
	}
}

// A video the caller does not track is a 404 page, not a permission error —
// the dashboard must not confirm that somebody else's video exists.
func TestUnknownVideoRendersTheSiteNotFoundPage(t *testing.T) {
	h := newHarness(t)
	cookies := h.signIn(t, "nosy@example.com")

	rr := h.do(browserRequest(http.MethodGet, "/videos/00000000-0000-0000-0000-000000000000", nil), cookies)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "not on your list") {
		t.Errorf("body = %s", truncate(rr.Body.String(), 300))
	}
	// Rendered in the site's own layout rather than as a bare string.
	if !strings.Contains(rr.Body.String(), "<title>") {
		t.Error("the error page is not a page")
	}
}

func TestUnknownPathNegotiates(t *testing.T) {
	h := newHarness(t)

	rr := h.do(browserRequest(http.MethodGet, "/no/such/page", nil), nil)
	if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "<title>") {
		t.Errorf("browser 404: status %d, body %s", rr.Code, truncate(rr.Body.String(), 120))
	}

	r := httptest.NewRequest(http.MethodGet, "/no/such/page", nil)
	r.Header.Set("Accept", "application/json")
	rr = h.do(r, nil)
	if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), `"route_not_found"`) {
		t.Errorf("api 404: status %d, body %s", rr.Code, rr.Body.String())
	}
}

func TestStaticAssetsAreServedAndCacheable(t *testing.T) {
	h := newHarness(t)
	hash, _ := hashAssets()

	for _, name := range []string{"app.css", "app.js"} {
		rr := h.do(httptest.NewRequest(http.MethodGet, "/static/"+hash+"/"+name, nil), nil)

		if rr.Code != http.StatusOK {
			t.Errorf("%s: status = %d", name, rr.Code)
			continue
		}
		if rr.Body.Len() == 0 {
			t.Errorf("%s is empty", name)
		}
		// The path carries a content hash, so the file is immutable by
		// construction and can be held for a year.
		if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("%s: Cache-Control = %q", name, cc)
		}
	}

	// Assets are public: putting them behind a session would break the styling
	// of the sign-in page itself.
	rr := h.do(httptest.NewRequest(http.MethodGet, "/static/"+hash+"/app.css", nil), nil)
	if rr.Code != http.StatusOK {
		t.Errorf("anonymous asset fetch = %d", rr.Code)
	}
}

// Titles, labels and notes are user-supplied and end up in HTML.
func TestUserSuppliedTextIsEscaped(t *testing.T) {
	h := newHarness(t)
	cookies := h.signIn(t, "xss@example.com")

	user, err := h.db.Users().ByEmail(h.ctx, "xss@example.com")
	if err != nil {
		t.Fatalf("ByEmail: %v", err)
	}
	video, err := h.db.Videos().Upsert(h.ctx, domain.NewVideo{
		Platform: domain.PlatformYouTube, PlatformVideoID: "xss", CanonicalURL: "https://x.test/xss",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	const payload = `<script>alert('xss')</script>`
	if _, err := h.db.Tracking().Track(h.ctx, user.ID, video.ID, payload); err != nil {
		t.Fatalf("Track: %v", err)
	}

	rr := h.do(browserRequest(http.MethodGet, "/", nil), cookies)
	body := rr.Body.String()

	if strings.Contains(body, "<script>alert") {
		t.Fatal("a label was rendered as markup")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("the label does not appear escaped: %s", truncate(body, 400))
	}
}

// Bad query parameters must fall back rather than reaching a query or a 500.
func TestQueryParametersAreWhitelisted(t *testing.T) {
	h := newHarness(t)
	cookies := h.signIn(t, "params@example.com")

	for _, query := range []string{
		"?sort=" + url.QueryEscape("id;DROP TABLE users"),
		"?window=notaduration",
		"?platform=myspace",
		"?sort=&window=&platform=",
		"?window=99999h",
	} {
		rr := h.do(browserRequest(http.MethodGet, "/"+query, nil), cookies)
		if rr.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want the defaults to apply", query, rr.Code)
		}
	}

	var count int
	if err := h.db.Pool.QueryRow(h.ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("the users table is gone: %v", err)
	}
}
