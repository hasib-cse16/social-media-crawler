package meta

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

// Instagram counters are read from the oEmbed-style embed page rather than the
// normal post page. The embed page exists to be rendered inside third-party
// sites, so it is served without a login wall and it still carries the post's
// GraphQL payload — the same shortcode_media object the app consumes. The
// normal post page increasingly answers logged-out clients with a login
// redirect, so it is only used as a fallback for its OpenGraph tags.
//
// The payload is nested inside a JSON string in the page's bootstrap script,
// which is why extraction unescapes before scanning; see scan.go.

// shortcodeMedia mirrors the fields consumed from the embed payload.
type shortcodeMedia struct {
	ID               string `json:"id"`
	Shortcode        string `json:"shortcode"`
	IsVideo          bool   `json:"is_video"`
	TakenAtTimestamp int64  `json:"taken_at_timestamp"`

	// Instagram has used both spellings over time and returns whichever the
	// serving cluster is on; both are read and the larger, non-zero one wins.
	VideoViewCount uint64 `json:"video_view_count"`
	VideoPlayCount uint64 `json:"video_play_count"`

	Owner struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		FullName string `json:"full_name"`
	} `json:"owner"`

	EdgeMediaPreviewLike struct {
		Count uint64 `json:"count"`
	} `json:"edge_media_preview_like"`
	EdgeLikedBy struct {
		Count uint64 `json:"count"`
	} `json:"edge_liked_by"`
	EdgeMediaToComment struct {
		Count uint64 `json:"count"`
	} `json:"edge_media_to_comment"`
	EdgeMediaToParentComment struct {
		Count uint64 `json:"count"`
	} `json:"edge_media_to_parent_comment"`

	EdgeMediaToCaption struct {
		Edges []struct {
			Node struct {
				Text string `json:"text"`
			} `json:"node"`
		} `json:"edges"`
	} `json:"edge_media_to_caption"`
}

func (m shortcodeMedia) caption() string {
	if len(m.EdgeMediaToCaption.Edges) == 0 {
		return ""
	}
	return m.EdgeMediaToCaption.Edges[0].Node.Text
}

// instagramStats fetches and maps one Instagram post.
func (p *Provider) instagramStats(ctx context.Context, ref Ref, rawURL string) (*domain.VideoStats, error) {
	target := embedURL(ref)
	if ref.Short {
		// A share link carries no shortcode, so the fetch itself follows the
		// redirect and the identifiers come out of the payload.
		target = rawURL
	}

	body, err := p.fetchPage(ctx, target)
	if err != nil {
		return nil, err
	}

	media, ok := extractShortcodeMedia(body)
	if !ok {
		// The embed page renders a "Sorry, this page isn't available" stub for
		// posts that are gone, private, or age-restricted. It is short and has
		// no payload, which is exactly the shape we are in here.
		if isInstagramUnavailable(body) {
			return nil, fmt.Errorf("%w: instagram post %s is unavailable, private or removed", domain.ErrNotFound, ref.Key())
		}
		return p.instagramFromPostPage(ctx, ref)
	}

	return mediaToStats(media, ref), nil
}

// mediaToStats maps the embed payload onto the platform-agnostic shape.
func mediaToStats(m shortcodeMedia, ref Ref) *domain.VideoStats {
	code := m.Shortcode
	if code == "" {
		code = ref.Shortcode
	}
	resolved := ref
	resolved.Shortcode = code
	if resolved.Kind == "" || resolved.Short {
		resolved.Kind = KindPost
		if m.IsVideo {
			resolved.Kind = KindReel
		}
	}

	stats := &domain.VideoStats{
		Platform:     domain.PlatformMeta,
		VideoID:      code,
		CanonicalURL: CanonicalURL(resolved),
		Title:        m.caption(),
		ChannelID:    m.Owner.ID,
		ChannelTitle: firstNonEmpty(m.Owner.Username, m.Owner.FullName),
		FetchedAt:    time.Now().UTC(),

		LikeCount:    nonZero(max(m.EdgeMediaPreviewLike.Count, m.EdgeLikedBy.Count)),
		CommentCount: nonZero(max(m.EdgeMediaToComment.Count, m.EdgeMediaToParentComment.Count)),
	}

	// Photos and carousels have no view count at all, and reporting zero there
	// would be a lie rather than a measurement.
	if m.IsVideo {
		stats.ViewCount = nonZero(max(m.VideoViewCount, m.VideoPlayCount))
	}

	if m.TakenAtTimestamp > 0 {
		t := time.Unix(m.TakenAtTimestamp, 0).UTC()
		stats.PublishedAt = &t
	}
	return stats
}

// instagramFromPostPage is the fallback: read the post page's OpenGraph tags.
// It yields rounded counters ("1.2M likes"), so it is only reached when the
// embed payload is missing.
func (p *Provider) instagramFromPostPage(ctx context.Context, ref Ref) (*domain.VideoStats, error) {
	if ref.Shortcode == "" {
		return nil, fmt.Errorf("%w: instagram embed payload was absent and there is no shortcode to retry with", domain.ErrUpstreamFailure)
	}

	body, err := p.fetchPage(ctx, CanonicalURL(ref))
	if err != nil {
		return nil, err
	}

	// The post page sometimes carries the payload even when the embed page did
	// not; prefer it over the rendered figures whenever it is there.
	if media, ok := extractShortcodeMedia(body); ok {
		return mediaToStats(media, ref), nil
	}
	if isInstagramUnavailable(body) {
		return nil, fmt.Errorf("%w: instagram post %s is unavailable, private or removed", domain.ErrNotFound, ref.Shortcode)
	}

	desc := metaContent(body, "og:description")
	counts := parseHumanCounts(desc)
	if len(counts) == 0 {
		return nil, fmt.Errorf("%w: instagram served no post payload and no readable counters (login wall, or the page structure changed)", domain.ErrBlocked)
	}
	p.log.WarnContext(ctx, "instagram counters read from rendered text; abbreviated figures are rounded",
		"shortcode", ref.Shortcode, "description", truncate(desc, 200))

	stats := &domain.VideoStats{
		Platform:     domain.PlatformMeta,
		VideoID:      ref.Shortcode,
		CanonicalURL: CanonicalURL(ref),
		Title:        metaContent(body, "og:title"),
		ChannelTitle: ref.Username,
		FetchedAt:    time.Now().UTC(),
	}
	if v, ok := counts["views"]; ok {
		stats.ViewCount = &v
	}
	if v, ok := counts["likes"]; ok {
		stats.LikeCount = &v
	}
	if v, ok := counts["comments"]; ok {
		stats.CommentCount = &v
	}
	return stats, nil
}

// extractShortcodeMedia pulls the GraphQL media object out of a page.
func extractShortcodeMedia(body []byte) (shortcodeMedia, bool) {
	// The payload appears escaped inside a bootstrap string on most renders and
	// plain on others, so both forms are attempted.
	for _, candidate := range [][]byte{body, unescapeJSONString(body)} {
		obj := findObject(candidate, "shortcode_media")
		if obj == nil {
			obj = findObject(candidate, "xdt_shortcode_media")
		}
		var media shortcodeMedia
		if decodeInto(obj, &media) && (media.ID != "" || media.Shortcode != "") {
			return media, true
		}
	}
	return shortcodeMedia{}, false
}

// isInstagramUnavailable recognises the "page isn't available" stub.
func isInstagramUnavailable(body []byte) bool {
	s := strings.ToLower(string(body))
	for _, marker := range []string{
		"sorry, this page isn't available",
		"the link you followed may be broken",
		// The embed render's own wording, observed live.
		"may be broken, or the post may have been removed",
		"page not found",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// embedURL is the login-free render of a post.
func embedURL(ref Ref) string {
	kind := ref.Kind
	if kind == "" {
		kind = KindPost
	}
	return "https://www.instagram.com/" + string(kind) + "/" + ref.Shortcode + "/embed/captioned/"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// nonZero returns a pointer to n, or nil when n is zero. Meta omits counters by
// sending zero, so zero and "hidden" are indistinguishable; nil is the honest
// answer, matching how domain.VideoStats defines its pointer counters.
func nonZero(n uint64) *uint64 {
	if n == 0 {
		return nil
	}
	return &n
}
