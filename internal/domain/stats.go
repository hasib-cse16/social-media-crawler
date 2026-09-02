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
	Platform     Platform `json:"platform"`
	VideoID      string   `json:"video_id"`
	CanonicalURL string   `json:"canonical_url"`
	Title        string   `json:"title,omitempty"`
	ChannelID    string   `json:"channel_id,omitempty"`
	ChannelTitle string   `json:"channel_title,omitempty"`

	// ChannelURL is a link to the channel/profile the video was posted by.
	ChannelURL string `json:"channel_url,omitempty"`

	// ChannelEmail is the public business email the channel owner published,
	// when there is one. It is not a separate API surface: owners write it into
	// the channel description, so it is read out of there. Empty is the common
	// case and means "not published", never "lookup failed".
	ChannelEmail string `json:"channel_email,omitempty"`

	// ChannelDescription is the channel's own about-text, kept because it is
	// where ChannelEmail came from and is what a human checks when the regex
	// picked the wrong address.
	ChannelDescription string     `json:"channel_description,omitempty"`
	PublishedAt        *time.Time `json:"published_at,omitempty"`
	ViewCount          *uint64    `json:"view_count,omitempty"`
	LikeCount          *uint64    `json:"like_count,omitempty"`
	CommentCount       *uint64    `json:"comment_count,omitempty"`
	ShareCount         *uint64    `json:"share_count,omitempty"`
	SaveCount          *uint64    `json:"save_count,omitempty"`
	FetchedAt          time.Time  `json:"fetched_at"`
}

func U64(v uint64) *uint64 { return &v }
