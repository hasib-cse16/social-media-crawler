package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/stats"
)

// fakeService lets the transport layer be tested without any network access.
type fakeService struct {
	result *stats.Result
	err    error
}

func (f fakeService) ByURL(context.Context, string) (*stats.Result, error) {
	return f.result, f.err
}

func (f fakeService) Platforms() []domain.Platform {
	return []domain.Platform{domain.PlatformYouTube}
}

// fakeProber stands in for the database in readiness checks.
type fakeProber struct{ err error }

func (f fakeProber) Ping(context.Context) error { return f.err }

func newTestRouter(svc StatsService) http.Handler {
	return newTestRouterWithConfig(svc, RouterConfig{HandlerTimeout: time.Second})
}

func newTestRouterWithConfig(svc StatsService, cfg RouterConfig) http.Handler {
	return newTestRouterWithProber(svc, cfg, fakeProber{})
}

func newTestRouterWithProber(svc StatsService, cfg RouterConfig, db Prober) http.Handler {
	log := slog.New(slog.DiscardHandler)
	return NewRouter(NewHandler(svc, log, "test", db), log, cfg)
}

func okService() fakeService {
	return fakeService{result: &stats.Result{Stats: &domain.VideoStats{
		Platform:  domain.PlatformYouTube,
		VideoID:   "dQw4w9WgXcQ",
		ViewCount: domain.U64(42),
	}}}
}

func TestGetStatsOK(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/stats?url=https://youtu.be/dQw4w9WgXcQ", nil)
	newTestRouter(okService()).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Data struct {
			VideoID   string `json:"video_id"`
			ViewCount uint64 `json:"view_count"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Data.ViewCount != 42 || body.Data.VideoID != "dQw4w9WgXcQ" {
		t.Errorf("unexpected body: %s", rr.Body.String())
	}
	if rr.Header().Get("X-Request-Id") == "" {
		t.Error("X-Request-Id header not set")
	}
}

func TestPostStatsOK(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/stats", strings.NewReader(`{"url":"https://youtu.be/dQw4w9WgXcQ"}`))
	newTestRouter(okService()).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestStatsErrorStatuses(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{domain.ErrInvalidURL, http.StatusBadRequest, "invalid_url"},
		{domain.ErrUnsupported, http.StatusBadRequest, "unsupported_platform"},
		{domain.ErrNotFound, http.StatusNotFound, "not_found"},
		{domain.ErrRateLimited, http.StatusTooManyRequests, "rate_limited"},
		{domain.ErrNotImplemented, http.StatusNotImplemented, "not_implemented"},
		{domain.ErrMisconfigured, http.StatusServiceUnavailable, "provider_unavailable"},
		{domain.ErrUpstreamFailure, http.StatusBadGateway, "upstream_error"},
		{context.DeadlineExceeded, http.StatusGatewayTimeout, "upstream_timeout"},
	}
	for _, tc := range cases {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/stats?url=https://x.test/a", nil)
		newTestRouter(fakeService{err: tc.err}).ServeHTTP(rr, req)

		if rr.Code != tc.wantStatus {
			t.Errorf("%v: status = %d, want %d", tc.err, rr.Code, tc.wantStatus)
		}
		var body struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		if body.Error.Code != tc.wantCode {
			t.Errorf("%v: code = %q, want %q", tc.err, body.Error.Code, tc.wantCode)
		}
	}
}

func TestMissingURLParam(t *testing.T) {
	rr := httptest.NewRecorder()
	newTestRouter(okService()).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/stats", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestHealthAndUnknownRoute(t *testing.T) {
	router := newTestRouter(okService())

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("healthz status = %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown route status = %d", rr.Code)
	}
}

func TestReadyzReflectsDatabaseHealth(t *testing.T) {
	cfg := RouterConfig{HandlerTimeout: time.Second}

	rr := httptest.NewRecorder()
	newTestRouterWithProber(okService(), cfg, fakeProber{}).
		ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("healthy database: status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"database":"ok"`) {
		t.Errorf("healthy database: body = %s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	newTestRouterWithProber(okService(), cfg, fakeProber{err: errors.New("connection refused")}).
		ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("unreachable database: status = %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"database":"unreachable"`) {
		t.Errorf("unreachable database: body = %s", rr.Body.String())
	}
}

// Liveness must not depend on the database: restarting the process does not
// repair a database, it only removes capacity from a fleet that would recover.
func TestHealthzIgnoresDatabase(t *testing.T) {
	rr := httptest.NewRecorder()
	newTestRouterWithProber(okService(), RouterConfig{HandlerTimeout: time.Second},
		fakeProber{err: errors.New("connection refused")}).
		ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rr.Code != http.StatusOK {
		t.Errorf("healthz status = %d with a dead database, want 200", rr.Code)
	}
}

func TestDocsRoutes(t *testing.T) {
	router := newTestRouterWithConfig(okService(), RouterConfig{HandlerTimeout: time.Second, DocsEnabled: true})

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("spec status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "openapi: 3.1.0") {
		t.Error("spec body does not look like an openapi document")
	}

	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "swagger-ui") {
		t.Errorf("docs status = %d, body = %q", rr.Code, rr.Body.String())
	}
}

func TestSwaggerAliases(t *testing.T) {
	router := newTestRouterWithConfig(okService(), RouterConfig{HandlerTimeout: time.Second, DocsEnabled: true})

	// The well-known swaggo/gin-swagger path serves the UI directly.
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/swagger/index.html", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "swagger-ui") {
		t.Errorf("/swagger/index.html status = %d, body = %q", rr.Code, rr.Body.String())
	}

	// Bare /swagger and /swagger/ redirect to the canonical /docs.
	for _, path := range []string{"/swagger", "/swagger/"} {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusMovedPermanently {
			t.Errorf("%s status = %d, want 301", path, rr.Code)
		}
		if loc := rr.Header().Get("Location"); loc != "/docs" {
			t.Errorf("%s Location = %q, want /docs", path, loc)
		}
	}
}

func TestDocsDisabled(t *testing.T) {
	router := newTestRouterWithConfig(okService(), RouterConfig{HandlerTimeout: time.Second, DocsEnabled: false})

	for _, path := range []string{"/docs", "/openapi.yaml", "/swagger/index.html", "/swagger"} {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404 when docs are disabled", path, rr.Code)
		}
	}
}
