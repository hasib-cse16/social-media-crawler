package domain

import "context"

// Provider is implemented once per platform. Adding a platform means adding a
// package that satisfies this interface and registering it.
type Provider interface {
	// Platform returns the platform this provider serves.
	Platform() Platform

	// Match reports whether rawURL belongs to this platform. It must be cheap
	// and must not perform network I/O.
	Match(rawURL string) bool

	// Identify extracts the platform-native identity of rawURL without network
	// I/O, so a video can be recorded and tracked before — or instead of — a
	// successful fetch.
	//
	// It returns ErrNeedsResolution for short links, whose id only becomes
	// knowable by following a redirect. That is not a failure: it tells the
	// caller the identity has to come from a fetch instead.
	Identify(rawURL string) (VideoRef, error)

	// Stats resolves rawURL and fetches its public metrics.
	Stats(ctx context.Context, rawURL string) (*VideoStats, error)
}

// VideoRef is a video's identity on its platform: enough to store it, dedupe it
// and link to it, without any of its metrics.
type VideoRef struct {
	Platform Platform

	// VideoID is the platform's own id — a YouTube id, a TikTok item id, an
	// Instagram shortcode. Together with Platform it is the natural key.
	VideoID string

	// CanonicalURL is the normalised watch URL, so that the several forms a
	// user might paste all resolve to one stored address.
	CanonicalURL string
}

// Ref extracts the identity from a fetched result.
func (v *VideoStats) Ref() VideoRef {
	return VideoRef{
		Platform:     v.Platform,
		VideoID:      v.VideoID,
		CanonicalURL: v.CanonicalURL,
	}
}
