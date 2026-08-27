// Package stats is the application layer: it coordinates provider lookup and
// caching, and is the only thing HTTP handlers talk to.
package stats

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

// Resolver is satisfied by provider.Registry.
type Resolver interface {
	For(rawURL string) (domain.Provider, error)
	Platforms() []domain.Platform
}

// Service answers "what are this URL's stats?".
type Service struct {
	resolver Resolver
	cache    *Cache
	log      *slog.Logger
}

func NewService(resolver Resolver, cache *Cache, log *slog.Logger) *Service {
	return &Service{resolver: resolver, cache: cache, log: log}
}

// Result pairs the stats with whether they were served from cache.
type Result struct {
	Stats  *domain.VideoStats
	Cached bool
}

// ByURL returns the stats for a single video URL.
func (s *Service) ByURL(ctx context.Context, rawURL string) (*Result, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, domain.ErrInvalidURL
	}

	provider, err := s.resolver.For(rawURL)
	if err != nil {
		return nil, err
	}

	key := string(provider.Platform()) + "|" + rawURL
	if v, ok := s.cache.Get(key); ok {
		if cached, ok := v.(*domain.VideoStats); ok {
			return &Result{Stats: cached, Cached: true}, nil
		}
	}

	start := time.Now()
	result, err := provider.Stats(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	s.log.InfoContext(ctx, "stats fetched",
		"platform", result.Platform,
		"video_id", result.VideoID,
		"duration_ms", time.Since(start).Milliseconds(),
	)

	s.cache.Set(key, result)
	return &Result{Stats: result}, nil
}

// Platforms lists what this deployment can serve.
func (s *Service) Platforms() []domain.Platform { return s.resolver.Platforms() }
