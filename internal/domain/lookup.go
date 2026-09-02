package domain

import "time"

// Counters is the set of public metrics a platform may report.
//
// Every field is a pointer for the same reason it is in VideoStats: nil means
// "this platform does not report it, or the uploader hides it", which is not
// the same fact as zero. Collapsing the two would make an Instagram photo post
// look like a video nobody watched.
type Counters struct {
	ViewCount    *uint64 `json:"view_count,omitempty"`
	LikeCount    *uint64 `json:"like_count,omitempty"`
	CommentCount *uint64 `json:"comment_count,omitempty"`
	ShareCount   *uint64 `json:"share_count,omitempty"`
	SaveCount    *uint64 `json:"save_count,omitempty"`
}

// Counters extracts just the metrics from a provider result.
func (v *VideoStats) Counters() Counters {
	return Counters{
		ViewCount:    v.ViewCount,
		LikeCount:    v.LikeCount,
		CommentCount: v.CommentCount,
		ShareCount:   v.ShareCount,
		SaveCount:    v.SaveCount,
	}
}

// Lookup is one user's one-off reading of one video.
//
// It is deliberately a flat, immutable row rather than a tracked entity: the
// counters it holds are what the platform said at LookedUpAt and are never
// revised. Looking the same URL up again appends another row, so the list a
// user sees is a history of what they asked, not a set of live gauges.
type Lookup struct {
	ID       int64  `json:"-"`
	PublicID string `json:"id"`
	UserID   int64  `json:"-"`

	Platform     Platform `json:"platform"`
	VideoID      string   `json:"video_id"`
	CanonicalURL string   `json:"canonical_url"`

	Title       string     `json:"title,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`

	ChannelID          string `json:"channel_id,omitempty"`
	ChannelTitle       string `json:"channel_title,omitempty"`
	ChannelURL         string `json:"channel_url,omitempty"`
	ChannelEmail       string `json:"channel_email,omitempty"`
	ChannelDescription string `json:"channel_description,omitempty"`

	Counters   `json:"counters"`
	LookedUpAt time.Time `json:"looked_up_at"`
}

// NewLookup builds the row to persist from a provider result.
func NewLookup(userID int64, s *VideoStats) Lookup {
	return Lookup{
		UserID:             userID,
		Platform:           s.Platform,
		VideoID:            s.VideoID,
		CanonicalURL:       s.CanonicalURL,
		Title:              s.Title,
		PublishedAt:        s.PublishedAt,
		ChannelID:          s.ChannelID,
		ChannelTitle:       s.ChannelTitle,
		ChannelURL:         s.ChannelURL,
		ChannelEmail:       s.ChannelEmail,
		ChannelDescription: s.ChannelDescription,
		Counters:           s.Counters(),
		LookedUpAt:         s.FetchedAt,
	}
}
