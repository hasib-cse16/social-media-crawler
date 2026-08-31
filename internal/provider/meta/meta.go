// Package meta implements domain.Provider for Meta's two video properties,
// Instagram and Facebook.
//
// # Why this provider is shaped the way it is
//
// Meta has no public API that returns metrics for an arbitrary video URL. The
// Graph API only answers for objects the calling app has been granted — in
// practice, media on Pages or Instagram Business accounts the token
// administers — and the public-data endpoints that once served view counts for
// any id were removed in Graph v3.0. oEmbed, the one endpoint that does take a
// public URL, returns embed markup and an author name, never a counter.
//
// So the provider has two paths:
//
//	Graph API   used for Facebook when META_ACCESS_TOKEN is set and the token
//	            can see the object. Exact figures, no anti-bot risk.
//	Public page used otherwise. Instagram counters come from the login-free
//	            embed render, which still carries the post's GraphQL payload;
//	            Facebook counters come from the Relay payload on the public
//	            video page when Facebook serves one.
//
// The consequences, stated plainly:
//
//   - The page path is an undocumented surface. Meta can change it without
//     notice, so extraction fails loudly (ErrUpstreamFailure / ErrBlocked)
//     rather than silently reporting zero.
//   - Facebook shows a login wall to logged-out clients for much of its
//     catalogue. That is reported as ErrBlocked, not as a missing video, since
//     the remedy is different: authenticate, or accept the gap.
//   - Instagram is the more reliable of the two, because the embed render is a
//     supported product surface rather than something we are sneaking past.
//   - Automated access to the public pages is contrary to Meta's Terms of
//     Service. Whether to run it is a decision for whoever deploys this
//     service, which is why it can be turned off with META_PAGE_FETCH=false
//     (leaving only the Graph path) or META_ENABLED=false (leaving nothing).
package meta

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/foodibd/socialstats/internal/config"
	"github.com/foodibd/socialstats/internal/domain"
)

const (
	providerName = "meta"

	// Meta video pages are large; the cap is generous but bounded.
	maxBodyBytes = 8 << 20

	// A 200 this small is a redirect stub or a block, never a rendered page.
	// Instagram's embed page is the smallest real response we expect, at ~30 KB.
	minPlausiblePageBytes = 4 << 10
)

// Provider serves Facebook and Instagram behind one domain.Provider.
type Provider struct {
	client *http.Client
	log    *slog.Logger
	cfg    config.MetaConfig
}

func New(cfg config.MetaConfig, client *http.Client, log *slog.Logger) *Provider {
	return &Provider{client: client, log: log.With("provider", providerName), cfg: cfg}
}

func (p *Provider) Platform() domain.Platform { return domain.PlatformMeta }

func (p *Provider) Match(rawURL string) bool { return IsMetaURL(rawURL) }

// Stats resolves rawURL and fetches the post's public metrics.
func (p *Provider) Stats(ctx context.Context, rawURL string) (*domain.VideoStats, error) {
	if !p.cfg.Enabled() {
		return nil, fmt.Errorf("%w: META_ENABLED is false", domain.ErrMisconfigured)
	}

	ref, err := ParseURL(rawURL)
	if err != nil {
		return nil, err
	}

	switch ref.Network {
	case NetworkInstagram:
		if !p.cfg.PageFetchEnabled() {
			// Instagram has no Graph path for third-party media at all, so
			// disabling page fetches disables Instagram entirely.
			return nil, fmt.Errorf("%w: META_PAGE_FETCH is false and instagram has no api path for third-party posts", domain.ErrMisconfigured)
		}
		return p.instagramStats(ctx, ref, rawURL)

	case NetworkFacebook:
		if !p.cfg.PageFetchEnabled() {
			if !p.cfg.HasToken() {
				return nil, fmt.Errorf("%w: META_PAGE_FETCH is false and META_ACCESS_TOKEN is empty", domain.ErrMisconfigured)
			}
			if ref.ID == "" {
				return nil, fmt.Errorf("%w: %q is a short link, which can only be resolved by fetching it", domain.ErrInvalidURL, rawURL)
			}
			return p.graphVideo(ctx, ref)
		}
		return p.facebookStats(ctx, ref, rawURL)

	default:
		return nil, fmt.Errorf("%w: %s", domain.ErrUnsupported, rawURL)
	}
}

// fetchPage gets a public page, retrying while Meta serves stubs. Meta is less
// aggressive than TikTok here — most failures are permanent login walls rather
// than random challenges — so the default attempt count is lower.
func (p *Provider) fetchPage(ctx context.Context, target string) ([]byte, error) {
	attempts := max(p.cfg.MaxAttempts, 1)

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		body, retryable, err := p.fetchOnce(ctx, target)
		if err == nil {
			if attempt > 1 {
				p.log.InfoContext(ctx, "meta page fetched after retry", "attempt", attempt, "url", target)
			}
			return body, nil
		}
		lastErr = err

		if !retryable || attempt == attempts {
			break
		}

		p.log.DebugContext(ctx, "retrying meta page fetch", "attempt", attempt, "error", err)
		if err := sleepCtx(ctx, time.Duration(attempt)*p.cfg.RetryBackoff); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// fetchOnce performs a single page fetch, reporting whether the failure is
// worth retrying.
func (p *Provider) fetchOnce(ctx context.Context, target string) (body []byte, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, false, fmt.Errorf("%w: build request: %v", domain.ErrUpstreamFailure, err)
	}

	// Meta serves a stripped page to clients that do not look like a browser.
	req.Header.Set("User-Agent", p.cfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	// Instagram's embed render only includes the GraphQL payload when it
	// believes it is being embedded by a real site.
	req.Header.Set("Sec-Fetch-Dest", "iframe")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "cross-site")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("%w: %v", domain.ErrUpstreamFailure, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, true, fmt.Errorf("%w: read body: %v", domain.ErrUpstreamFailure, err)
	}

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, false, fmt.Errorf("%w: meta returned %d for %s", domain.ErrNotFound, resp.StatusCode, target)
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, true, fmt.Errorf("%w: meta returned 429", domain.ErrRateLimited)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, false, fmt.Errorf("%w: meta returned %d; this content needs an authenticated session", domain.ErrBlocked, resp.StatusCode)
	case resp.StatusCode >= 500:
		return nil, true, domain.NewUpstreamError(providerName, resp.StatusCode, truncate(string(raw), 256), domain.ErrUpstreamFailure)
	case resp.StatusCode != http.StatusOK:
		return nil, false, domain.NewUpstreamError(providerName, resp.StatusCode, truncate(string(raw), 256), domain.ErrUpstreamFailure)
	}

	if len(raw) < minPlausiblePageBytes {
		// Too small to be a render. Unlike TikTok's hard block this is usually a
		// transient edge response, so one more attempt is worth making.
		return nil, true, fmt.Errorf("%w: meta served a %d byte stub", domain.ErrBlocked, len(raw))
	}
	return raw, false, nil
}

// get performs a single Graph API call and returns the body and status. Graph
// puts its own error object in the body of a 400, so the status alone is not
// enough to classify the failure and both are returned.
func (p *Provider) get(ctx context.Context, endpoint string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: build request: %v", domain.ErrUpstreamFailure, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", domain.ErrUpstreamFailure, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("%w: read body: %v", domain.ErrUpstreamFailure, err)
	}
	return body, resp.StatusCode, nil
}

// sleepCtx waits for d unless ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
