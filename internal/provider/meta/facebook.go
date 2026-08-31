package meta

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

// Facebook is the hardest of the three platforms this service supports.
//
// There is no public, unauthenticated way to read a video's view count for an
// arbitrary URL. The Graph API only answers for objects the token's app has
// been granted, which in practice means videos on Pages you administer, and
// even then `views` is a Page-insights field. Everything else has to come off
// the public page, where Facebook does still embed its Relay payload — but only
// for content it decides to serve logged-out, and it swaps a login wall in
// aggressively.
//
// So the order is: Graph API when a token is configured and the object is
// reachable, public page otherwise, and a clear ErrBlocked when Facebook shows
// a login wall instead of content. The one thing this provider will not do is
// report a zero it did not measure.

// facebookStats fetches one Facebook video, Graph API first when configured.
func (p *Provider) facebookStats(ctx context.Context, ref Ref, rawURL string) (*domain.VideoStats, error) {
	if p.cfg.HasToken() && ref.ID != "" {
		stats, err := p.graphVideo(ctx, ref)
		if err == nil {
			return stats, nil
		}
		// A token that cannot see this object is the normal case for third-party
		// videos, so this is a routine fallback, not an error worth surfacing.
		p.log.DebugContext(ctx, "graph api could not serve this video; falling back to the public page",
			"id", ref.ID, "error", err)
	}

	target := rawURL
	if ref.ID != "" {
		target = CanonicalURL(ref)
	}

	body, err := p.fetchPage(ctx, target)
	if err != nil {
		return nil, err
	}

	if isFacebookLoginWall(body) {
		return nil, fmt.Errorf("%w: facebook served a login wall for %s; this video is not readable without an authenticated session", domain.ErrBlocked, target)
	}

	stats, ok := facebookFromPage(body, ref)
	if !ok {
		return nil, fmt.Errorf("%w: facebook page carried no readable video payload (the video may be private, or the page structure changed)", domain.ErrUpstreamFailure)
	}
	return stats, nil
}

// facebookFromPage maps the Relay payload embedded in a public video page.
func facebookFromPage(body []byte, ref Ref) (*domain.VideoStats, bool) {
	flat := unescapeJSONString(body)

	id := ref.ID
	if id == "" {
		// Short links resolve to a page whose payload names the real id.
		if v := stringAfterKey(flat, "video_id"); v != "" {
			id = v
		} else if v := metaContent(body, "al:android:url"); strings.Contains(v, "/") {
			id = lastNumeric(splitPath(v))
		}
	}
	if id == "" {
		return nil, false
	}

	resolved := ref
	resolved.ID = id

	stats := &domain.VideoStats{
		Platform:     domain.PlatformMeta,
		VideoID:      id,
		CanonicalURL: CanonicalURL(resolved),
		Title:        firstNonEmpty(metaContent(body, "og:title"), stringAfterKey(flat, "title")),
		ChannelTitle: ref.Username,
		FetchedAt:    time.Now().UTC(),

		// Facebook names the play counter several ways depending on which
		// surface rendered the page; the first one present wins.
		ViewCount: firstCount(flat, "video_view_count", "play_count", "post_view_count", "video_play_count"),
	}

	// Reactions, comments and shares live in the feedback object when the page
	// renders one.
	if fb := findObject(flat, "feedback"); fb != nil {
		if v := countInObject(fb, "reaction_count"); v != nil {
			stats.LikeCount = v
		}
		if v := countInObject(fb, "comment_count", "total_comment_count", "comment_rendering_instance"); v != nil {
			stats.CommentCount = v
		}
		if v := countInObject(fb, "share_count", "reshare_count"); v != nil {
			stats.ShareCount = v
		}
	}

	if ts := numberAfterKey(flat, "publish_time"); ts != nil && *ts > 0 {
		t := time.Unix(int64(*ts), 0).UTC()
		stats.PublishedAt = &t
	}

	if owner := findObject(flat, "owner"); owner != nil {
		if v := stringAfterKey(owner, "id"); v != "" {
			stats.ChannelID = v
		}
		if v := stringAfterKey(owner, "name"); v != "" {
			stats.ChannelTitle = v
		}
	}

	// Metadata alone is not a result: the whole point of the call is a counter.
	return stats, stats.ViewCount != nil || stats.LikeCount != nil || stats.CommentCount != nil
}

// countInObject reads `"key":{"count":N}` or `"key":N` for the first key that
// is present, which is how Facebook writes feedback counters.
func countInObject(obj []byte, keys ...string) *uint64 {
	for _, key := range keys {
		if inner := findObject(obj, key); inner != nil {
			for _, field := range []string{"count", "total_count"} {
				if v := numberAfterKey(inner, field); v != nil {
					return v
				}
			}
			continue
		}
		if v := numberAfterKey(obj, key); v != nil {
			return v
		}
	}
	return nil
}

func firstCount(body []byte, keys ...string) *uint64 {
	for _, key := range keys {
		if v := numberAfterKey(body, key); v != nil {
			return v
		}
	}
	return nil
}

// isFacebookLoginWall recognises the interstitial Facebook serves instead of
// content. It is a 200 with a full-size page, so only the markers distinguish it.
func isFacebookLoginWall(body []byte) bool {
	s := strings.ToLower(string(body))
	markers := []string{
		"you must log in to continue",
		"log in to facebook",
		"login_form",
		"/login/?next=",
	}
	hits := 0
	for _, m := range markers {
		if strings.Contains(s, m) {
			hits++
		}
	}
	// A real video page links to /login too, so one marker is not enough;
	// the wall page hits several and carries no video payload.
	return hits >= 2 && !strings.Contains(s, "video_view_count") && !strings.Contains(s, "\"feedback\"")
}
