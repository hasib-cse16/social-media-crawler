// Package youtube implements domain.Provider on top of the YouTube Data API v3.
package youtube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/foodibd/socialstats/internal/config"
	"github.com/foodibd/socialstats/internal/domain"
)

const (
	providerName = "youtube"
	maxBodyBytes = 1 << 20 // 1 MiB is far more than videos.list returns
)

// Provider fetches public video statistics from the YouTube Data API.
type Provider struct {
	client  *http.Client
	log     *slog.Logger
	apiKey  string
	baseURL string
}

// New builds a YouTube provider. It returns an error when no API key is set so
// misconfiguration surfaces at boot rather than on the first request.
func New(cfg config.YouTubeConfig, client *http.Client, log *slog.Logger) (*Provider, error) {
	if !cfg.Enabled() {
		return nil, fmt.Errorf("%w: YOUTUBE_API_KEY is empty", domain.ErrMisconfigured)
	}
	return &Provider{
		client:  client,
		log:     log.With("provider", providerName),
		apiKey:  cfg.APIKey,
		baseURL: cfg.BaseURL,
	}, nil
}

func (p *Provider) Platform() domain.Platform { return domain.PlatformYouTube }

func (p *Provider) Match(rawURL string) bool { return IsYouTubeURL(rawURL) }

// videosListResponse mirrors only the fields we consume from videos.list.
type videosListResponse struct {
	Items []struct {
		ID      string `json:"id"`
		Snippet struct {
			Title        string `json:"title"`
			ChannelID    string `json:"channelId"`
			ChannelTitle string `json:"channelTitle"`
			PublishedAt  string `json:"publishedAt"`
		} `json:"snippet"`
		Statistics struct {
			ViewCount    string `json:"viewCount"`
			LikeCount    string `json:"likeCount"`
			CommentCount string `json:"commentCount"`
		} `json:"statistics"`
	} `json:"items"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Stats resolves the video id from rawURL and fetches its metrics.
func (p *Provider) Stats(ctx context.Context, rawURL string) (*domain.VideoStats, error) {
	id, err := ExtractVideoID(rawURL)
	if err != nil {
		return nil, err
	}

	body, err := p.get(ctx, "/videos", url.Values{
		"part": {"snippet,statistics"},
		"id":   {id},
		"key":  {p.apiKey},
	})
	if err != nil {
		return nil, err
	}

	var payload videosListResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("%w: decode videos.list: %v", domain.ErrUpstreamFailure, err)
	}
	if len(payload.Items) == 0 {
		// videos.list returns 200 with an empty list for unknown or private ids.
		return nil, fmt.Errorf("%w: youtube video %s", domain.ErrNotFound, id)
	}

	item := payload.Items[0]
	stats := &domain.VideoStats{
		Platform:     domain.PlatformYouTube,
		VideoID:      item.ID,
		CanonicalURL: CanonicalURL(item.ID),
		Title:        item.Snippet.Title,
		ChannelID:    item.Snippet.ChannelID,
		ChannelTitle: item.Snippet.ChannelTitle,
		ViewCount:    parseCount(item.Statistics.ViewCount),
		LikeCount:    parseCount(item.Statistics.LikeCount),
		CommentCount: parseCount(item.Statistics.CommentCount),
		FetchedAt:    time.Now().UTC(),
	}
	if t, err := time.Parse(time.RFC3339, item.Snippet.PublishedAt); err == nil {
		stats.PublishedAt = &t
	}
	return stats, nil
}

func (p *Provider) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	endpoint := p.baseURL + path + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", domain.ErrUpstreamFailure, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrUpstreamFailure, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", domain.ErrUpstreamFailure, err)
	}

	if resp.StatusCode != http.StatusOK {
		p.log.WarnContext(ctx, "youtube api error", "status", resp.StatusCode, "body", truncate(string(body), 512))
		return nil, domain.NewUpstreamError(providerName, resp.StatusCode, truncate(string(body), 512), sentinelFor(resp.StatusCode))
	}
	return body, nil
}

func sentinelFor(status int) error {
	switch status {
	case http.StatusNotFound:
		return domain.ErrNotFound
	case http.StatusTooManyRequests:
		return domain.ErrRateLimited
	case http.StatusForbidden:
		// The Data API reports quota exhaustion as 403.
		return domain.ErrRateLimited
	default:
		return domain.ErrUpstreamFailure
	}
}

func parseCount(s string) *uint64 {
	if s == "" {
		return nil // hidden by the uploader, or not returned
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Identify extracts the video id from rawURL. Every YouTube URL form carries
// its id, so this never needs the network.
func (p *Provider) Identify(rawURL string) (domain.VideoRef, error) {
	id, err := ExtractVideoID(rawURL)
	if err != nil {
		return domain.VideoRef{}, err
	}
	return domain.VideoRef{
		Platform:     domain.PlatformYouTube,
		VideoID:      id,
		CanonicalURL: CanonicalURL(id),
	}, nil
}
