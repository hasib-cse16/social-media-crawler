package domain

import "context"

// Provider is implemented once per platform. Adding Meta or TikTok means
// adding a package that satisfies this interface and registering it.
type Provider interface {
	// Platform returns the platform this provider serves.
	Platform() Platform

	// Match reports whether rawURL belongs to this platform. It must be cheap
	// and must not perform network I/O.
	Match(rawURL string) bool

	// Stats resolves rawURL and fetches its public metrics.
	Stats(ctx context.Context, rawURL string) (*VideoStats, error)
}
