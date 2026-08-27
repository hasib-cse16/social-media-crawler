// Package meta is the Facebook/Instagram provider. URL matching is implemented
// so the router already routes Meta links here; metric fetching lands once a
// Graph API app + token is provisioned.
package meta

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

const providerName = "meta"

var hostSuffixes = []string{
	"facebook.com",
	"fb.watch",
	"fb.com",
	"instagram.com",
}

type Provider struct {
	client *http.Client
	log    *slog.Logger
	cfg    config.MetaConfig
}

func New(cfg config.MetaConfig, client *http.Client, log *slog.Logger) *Provider {
	return &Provider{client: client, log: log.With("provider", providerName), cfg: cfg}
}

func (p *Provider) Platform() domain.Platform { return domain.PlatformMeta }

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

// Stats will call the Graph API video/media node once credentials exist.
//
// Planned shape:
//
//	GET {base}/{version}/{media-id}?fields=id,permalink_url,video_insights
//	Authorization: Bearer {access token}
//
// then map the response into domain.VideoStats exactly as the YouTube provider does.
func (p *Provider) Stats(ctx context.Context, rawURL string) (*domain.VideoStats, error) {
	if !p.cfg.Enabled() {
		return nil, fmt.Errorf("%w: META_ACCESS_TOKEN is empty", domain.ErrMisconfigured)
	}
	return nil, fmt.Errorf("%w: meta stats", domain.ErrNotImplemented)
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
