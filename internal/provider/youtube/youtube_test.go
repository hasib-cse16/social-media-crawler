package youtube

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/foodibd/socialstats/internal/config"
	"github.com/foodibd/socialstats/internal/domain"
)

func newTestProvider(t *testing.T, handler http.HandlerFunc) (*Provider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p, err := New(config.YouTubeConfig{APIKey: "test-key", BaseURL: srv.URL}, srv.Client(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, srv
}

func TestStatsSuccess(t *testing.T) {
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("id"); got != "dQw4w9WgXcQ" {
			t.Errorf("id param = %q", got)
		}
		if got := r.URL.Query().Get("key"); got != "test-key" {
			t.Errorf("key param = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"dQw4w9WgXcQ",
			"snippet":{"title":"Never Gonna Give You Up","channelId":"UC38","channelTitle":"Rick","publishedAt":"2009-10-25T06:57:33Z"},
			"statistics":{"viewCount":"1500000000","likeCount":"18000000","commentCount":"2200000"}}]}`))
	})

	got, err := p.Stats(context.Background(), "https://youtu.be/dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.ViewCount == nil || *got.ViewCount != 1500000000 {
		t.Errorf("ViewCount = %v, want 1500000000", got.ViewCount)
	}
	if got.Title != "Never Gonna Give You Up" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.CanonicalURL != "https://www.youtube.com/watch?v=dQw4w9WgXcQ" {
		t.Errorf("CanonicalURL = %q", got.CanonicalURL)
	}
	if got.PublishedAt == nil {
		t.Error("PublishedAt = nil, want parsed timestamp")
	}
}

func TestStatsHiddenCountsStayNil(t *testing.T) {
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"id":"dQw4w9WgXcQ","statistics":{"viewCount":"10"}}]}`))
	})

	got, err := p.Stats(context.Background(), "https://youtu.be/dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.LikeCount != nil {
		t.Errorf("LikeCount = %v, want nil for a hidden counter", *got.LikeCount)
	}
}

func TestStatsErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{"empty items is not found", 200, `{"items":[]}`, domain.ErrNotFound},
		{"quota exhausted is rate limited", 403, `{"error":{"code":403}}`, domain.ErrRateLimited},
		{"too many requests", 429, `{}`, domain.ErrRateLimited},
		{"server error is upstream failure", 500, `boom`, domain.ErrUpstreamFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := p.Stats(context.Background(), "https://youtu.be/dQw4w9WgXcQ")
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Stats error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestNewRequiresAPIKey(t *testing.T) {
	_, err := New(config.YouTubeConfig{}, http.DefaultClient, slog.New(slog.DiscardHandler))
	if !errors.Is(err, domain.ErrMisconfigured) {
		t.Errorf("New without key = %v, want ErrMisconfigured", err)
	}
}
