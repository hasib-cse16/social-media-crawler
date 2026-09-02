// Package lookup is the application layer for the dashboard: it fetches a
// video's public stats and records what was found, per user.
//
// There is no scheduling, no refresh and no reconciliation here. A lookup is a
// question a user asked once, and the answer is stored exactly as it was given.
package lookup

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/stats"
)

// Store is the persistence this service needs, declared here rather than
// imported so the postgres package stays a detail.
type Store interface {
	Create(ctx context.Context, in domain.Lookup) (*domain.Lookup, error)
	ByPublicID(ctx context.Context, userID int64, publicID string) (*domain.Lookup, error)
	Recent(ctx context.Context, userID int64, limit int) ([]domain.Lookup, error)
	Delete(ctx context.Context, userID int64, publicID string) error
}

// Fetcher is satisfied by *stats.Service.
type Fetcher interface {
	ByURL(ctx context.Context, rawURL string) (*stats.Result, error)
	Platforms() []domain.Platform
}

// Config bounds the two things a user can grow without limit.
type Config struct {
	// Timeout bounds one lookup, including the provider call.
	Timeout time.Duration

	// HistoryLimit is how many past lookups the dashboard lists.
	HistoryLimit int
}

const (
	defaultTimeout      = 30 * time.Second
	defaultHistoryLimit = 100
)

func (c Config) withDefaults() Config {
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if c.HistoryLimit <= 0 {
		c.HistoryLimit = defaultHistoryLimit
	}
	return c
}

// Service answers "what are this URL's numbers, and who posted it?".
type Service struct {
	store   Store
	fetcher Fetcher
	cfg     Config
	log     *slog.Logger
}

func NewService(store Store, fetcher Fetcher, cfg Config, log *slog.Logger) *Service {
	return &Service{store: store, fetcher: fetcher, cfg: cfg.withDefaults(), log: log}
}

// Lookup fetches rawURL's stats and records them for this user.
//
// The row is written after a successful fetch and never before: a failed
// lookup is not history, it is a retry waiting to happen.
func (s *Service) Lookup(ctx context.Context, userID int64, rawURL string) (*domain.Lookup, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, domain.ErrInvalidURL
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	result, err := s.fetcher.ByURL(ctx, rawURL)
	if err != nil {
		return nil, err
	}

	saved, err := s.store.Create(ctx, domain.NewLookup(userID, result.Stats))
	if err != nil {
		return nil, fmt.Errorf("record lookup: %w", err)
	}

	s.log.InfoContext(ctx, "lookup recorded",
		"user_id", userID,
		"platform", saved.Platform,
		"video_id", saved.VideoID,
		"cached", result.Cached,
	)
	return saved, nil
}

// Get returns one of this user's past lookups.
func (s *Service) Get(ctx context.Context, userID int64, publicID string) (*domain.Lookup, error) {
	return s.store.ByPublicID(ctx, userID, publicID)
}

// History lists this user's lookups, newest first.
func (s *Service) History(ctx context.Context, userID int64) ([]domain.Lookup, error) {
	return s.store.Recent(ctx, userID, s.cfg.HistoryLimit)
}

// Remove deletes one of this user's lookups.
func (s *Service) Remove(ctx context.Context, userID int64, publicID string) error {
	return s.store.Delete(ctx, userID, publicID)
}

// Platforms lists what this deployment can resolve.
func (s *Service) Platforms() []domain.Platform { return s.fetcher.Platforms() }
