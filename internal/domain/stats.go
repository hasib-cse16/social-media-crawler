package domain

import "time"

// Platform identifies a supported social video platform.
type Platform string

const (
	PlatformYouTube Platform = "youtube"
	PlatformMeta    Platform = "meta"
	PlatformTikTok  Platform = "tiktok"
)

// VideoStats is the platform-agnostic view of a single video's public metrics.
// Counters are pointers so that "not supported by this platform" (nil) stays
// distinguishable from "genuinely zero".
type VideoStats struct {
	Platform     Platform   `json:"platform"`
	VideoID      string     `json:"video_id"`
	CanonicalURL string     `json:"canonical_url"`
	Title        string     `json:"title,omitempty"`
	ChannelID    string     `json:"channel_id,omitempty"`
	ChannelTitle string     `json:"channel_title,omitempty"`
	PublishedAt  *time.Time `json:"published_at,omitempty"`
	ViewCount    *uint64    `json:"view_count,omitempty"`
	LikeCount    *uint64    `json:"like_count,omitempty"`
	CommentCount *uint64    `json:"comment_count,omitempty"`
	ShareCount   *uint64    `json:"share_count,omitempty"`
	SaveCount    *uint64    `json:"save_count,omitempty"`
	FetchedAt    time.Time  `json:"fetched_at"`
}

func U64(v uint64) *uint64 { return &v }
