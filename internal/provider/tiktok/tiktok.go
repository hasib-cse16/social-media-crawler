// Package tiktok implements domain.Provider for TikTok.
//
// # Why this provider scrapes
//
// TikTok has no public API that returns metrics for an arbitrary video URL.
// The official Display API (/v2/video/query/) only returns videos belonging to
// the user who granted an OAuth token, and the Research API is restricted to
// approved academic applicants. Since this service must answer "here is a URL,
// give me its view count", the only available source is the public video page,
// which embeds its own state as JSON in a __UNIVERSAL_DATA_FOR_REHYDRATION__
// script tag.
//
// That has consequences worth stating plainly:
//
//   - It is an undocumented surface. TikTok can change or remove it without
//     notice, so the parser fails loudly (ErrUpstreamFailure) rather than
//     silently reporting zero.
//   - It is subject to anti-bot measures, probabilistically. Roughly a third of
//     requests get a challenge page instead of the video page, regardless of
//     headers, HTTP version or TLS settings — measured at ~60-70% success per
//     attempt from a single IP. Retrying usually succeeds, so Stats retries up
//     to TIKTOK_MAX_ATTEMPTS times before giving up with ErrBlocked, which
//     takes the observed success rate to ~90%.
//   - Automated access is contrary to TikTok's Terms of Service. Whether to run
//     it is a decision for whoever deploys this service, which is why the
//     provider can be disabled with TIKTOK_ENABLED=false.
//
// If you are granted Display API access for your own account's videos, add it
// as a preferred path inside Stats and keep page extraction as the fallback.
package tiktok

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/foodibd/socialstats/internal/config"
	"github.com/foodibd/socialstats/internal/domain"
)

const (
	providerName = "tiktok"

	// Video pages are ~400 KB; the cap is generous but bounded.
	maxBodyBytes = 8 << 20

	// A page this small is a block or challenge stub, never a video page.
	minPlausiblePageBytes = 10 << 10
)

// rehydrationRe pulls out the embedded state blob. The id attribute is matched
// loosely because attribute order and spacing in the tag are not stable.
var rehydrationRe = regexp.MustCompile(`(?s)<script[^>]+id="__UNIVERSAL_DATA_FOR_REHYDRATION__"[^>]*>(.*?)</script>`)

// Provider fetches TikTok metrics by reading the public video page's own state.
type Provider struct {
	client *http.Client
	log    *slog.Logger
	cfg    config.TikTokConfig
}

func New(cfg config.TikTokConfig, client *http.Client, log *slog.Logger) *Provider {
	return &Provider{client: client, log: log.With("provider", providerName), cfg: cfg}
}

func (p *Provider) Platform() domain.Platform { return domain.PlatformTikTok }

func (p *Provider) Match(rawURL string) bool { return IsTikTokURL(rawURL) }

// pageState mirrors the fields consumed from the embedded page state.
type pageState struct {
	Scope struct {
		VideoDetail struct {
			StatusCode int    `json:"statusCode"`
			StatusMsg  string `json:"statusMsg"`
			ItemInfo   struct {
				ItemStruct struct {
					ID         string    `json:"id"`
					Desc       string    `json:"desc"`
					CreateTime flexInt64 `json:"createTime"`
					Author     struct {
						ID       string `json:"id"`
						UniqueID string `json:"uniqueId"`
						Nickname string `json:"nickname"`
					} `json:"author"`
					// stats and statsV2 carry the same counters; statsV2 sends them
					// as strings, which is preferred because counts above 2^53
					// would lose precision as JSON numbers. Both are decoded
					// flexibly since TikTok mixes numbers and strings within a
					// single object.
					Stats struct {
						PlayCount    flexUint64 `json:"playCount"`
						DiggCount    flexUint64 `json:"diggCount"`
						CommentCount flexUint64 `json:"commentCount"`
						ShareCount   flexUint64 `json:"shareCount"`
						CollectCount flexUint64 `json:"collectCount"`
					} `json:"stats"`
					StatsV2 struct {
						PlayCount    flexUint64 `json:"playCount"`
						DiggCount    flexUint64 `json:"diggCount"`
						CommentCount flexUint64 `json:"commentCount"`
						ShareCount   flexUint64 `json:"shareCount"`
						CollectCount flexUint64 `json:"collectCount"`
					} `json:"statsV2"`
				} `json:"itemStruct"`
			} `json:"itemInfo"`
		} `json:"webapp.video-detail"`
	} `json:"__DEFAULT_SCOPE__"`
}

// TikTok status codes seen on the video-detail scope.
const (
	statusOK            = 0
	statusItemNotFound  = 10204
	statusItemInvisible = 10216 // private, or restricted in this region
	statusAuthorPrivate = 10222
	statusItemDeleted   = 10217
	statusCrossBorder   = 10231
)

// Stats resolves rawURL and reads the video's public counters.
func (p *Provider) Stats(ctx context.Context, rawURL string) (*domain.VideoStats, error) {
	if !p.cfg.Enabled() {
		return nil, fmt.Errorf("%w: TIKTOK_ENABLED is false", domain.ErrMisconfigured)
	}

	ref, err := ParseURL(rawURL)
	if err != nil {
		return nil, err
	}

	// Short links carry no id, so the page fetch itself resolves the redirect
	// and the id is read from the page state.
	target := rawURL
	if ref.VideoID != "" {
		target = CanonicalURL(ref.Username, ref.VideoID)
	}

	body, err := p.fetchPage(ctx, target)
	if err != nil {
		return nil, err
	}

	state, err := extractState(body)
	if err != nil {
		return nil, err
	}

	detail := state.Scope.VideoDetail
	if detail.StatusCode != statusOK {
		return nil, statusError(detail.StatusCode, detail.StatusMsg)
	}

	item := detail.ItemInfo.ItemStruct
	if item.ID == "" {
		return nil, fmt.Errorf("%w: page state carried no video", domain.ErrUpstreamFailure)
	}

	username := item.Author.UniqueID
	if username == "" {
		username = ref.Username
	}

	stats := &domain.VideoStats{
		Platform:     domain.PlatformTikTok,
		VideoID:      item.ID,
		CanonicalURL: CanonicalURL(username, item.ID),
		Title:        item.Desc,
		ChannelID:    item.Author.ID,
		ChannelTitle: item.Author.Nickname,
		FetchedAt:    time.Now().UTC(),

		// statsV2 first, falling back to the stats block.
		ViewCount:    pickCount(item.StatsV2.PlayCount, item.Stats.PlayCount),
		LikeCount:    pickCount(item.StatsV2.DiggCount, item.Stats.DiggCount),
		CommentCount: pickCount(item.StatsV2.CommentCount, item.Stats.CommentCount),
		ShareCount:   pickCount(item.StatsV2.ShareCount, item.Stats.ShareCount),
		SaveCount:    pickCount(item.StatsV2.CollectCount, item.Stats.CollectCount),
	}
	if item.CreateTime.Set && item.CreateTime.Value > 0 {
		t := time.Unix(item.CreateTime.Value, 0).UTC()
		stats.PublishedAt = &t
	}
	return stats, nil
}

// fetchPage gets the video page, retrying while TikTok serves challenge pages.
func (p *Provider) fetchPage(ctx context.Context, target string) ([]byte, error) {
	attempts := p.cfg.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		body, retryable, err := p.fetchOnce(ctx, target)
		if err == nil {
			if attempt > 1 {
				p.log.InfoContext(ctx, "tiktok page fetched after retry", "attempt", attempt, "url", target)
			}
			return body, nil
		}
		lastErr = err

		if !retryable || attempt == attempts {
			break
		}

		p.log.DebugContext(ctx, "retrying tiktok page fetch", "attempt", attempt, "error", err)
		if err := sleepCtx(ctx, time.Duration(attempt)*p.cfg.RetryBackoff); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// fetchOnce performs a single page fetch. It reports whether the failure is
// worth retrying: challenge pages are, genuine 404s and decode failures are not.
func (p *Provider) fetchOnce(ctx context.Context, target string) (body []byte, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, false, fmt.Errorf("%w: build request: %v", domain.ErrUpstreamFailure, err)
	}

	// TikTok serves a stub page to clients that do not look like a browser.
	req.Header.Set("User-Agent", p.cfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Upgrade-Insecure-Requests", "1")

	resp, err := p.client.Do(req)
	if err != nil {
		// Transport-level failures are usually transient.
		return nil, true, fmt.Errorf("%w: %v", domain.ErrUpstreamFailure, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, true, fmt.Errorf("%w: read body: %v", domain.ErrUpstreamFailure, err)
	}

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, false, fmt.Errorf("%w: tiktok returned 404 for %s", domain.ErrNotFound, target)
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, true, fmt.Errorf("%w: tiktok returned 429", domain.ErrRateLimited)
	case resp.StatusCode >= 500:
		return nil, true, domain.NewUpstreamError(providerName, resp.StatusCode, snippet(raw), domain.ErrUpstreamFailure)
	case resp.StatusCode != http.StatusOK:
		return nil, false, domain.NewUpstreamError(providerName, resp.StatusCode, snippet(raw), domain.ErrUpstreamFailure)
	}

	// A 200 far too small to be a video page is a hard block: TikTok has decided
	// this client is a bot, and every immediate retry gets the same stub while
	// deepening the block. Fail fast instead of hammering. Observed after
	// closely-spaced retries; it clears on its own within a minute or two.
	if len(raw) < minPlausiblePageBytes {
		return nil, false, fmt.Errorf("%w: tiktok served a %d byte block stub; back off before retrying", domain.ErrBlocked, len(raw))
	}

	// A full-size page without the state script is the soft challenge page
	// TikTok serves to roughly a third of requests at random. This one IS worth
	// retrying: a fresh connection usually gets the real page.
	if !rehydrationRe.Match(raw) {
		return nil, true, fmt.Errorf("%w: tiktok served a challenge page (%d bytes, no state script)", domain.ErrBlocked, len(raw))
	}

	return raw, false, nil
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

// extractState pulls the embedded JSON state out of the page HTML.
func extractState(body []byte) (*pageState, error) {
	match := rehydrationRe.FindSubmatch(body)
	if match == nil {
		// fetchOnce only returns pages containing the script, so reaching here
		// means the markup changed shape.
		return nil, fmt.Errorf("%w: page state script not found (tiktok may have changed its page structure)", domain.ErrUpstreamFailure)
	}

	var state pageState
	if err := json.Unmarshal(match[1], &state); err != nil {
		return nil, fmt.Errorf("%w: decode page state: %v", domain.ErrUpstreamFailure, err)
	}
	return &state, nil
}

func statusError(code int, msg string) error {
	if msg == "" {
		msg = "no message"
	}
	switch code {
	case statusItemNotFound, statusItemDeleted:
		return fmt.Errorf("%w: tiktok status %d (%s)", domain.ErrNotFound, code, msg)
	case statusItemInvisible, statusAuthorPrivate, statusCrossBorder:
		// Private, region-blocked or author-restricted: indistinguishable from
		// missing to an unauthenticated caller, so report it as not found.
		return fmt.Errorf("%w: tiktok status %d (%s)", domain.ErrNotFound, code, msg)
	default:
		return fmt.Errorf("%w: tiktok status %d (%s)", domain.ErrUpstreamFailure, code, msg)
	}
}

// pickCount prefers the statsV2 counter and falls back to the stats block.
func pickCount(preferred, fallback flexUint64) *uint64 {
	if v := preferred.ptr(); v != nil {
		return v
	}
	return fallback.ptr()
}

func snippet(body []byte) string {
	const limit = 256
	s := strings.TrimSpace(string(body[:min(len(body), limit)]))
	if len(body) > limit {
		s += "..."
	}
	return s
}

// Identify extracts the item id from rawURL.
//
// Short links (vm.tiktok.com, vt.tiktok.com, /t/<token>) carry no id, so they
// report ErrNeedsResolution rather than guessing: the id only exists after the
// redirect has been followed.
func (p *Provider) Identify(rawURL string) (domain.VideoRef, error) {
	ref, err := ParseURL(rawURL)
	if err != nil {
		return domain.VideoRef{}, err
	}
	if ref.VideoID == "" {
		return domain.VideoRef{}, fmt.Errorf("%w: %q is a tiktok short link", domain.ErrNeedsResolution, rawURL)
	}
	return domain.VideoRef{
		Platform:     domain.PlatformTikTok,
		VideoID:      ref.VideoID,
		CanonicalURL: CanonicalURL(ref.Username, ref.VideoID),
	}, nil
}
