package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

// The Graph API is the correct source when it is available, and it is available
// far less often than its documentation suggests: reading a video node needs a
// token whose app has been granted that object, which in practice means videos
// on Pages the token administers. There is no public-data tier for third-party
// video metrics — the old `?fields=views` on arbitrary ids was removed with
// Graph v3.0 — so this path is an optimisation for first-party content, and the
// page path in facebook.go is what answers everything else.

// graphVideoResponse mirrors the fields consumed from a /{video-id} node.
type graphVideoResponse struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	CreatedTime  string `json:"created_time"`
	PermalinkURL string `json:"permalink_url"`
	Views        uint64 `json:"views"`
	From         struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"from"`
	Likes struct {
		Summary struct {
			TotalCount uint64 `json:"total_count"`
		} `json:"summary"`
	} `json:"likes"`
	Comments struct {
		Summary struct {
			TotalCount uint64 `json:"total_count"`
		} `json:"summary"`
	} `json:"comments"`
	Error *graphError `json:"error"`
}

type graphError struct {
	Message   string `json:"message"`
	Type      string `json:"type"`
	Code      int    `json:"code"`
	Subcode   int    `json:"error_subcode"`
	TraceID   string `json:"fbtrace_id"`
	UserTitle string `json:"error_user_title"`
}

func (e *graphError) Error() string {
	return fmt.Sprintf("graph error %d/%d (%s): %s", e.Code, e.Subcode, e.Type, e.Message)
}

// graphVideo reads a video node. It is only called when a token is configured.
func (p *Provider) graphVideo(ctx context.Context, ref Ref) (*domain.VideoStats, error) {
	endpoint := fmt.Sprintf("%s/%s/%s?%s",
		strings.TrimSuffix(p.cfg.BaseURL, "/"),
		p.cfg.APIVersion,
		url.PathEscape(ref.ID),
		url.Values{
			"fields": {"id,title,description,created_time,permalink_url,views,from{id,name}," +
				"likes.summary(true).limit(0),comments.summary(true).limit(0)"},
			"access_token": {p.cfg.AccessToken},
		}.Encode())

	body, status, err := p.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}

	var payload graphVideoResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("%w: decode graph response: %v", domain.ErrUpstreamFailure, err)
	}
	if payload.Error != nil {
		return nil, graphSentinel(payload.Error)
	}
	if status != 200 || payload.ID == "" {
		return nil, domain.NewUpstreamError(providerName, status, truncate(string(body), 256), domain.ErrUpstreamFailure)
	}

	resolved := ref
	resolved.ID = payload.ID

	canonical := payload.PermalinkURL
	switch {
	case canonical == "":
		canonical = CanonicalURL(resolved)
	case strings.HasPrefix(canonical, "/"):
		canonical = "https://www.facebook.com" + canonical
	}

	stats := &domain.VideoStats{
		Platform:     domain.PlatformMeta,
		VideoID:      payload.ID,
		CanonicalURL: canonical,
		Title:        firstNonEmpty(payload.Title, payload.Description),
		ChannelID:    payload.From.ID,
		ChannelTitle: payload.From.Name,
		FetchedAt:    time.Now().UTC(),

		ViewCount:    nonZero(payload.Views),
		LikeCount:    nonZero(payload.Likes.Summary.TotalCount),
		CommentCount: nonZero(payload.Comments.Summary.TotalCount),
	}
	if t, err := time.Parse("2006-01-02T15:04:05-0700", payload.CreatedTime); err == nil {
		utc := t.UTC()
		stats.PublishedAt = &utc
	}
	return stats, nil
}

// graphSentinel maps a Graph error onto the domain sentinels. The codes matter
// more than the status: Graph answers 400 for most of these.
func graphSentinel(e *graphError) error {
	switch e.Code {
	case 100, 803:
		// 100 "unsupported get request" is what Graph says for an object the
		// token cannot see, which is indistinguishable from a missing one.
		return fmt.Errorf("%w: %s", domain.ErrNotFound, e.Error())
	case 4, 17, 32, 613:
		return fmt.Errorf("%w: %s", domain.ErrRateLimited, e.Error())
	case 102, 190:
		return fmt.Errorf("%w: META_ACCESS_TOKEN is invalid or expired: %s", domain.ErrMisconfigured, e.Error())
	case 10, 200, 299:
		return fmt.Errorf("%w: the token lacks permission for this object: %s", domain.ErrNotFound, e.Error())
	default:
		return fmt.Errorf("%w: %s", domain.ErrUpstreamFailure, e.Error())
	}
}
