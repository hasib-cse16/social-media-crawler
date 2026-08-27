// Package tiktok is the TikTok provider. URL matching is implemented so the
// router already routes TikTok links here; metric fetching lands once a
// TikTok Display API client is provisioned.
package tiktok

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/foodibd/socialstats/internal/config"
	"github.com/foodibd/socialstats/internal/domain"
)

const providerName = "tiktok"

var hostSuffixes = []string{"tiktok.com", "vm.tiktok.com", "vt.tiktok.com"}

type Provider struct {
	client *http.Client
	log    *slog.Logger
	cfg    config.TikTokConfig
}

func New(cfg config.TikTokConfig, client *http.Client, log *slog.Logger) *Provider {
	return &Provider{client: client, log: log.With("provider", providerName), cfg: cfg}
}

func (p *Provider) Platform() domain.Platform { return domain.PlatformTikTok }

func (p *Provider) Match(rawURL string) bool {
	host, ok := hostOf(rawURL)
	if !ok {
		return false
	}
	for _, suffix := range hostSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// Stats will resolve the video id from the URL (following short-link redirects
// for vm./vt. hosts) and call the Display API video/query endpoint for
// view_count, like_count and comment_count.
func (p *Provider) Stats(ctx context.Context, rawURL string) (*domain.VideoStats, error) {
	return nil, fmt.Errorf("%w: tiktok stats", domain.ErrNotImplemented)
}

func hostOf(rawURL string) (string, bool) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", false
	}
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	return strings.TrimPrefix(host, "www."), true
}
