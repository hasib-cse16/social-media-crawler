package meta

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

// padding pushes a fixture past minPlausiblePageBytes so it is not mistaken for
// a stub response.
var padding = strings.Repeat("<!-- markup -->", 400)

// embedPage wraps a shortcode_media payload the way Instagram's embed render
// does: escaped, inside a bootstrap string, in an otherwise ordinary page.
func embedPage(media string) string {
	escaped := strings.ReplaceAll(media, `"`, `\"`)
	return "<!DOCTYPE html><html><body>" + padding +
		`<script type="text/javascript">requireLazy(["ScheduledServerJS"],function(h){h.handle({"contextJSON":"{\"gql_data\":{\"shortcode_media\":` +
		escaped + `}}"})})</script></body></html>`
}

const okMedia = `{
  "id": "3245678901234567890",
  "shortcode": "Cx1y2z3AbCd",
  "is_video": true,
  "taken_at_timestamp": 1687876902,
  "video_view_count": 220500,
  "owner": {"id": "787132", "username": "natgeo", "full_name": "National Geographic"},
  "edge_media_preview_like": {"count": 8702},
  "edge_media_to_comment": {"count": 113},
  "edge_media_to_caption": {"edges": [{"node": {"text": "A caption"}}]}
}`

func testConfig() config.MetaConfig {
	return config.MetaConfig{
		On:          true,
		PageFetch:   true,
		UserAgent:   "test-agent",
		MaxAttempts: 1,
		BaseURL:     "https://graph.facebook.com",
		APIVersion:  "v21.0",
	}
}

// statsVia points the provider at a test server, since Stats builds real
// instagram.com / facebook.com URLs internally.
func statsVia(t *testing.T, cfg config.MetaConfig, handler http.HandlerFunc, rawURL string) (*domain.VideoStats, error) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client := srv.Client()
	client.Transport = rewriteHost{base: srv.URL, next: client.Transport}

	p := New(cfg, client, slog.New(slog.DiscardHandler))
	return p.Stats(context.Background(), rawURL)
}

// rewriteHost sends every request to the test server, whatever host was asked for.
type rewriteHost struct {
	base string
	next http.RoundTripper
}

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(r.base, "http://")
	return r.next.RoundTrip(req)
}

func TestInstagramStatsFromEmbedPage(t *testing.T) {
	var gotPath, gotUA string
	got, err := statsVia(t, testConfig(), func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotUA = r.URL.Path, r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(embedPage(okMedia)))
	}, "https://www.instagram.com/reel/Cx1y2z3AbCd/")

	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if gotPath != "/reel/Cx1y2z3AbCd/embed/captioned/" {
		t.Errorf("requested path = %q, want the login-free embed render", gotPath)
	}
	if gotUA != "test-agent" {
		t.Errorf("User-Agent = %q, want the configured agent", gotUA)
	}
	if got.Platform != domain.PlatformMeta {
		t.Errorf("Platform = %q, want meta", got.Platform)
	}
	if got.VideoID != "Cx1y2z3AbCd" {
		t.Errorf("VideoID = %q, want the shortcode", got.VideoID)
	}
	if got.CanonicalURL != "https://www.instagram.com/reel/Cx1y2z3AbCd/" {
		t.Errorf("CanonicalURL = %q", got.CanonicalURL)
	}
	if got.ViewCount == nil || *got.ViewCount != 220500 {
		t.Errorf("ViewCount = %v, want 220500", got.ViewCount)
	}
	if got.LikeCount == nil || *got.LikeCount != 8702 {
		t.Errorf("LikeCount = %v, want 8702", got.LikeCount)
	}
	if got.CommentCount == nil || *got.CommentCount != 113 {
		t.Errorf("CommentCount = %v, want 113", got.CommentCount)
	}
	if got.ChannelTitle != "natgeo" || got.ChannelID != "787132" {
		t.Errorf("channel = %q/%q, want natgeo/787132", got.ChannelTitle, got.ChannelID)
	}
	if got.Title != "A caption" {
		t.Errorf("Title = %q, want the caption", got.Title)
	}
	if got.PublishedAt == nil || got.PublishedAt.Year() != 2023 {
		t.Errorf("PublishedAt = %v", got.PublishedAt)
	}
}

func TestInstagramPhotoHasNoViewCount(t *testing.T) {
	media := strings.Replace(okMedia, `"is_video": true`, `"is_video": false`, 1)
	got, err := statsVia(t, testConfig(), func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(embedPage(media)))
	}, "https://www.instagram.com/p/Cx1y2z3AbCd/")

	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.ViewCount != nil {
		t.Errorf("ViewCount = %v, want nil for a photo post", *got.ViewCount)
	}
	if got.LikeCount == nil {
		t.Error("LikeCount should still be reported for a photo post")
	}
}

func TestInstagramZeroCountersAreOmitted(t *testing.T) {
	media := strings.Replace(okMedia, `"edge_media_to_comment": {"count": 113}`, `"edge_media_to_comment": {"count": 0}`, 1)
	got, err := statsVia(t, testConfig(), func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(embedPage(media)))
	}, "https://www.instagram.com/reel/Cx1y2z3AbCd/")

	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.CommentCount != nil {
		t.Errorf("CommentCount = %v; zero is indistinguishable from hidden and must be omitted", *got.CommentCount)
	}
}

func TestInstagramUnavailablePostIsNotFound(t *testing.T) {
	_, err := statsVia(t, testConfig(), func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><body>" + padding + "Sorry, this page isn't available.</body></html>"))
	}, "https://www.instagram.com/reel/Cx1y2z3AbCd/")

	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestInstagramRemovedPostStubIsNotFound(t *testing.T) {
	// The wording the embed render actually uses for a post it will not serve.
	stub := "<html><body>" + padding +
		"<div>The link to this photo or video may be broken, or the post may have been removed. Visit Instagram</div></body></html>"

	_, err := statsVia(t, testConfig(), func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(stub))
	}, "https://www.instagram.com/reel/Cx1y2z3AbCd/")

	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestInstagramFallsBackToPostPage(t *testing.T) {
	var paths []string
	got, err := statsVia(t, testConfig(), func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "/embed/") {
			// A payload-free embed render: the fallback should kick in.
			_, _ = w.Write([]byte("<html><body>" + padding + "</body></html>"))
			return
		}
		_, _ = w.Write([]byte(`<html><head>` +
			`<meta property="og:title" content="natgeo on Instagram">` +
			`<meta property="og:description" content="1.2M views, 40,102 likes, 733 comments - natgeo on June 27, 2023">` +
			`</head><body>` + padding + `</body></html>`))
	}, "https://www.instagram.com/reel/Cx1y2z3AbCd/")

	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(paths) != 2 || paths[1] != "/reel/Cx1y2z3AbCd/" {
		t.Errorf("request paths = %v, want the embed render then the post page", paths)
	}
	if got.ViewCount == nil || *got.ViewCount != 1200000 {
		t.Errorf("ViewCount = %v, want the rounded 1.2M", got.ViewCount)
	}
	if got.LikeCount == nil || *got.LikeCount != 40102 {
		t.Errorf("LikeCount = %v, want the exact 40102", got.LikeCount)
	}
	if got.CommentCount == nil || *got.CommentCount != 733 {
		t.Errorf("CommentCount = %v, want 733", got.CommentCount)
	}
}

func TestInstagramNoPayloadAndNoCountersIsBlocked(t *testing.T) {
	_, err := statsVia(t, testConfig(), func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><head><title>Login</title></head><body>" + padding + "</body></html>"))
	}, "https://www.instagram.com/reel/Cx1y2z3AbCd/")

	if !errors.Is(err, domain.ErrBlocked) {
		t.Errorf("error = %v, want ErrBlocked", err)
	}
}

func TestFacebookStatsFromPublicPage(t *testing.T) {
	page := `<html><head><meta property="og:title" content="A public video"></head><body>` + padding +
		`<script>requireLazy({"video_id":"1234567890","publish_time":1687876902,` +
		`"owner":{"id":"555","name":"National Geographic"},"video_view_count":98765,` +
		`"feedback":{"reaction_count":{"count":4321},"comment_count":{"total_count":42},"share_count":{"count":17}}})</script>` +
		`</body></html>`

	got, err := statsVia(t, testConfig(), func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(page))
	}, "https://www.facebook.com/natgeo/videos/a-slug/1234567890/")

	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.VideoID != "1234567890" {
		t.Errorf("VideoID = %q", got.VideoID)
	}
	if got.ViewCount == nil || *got.ViewCount != 98765 {
		t.Errorf("ViewCount = %v, want 98765", got.ViewCount)
	}
	if got.LikeCount == nil || *got.LikeCount != 4321 {
		t.Errorf("LikeCount = %v, want 4321", got.LikeCount)
	}
	if got.CommentCount == nil || *got.CommentCount != 42 {
		t.Errorf("CommentCount = %v, want 42", got.CommentCount)
	}
	if got.ShareCount == nil || *got.ShareCount != 17 {
		t.Errorf("ShareCount = %v, want 17", got.ShareCount)
	}
	if got.ChannelTitle != "National Geographic" || got.ChannelID != "555" {
		t.Errorf("channel = %q/%q", got.ChannelTitle, got.ChannelID)
	}
	if got.Title != "A public video" {
		t.Errorf("Title = %q", got.Title)
	}
}

func TestFacebookLoginWallIsBlocked(t *testing.T) {
	wall := `<html><body>` + padding +
		`<div>You must log in to continue.</div><form id="login_form" action="/login/?next=%2Fwatch"></form>` +
		`</body></html>`

	_, err := statsVia(t, testConfig(), func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(wall))
	}, "https://www.facebook.com/watch/?v=1234567890")

	if !errors.Is(err, domain.ErrBlocked) {
		t.Errorf("error = %v, want ErrBlocked", err)
	}
}

func TestFacebookPageWithoutCountersIsUpstreamFailure(t *testing.T) {
	_, err := statsVia(t, testConfig(), func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head><meta property="og:title" content="Something"></head><body>` + padding + `</body></html>`))
	}, "https://www.facebook.com/watch/?v=1234567890")

	if !errors.Is(err, domain.ErrUpstreamFailure) {
		t.Errorf("error = %v, want ErrUpstreamFailure", err)
	}
}

func TestFacebookPrefersGraphAPIWhenTokenIsSet(t *testing.T) {
	var graphCalls, pageCalls atomic.Int32

	cfg := testConfig()
	cfg.AccessToken = "token-123"

	got, err := statsVia(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v21.0/") {
			graphCalls.Add(1)
			if r.URL.Query().Get("access_token") != "token-123" {
				t.Errorf("graph call did not carry the access token: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"id":"1234567890","title":"Graph title","created_time":"2023-06-27T12:01:42+0000",
			  "permalink_url":"/natgeo/videos/1234567890/","views":5555,"from":{"id":"555","name":"National Geographic"},
			  "likes":{"summary":{"total_count":11}},"comments":{"summary":{"total_count":3}}}`))
			return
		}
		pageCalls.Add(1)
		_, _ = w.Write([]byte("<html><body>" + padding + "</body></html>"))
	}, "https://www.facebook.com/watch/?v=1234567890")

	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if graphCalls.Load() != 1 || pageCalls.Load() != 0 {
		t.Errorf("graph calls = %d, page calls = %d; want the graph path only", graphCalls.Load(), pageCalls.Load())
	}
	if got.ViewCount == nil || *got.ViewCount != 5555 {
		t.Errorf("ViewCount = %v, want 5555", got.ViewCount)
	}
	if got.CanonicalURL != "https://www.facebook.com/natgeo/videos/1234567890/" {
		t.Errorf("CanonicalURL = %q, want the absolute permalink", got.CanonicalURL)
	}
	if got.PublishedAt == nil || got.PublishedAt.Format(time.RFC3339) != "2023-06-27T12:01:42Z" {
		t.Errorf("PublishedAt = %v", got.PublishedAt)
	}
}

func TestFacebookFallsBackToPageWhenGraphRefuses(t *testing.T) {
	var pageCalls atomic.Int32

	cfg := testConfig()
	cfg.AccessToken = "token-123"

	got, err := statsVia(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v21.0/") {
			// "Unsupported get request" is what Graph says for an object the
			// token cannot see, which is the norm for third-party videos.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported get request.","type":"GraphMethodException","code":100}}`))
			return
		}
		pageCalls.Add(1)
		_, _ = w.Write([]byte(`<html><body>` + padding + `<script>({"video_view_count":777})</script></body></html>`))
	}, "https://www.facebook.com/watch/?v=1234567890")

	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if pageCalls.Load() != 1 {
		t.Errorf("page calls = %d, want 1 fallback fetch", pageCalls.Load())
	}
	if got.ViewCount == nil || *got.ViewCount != 777 {
		t.Errorf("ViewCount = %v, want 777", got.ViewCount)
	}
}

func TestDisabledProviderIsMisconfigured(t *testing.T) {
	cfg := testConfig()
	cfg.On = false

	p := New(cfg, http.DefaultClient, slog.New(slog.DiscardHandler))
	_, err := p.Stats(context.Background(), "https://www.instagram.com/reel/Cx1y2z3AbCd/")
	if !errors.Is(err, domain.ErrMisconfigured) {
		t.Errorf("error = %v, want ErrMisconfigured", err)
	}
}

func TestPageFetchDisabledBlocksInstagramButNotGraph(t *testing.T) {
	cfg := testConfig()
	cfg.PageFetch = false

	p := New(cfg, http.DefaultClient, slog.New(slog.DiscardHandler))
	if _, err := p.Stats(context.Background(), "https://www.instagram.com/reel/Cx1y2z3AbCd/"); !errors.Is(err, domain.ErrMisconfigured) {
		t.Errorf("instagram error = %v, want ErrMisconfigured", err)
	}
	if _, err := p.Stats(context.Background(), "https://www.facebook.com/watch/?v=1234567890"); !errors.Is(err, domain.ErrMisconfigured) {
		t.Errorf("facebook without a token error = %v, want ErrMisconfigured", err)
	}

	cfg.AccessToken = "token-123"
	withToken := New(cfg, http.DefaultClient, slog.New(slog.DiscardHandler))
	if _, err := withToken.Stats(context.Background(), "https://fb.watch/AbCdEfGh/"); !errors.Is(err, domain.ErrInvalidURL) {
		t.Errorf("short link without page fetches error = %v, want ErrInvalidURL", err)
	}
}

func TestUpstreamStatusMapping(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   error
	}{
		{"not found", http.StatusNotFound, domain.ErrNotFound},
		{"gone", http.StatusGone, domain.ErrNotFound},
		{"rate limited", http.StatusTooManyRequests, domain.ErrRateLimited},
		{"forbidden", http.StatusForbidden, domain.ErrBlocked},
		{"server error", http.StatusBadGateway, domain.ErrUpstreamFailure},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := statsVia(t, testConfig(), func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}, "https://www.instagram.com/reel/Cx1y2z3AbCd/")

			if !errors.Is(err, tc.want) {
				t.Errorf("status %d gave %v, want %v", tc.status, err, tc.want)
			}
		})
	}
}

func TestRetriesTransientStub(t *testing.T) {
	var calls atomic.Int32

	cfg := testConfig()
	cfg.MaxAttempts = 3
	cfg.RetryBackoff = time.Millisecond

	got, err := statsVia(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte("stub")) // under minPlausiblePageBytes
			return
		}
		_, _ = w.Write([]byte(embedPage(okMedia)))
	}, "https://www.instagram.com/reel/Cx1y2z3AbCd/")

	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("attempts = %d, want the stub retried once", calls.Load())
	}
	if got.ViewCount == nil {
		t.Error("ViewCount missing after a successful retry")
	}
}

func TestGraphErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		code int
		want error
	}{
		{"unsupported get", 100, domain.ErrNotFound},
		{"no permission", 200, domain.ErrNotFound},
		{"rate limited", 4, domain.ErrRateLimited},
		{"bad token", 190, domain.ErrMisconfigured},
		{"unknown", 1, domain.ErrUpstreamFailure},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := graphSentinel(&graphError{Code: tc.code, Message: "boom"})
			if !errors.Is(err, tc.want) {
				t.Errorf("code %d gave %v, want %v", tc.code, err, tc.want)
			}
		})
	}
}

func TestMatchAndPlatform(t *testing.T) {
	p := New(testConfig(), http.DefaultClient, slog.New(slog.DiscardHandler))
	if p.Platform() != domain.PlatformMeta {
		t.Errorf("Platform = %q", p.Platform())
	}
	if !p.Match("https://www.instagram.com/reel/Cx1y2z3AbCd/") {
		t.Error("Match should claim instagram urls")
	}
	if p.Match("https://www.tiktok.com/@u/video/7249376077976472833") {
		t.Error("Match should not claim tiktok urls")
	}
}

func TestContextCancellationIsPropagated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	client.Transport = rewriteHost{base: srv.URL, next: client.Transport}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	p := New(testConfig(), client, slog.New(slog.DiscardHandler))
	if _, err := p.Stats(ctx, "https://www.instagram.com/reel/Cx1y2z3AbCd/"); err == nil {
		t.Fatal("Stats should fail when the context expires")
	}
}
