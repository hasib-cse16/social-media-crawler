package tiktok

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/foodibd/socialstats/internal/config"
	"github.com/foodibd/socialstats/internal/domain"
)

// pageWith wraps a video-detail payload in a page large enough to pass the
// stub-page check, mirroring the real page's shape.
func pageWith(detail string) string {
	state := `{"__DEFAULT_SCOPE__":{"webapp.video-detail":` + detail + `}}`
	padding := strings.Repeat("<!-- markup -->", 1200) // > minPlausiblePageBytes
	return "<!DOCTYPE html><html><body>" + padding +
		`<script id="__UNIVERSAL_DATA_FOR_REHYDRATION__" type="application/json">` + state + `</script>` +
		"</body></html>"
}

const okDetail = `{
  "statusCode": 0,
  "itemInfo": {"itemStruct": {
    "id": "7249376077976472833",
    "desc": "Jim Simons and Renaissance Technologies",
    "createTime": 1687876902,
    "author": {"id": "7205953225726624770", "uniqueId": "thebillionairebros", "nickname": "TheBillionaireBros"},
    "stats": {"diggCount": 8702, "shareCount": 690, "commentCount": 113, "playCount": 220500},
    "statsV2": {"diggCount": "8702", "shareCount": "690", "commentCount": "113", "playCount": "220500", "collectCount": "2732"}
  }}
}`

func newTestProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := config.TikTokConfig{BaseURL: srv.URL, UserAgent: "test-agent", On: true, MaxAttempts: 1}
	return New(cfg, srv.Client(), slog.New(slog.DiscardHandler))
}

// statsVia points the provider at the test server by rewriting the request host,
// since Stats builds a canonical tiktok.com URL internally.
func statsVia(t *testing.T, handler http.HandlerFunc, rawURL string) (*domain.VideoStats, error) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client := srv.Client()
	client.Transport = rewriteHost{base: srv.URL, next: client.Transport}

	p := New(config.TikTokConfig{UserAgent: "test-agent", On: true, MaxAttempts: 1}, client, slog.New(slog.DiscardHandler))
	return p.Stats(context.Background(), rawURL)
}

// rewriteHost sends every request to the test server, whatever host was asked for.
type rewriteHost struct {
	base string
	next http.RoundTripper
}

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	target := strings.TrimPrefix(r.base, "http://")
	req.URL.Scheme = "http"
	req.URL.Host = target
	return r.next.RoundTrip(req)
}

func TestStatsSuccess(t *testing.T) {
	var gotUA, gotPath string
	got, err := statsVia(t, func(w http.ResponseWriter, r *http.Request) {
		gotUA, gotPath = r.Header.Get("User-Agent"), r.URL.Path
		_, _ = w.Write([]byte(pageWith(okDetail)))
	}, "https://www.tiktok.com/@thebillionairebros/video/7249376077976472833")

	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if gotUA != "test-agent" {
		t.Errorf("User-Agent = %q, want the configured agent", gotUA)
	}
	if gotPath != "/@thebillionairebros/video/7249376077976472833" {
		t.Errorf("requested path = %q", gotPath)
	}
	if got.Platform != domain.PlatformTikTok {
		t.Errorf("Platform = %q", got.Platform)
	}
	if got.ViewCount == nil || *got.ViewCount != 220500 {
		t.Errorf("ViewCount = %v, want 220500", got.ViewCount)
	}
	if got.LikeCount == nil || *got.LikeCount != 8702 {
		t.Errorf("LikeCount = %v, want 8702", got.LikeCount)
	}
	if got.ShareCount == nil || *got.ShareCount != 690 {
		t.Errorf("ShareCount = %v, want 690", got.ShareCount)
	}
	if got.SaveCount == nil || *got.SaveCount != 2732 {
		t.Errorf("SaveCount = %v, want 2732", got.SaveCount)
	}
	if got.ChannelTitle != "TheBillionaireBros" {
		t.Errorf("ChannelTitle = %q", got.ChannelTitle)
	}
	if got.PublishedAt == nil || got.PublishedAt.Year() != 2023 {
		t.Errorf("PublishedAt = %v, want a 2023 timestamp", got.PublishedAt)
	}
	if got.CanonicalURL != "https://www.tiktok.com/@thebillionairebros/video/7249376077976472833" {
		t.Errorf("CanonicalURL = %q", got.CanonicalURL)
	}
}

func TestStatsPrefersStringCounters(t *testing.T) {
	// statsV2 disagrees with stats; the string value must win, and must survive
	// magnitudes that a float64 would round.
	detail := `{"statusCode":0,"itemInfo":{"itemStruct":{"id":"7249376077976472833",
		"stats":{"playCount":1},"statsV2":{"playCount":"9007199254740993"}}}}`

	got, err := statsVia(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(pageWith(detail)))
	}, "https://www.tiktok.com/@u/video/7249376077976472833")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.ViewCount == nil || *got.ViewCount != 9007199254740993 {
		t.Errorf("ViewCount = %v, want the exact statsV2 value", got.ViewCount)
	}
}

func TestStatsFallsBackToNumericCounters(t *testing.T) {
	detail := `{"statusCode":0,"itemInfo":{"itemStruct":{"id":"7249376077976472833",
		"stats":{"playCount":4321}}}}`

	got, err := statsVia(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(pageWith(detail)))
	}, "https://www.tiktok.com/@u/video/7249376077976472833")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.ViewCount == nil || *got.ViewCount != 4321 {
		t.Errorf("ViewCount = %v, want 4321 from the numeric block", got.ViewCount)
	}
	if got.SaveCount != nil {
		t.Error("SaveCount should stay nil when TikTok does not report it")
	}
}

func TestStatsStatusCodeMapping(t *testing.T) {
	cases := []struct {
		name    string
		detail  string
		wantErr error
	}{
		{"item missing", `{"statusCode":10204,"statusMsg":"item doesn't exist"}`, domain.ErrNotFound},
		{"item deleted", `{"statusCode":10217}`, domain.ErrNotFound},
		{"item invisible", `{"statusCode":10216}`, domain.ErrNotFound},
		{"author private", `{"statusCode":10222}`, domain.ErrNotFound},
		{"region blocked", `{"statusCode":10231}`, domain.ErrNotFound},
		{"unknown status", `{"statusCode":99999}`, domain.ErrUpstreamFailure},
		{"ok but empty", `{"statusCode":0,"itemInfo":{"itemStruct":{}}}`, domain.ErrUpstreamFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := statsVia(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(pageWith(tc.detail)))
			}, "https://www.tiktok.com/@u/video/7249376077976472833")
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestStatsDetectsBlocking(t *testing.T) {
	t.Run("hard block stub is not retried", func(t *testing.T) {
		// Retrying a hard block deepens it, so the provider must give up at once.
		var attempts int32
		cfg := config.TikTokConfig{UserAgent: "t", On: true, MaxAttempts: 4, RetryBackoff: time.Millisecond}
		_, err := statsViaWithConfig(t, cfg, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&attempts, 1)
			_, _ = w.Write([]byte(`<html><body>blocked</body></html>`))
		}, "https://www.tiktok.com/@u/video/7249376077976472833")

		if !errors.Is(err, domain.ErrBlocked) {
			t.Errorf("err = %v, want ErrBlocked", err)
		}
		if n := atomic.LoadInt32(&attempts); n != 1 {
			t.Errorf("attempts = %d, want 1 (a hard block must not be retried)", n)
		}
	})

	t.Run("soft challenge page is retried then reported", func(t *testing.T) {
		body := "<html><body>" + strings.Repeat("x", 20<<10) + "</body></html>"
		_, err := statsVia(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}, "https://www.tiktok.com/@u/video/7249376077976472833")
		if !errors.Is(err, domain.ErrBlocked) {
			t.Errorf("err = %v, want ErrBlocked", err)
		}
	})
}

func TestStatsMissingStateScriptIsTreatedAsAChallenge(t *testing.T) {
	// A full-size page with no state script is what TikTok's anti-bot challenge
	// looks like, so it is reported as blocked (and retried) rather than as a
	// parse failure. Crucially it never reports zero views.
	body := "<html><body>" + strings.Repeat("y", 20<<10) + "</body></html>"
	_, err := statsVia(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}, "https://www.tiktok.com/@u/video/7249376077976472833")
	if !errors.Is(err, domain.ErrBlocked) {
		t.Errorf("err = %v, want ErrBlocked", err)
	}
}

func TestStatsMalformedStateFailsLoudly(t *testing.T) {
	// The script is present but its contents are not what we expect: that is a
	// genuine upstream/parser mismatch, not a challenge.
	page := pageWith(`{"statusCode":0,"itemInfo":{"itemStruct":{"id":`) // truncated json
	_, err := statsVia(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(page))
	}, "https://www.tiktok.com/@u/video/7249376077976472833")
	if !errors.Is(err, domain.ErrUpstreamFailure) {
		t.Errorf("err = %v, want ErrUpstreamFailure", err)
	}
}

func TestStatsHTTPStatusMapping(t *testing.T) {
	cases := []struct {
		status  int
		wantErr error
	}{
		{http.StatusNotFound, domain.ErrNotFound},
		{http.StatusTooManyRequests, domain.ErrRateLimited},
		{http.StatusInternalServerError, domain.ErrUpstreamFailure},
	}
	for _, tc := range cases {
		_, err := statsVia(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}, "https://www.tiktok.com/@u/video/7249376077976472833")
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("status %d: err = %v, want %v", tc.status, err, tc.wantErr)
		}
	}
}

func TestStatsRejectsBadURLBeforeFetching(t *testing.T) {
	called := false
	_, err := statsVia(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	}, "https://www.tiktok.com/@u/video/not-an-id")

	if !errors.Is(err, domain.ErrInvalidURL) {
		t.Errorf("err = %v, want ErrInvalidURL", err)
	}
	if called {
		t.Error("provider hit the network for an unparseable url")
	}
}

func TestStatsDisabledProvider(t *testing.T) {
	p := New(config.TikTokConfig{On: false}, http.DefaultClient, slog.New(slog.DiscardHandler))
	_, err := p.Stats(context.Background(), "https://www.tiktok.com/@u/video/7249376077976472833")
	if !errors.Is(err, domain.ErrMisconfigured) {
		t.Errorf("err = %v, want ErrMisconfigured", err)
	}
}

func TestMatch(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {})
	if !p.Match("https://vm.tiktok.com/ZTRfabcdef/") {
		t.Error("Match should claim tiktok short links")
	}
	if p.Match("https://youtu.be/dQw4w9WgXcQ") {
		t.Error("Match should not claim youtube urls")
	}
}

// statsViaWithConfig is statsVia with control over retry settings.
func statsViaWithConfig(t *testing.T, cfg config.TikTokConfig, handler http.HandlerFunc, rawURL string) (*domain.VideoStats, error) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client := srv.Client()
	client.Transport = rewriteHost{base: srv.URL, next: client.Transport}

	return New(cfg, client, slog.New(slog.DiscardHandler)).
		Stats(context.Background(), rawURL)
}

func TestStatsRetriesPastChallengePages(t *testing.T) {
	// TikTok serves the challenge page for a fraction of requests; the third
	// attempt gets the real page.
	var attempts int32
	cfg := config.TikTokConfig{UserAgent: "t", On: true, MaxAttempts: 4, RetryBackoff: time.Millisecond}

	got, err := statsViaWithConfig(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			// Full-size page with no state script: the observed challenge shape.
			_, _ = w.Write([]byte("<html><body>" + strings.Repeat("z", 40<<10) + "</body></html>"))
			return
		}
		_, _ = w.Write([]byte(pageWith(okDetail)))
	}, "https://www.tiktok.com/@u/video/7249376077976472833")

	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.ViewCount == nil || *got.ViewCount != 220500 {
		t.Errorf("ViewCount = %v, want 220500", got.ViewCount)
	}
	if n := atomic.LoadInt32(&attempts); n != 3 {
		t.Errorf("attempts = %d, want 3", n)
	}
}

func TestStatsGivesUpAfterMaxAttempts(t *testing.T) {
	var attempts int32
	cfg := config.TikTokConfig{UserAgent: "t", On: true, MaxAttempts: 3, RetryBackoff: time.Millisecond}

	_, err := statsViaWithConfig(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		_, _ = w.Write([]byte("<html><body>" + strings.Repeat("z", 40<<10) + "</body></html>"))
	}, "https://www.tiktok.com/@u/video/7249376077976472833")

	if !errors.Is(err, domain.ErrBlocked) {
		t.Errorf("err = %v, want ErrBlocked", err)
	}
	if n := atomic.LoadInt32(&attempts); n != 3 {
		t.Errorf("attempts = %d, want exactly MaxAttempts (3)", n)
	}
}

func TestStatsDoesNotRetryNotFound(t *testing.T) {
	// A 404 is a settled answer: retrying wastes time and upstream goodwill.
	var attempts int32
	cfg := config.TikTokConfig{UserAgent: "t", On: true, MaxAttempts: 4, RetryBackoff: time.Millisecond}

	_, err := statsViaWithConfig(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusNotFound)
	}, "https://www.tiktok.com/@u/video/7249376077976472833")

	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if n := atomic.LoadInt32(&attempts); n != 1 {
		t.Errorf("attempts = %d, want 1 (no retry on 404)", n)
	}
}

func TestStatsRespectsContextDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><body>" + strings.Repeat("z", 40<<10) + "</body></html>"))
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	client.Transport = rewriteHost{base: srv.URL, next: client.Transport}

	cfg := config.TikTokConfig{UserAgent: "t", On: true, MaxAttempts: 10, RetryBackoff: 2 * time.Second}
	p := New(cfg, client, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := p.Stats(ctx, "https://www.tiktok.com/@u/video/7249376077976472833")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v; backoff ignored the context", elapsed)
	}
}
