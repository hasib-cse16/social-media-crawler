package web

import (
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

// View models.
//
// The templates are given these rather than the domain types directly. It costs
// a mapping step and buys two things: a template cannot reach into parts of the
// domain it has no business rendering, and a field that changes shape breaks the
// compile here rather than silently rendering "<no value>" in production.

// Page is what every template receives, so the layout can always find the
// things it needs.
type Page struct {
	Title     string
	Nav       string // which nav item is current
	User      *domain.User
	CSRFToken string
	Flash     *Flash
	Version   string
	Now       time.Time
	Data      any
}

// DashboardView backs the main page: the paste-a-URL form and what has been
// looked up before.
type DashboardView struct {
	Recent    []LookupRow
	Platforms []domain.Platform
	Empty     bool
}

// LookupRow is one line of the history list.
type LookupRow struct {
	ID           string
	Title        string
	Caption      string
	Platform     domain.Platform
	PlatformName string
	CanonicalURL string

	ChannelTitle string
	ChannelURL   string
	ChannelEmail string

	Views      string
	ViewsExact string
	Likes      string
	Comments   string
	Shares     string
	Saves      string

	When   string
	WhenAt *time.Time
}

// LookupView backs the detail page for one past lookup.
type LookupView struct {
	Row LookupRow

	// PublishedAt and ChannelDescription are shown only here rather than in the
	// list, because they are what someone drills in for.
	Published          string
	PublishedAt        *time.Time
	ChannelID          string
	ChannelDescription string
}

// SettingsView backs the account page.
type SettingsView struct {
	Timezones []Option
}

// Option is one choice in a select control.
type Option struct {
	Value    string
	Label    string
	Selected bool
}

// toRow maps a stored lookup into its row.
func toRow(l domain.Lookup) LookupRow {
	row := LookupRow{
		ID:           l.PublicID,
		Title:        lookupTitle(l),
		Platform:     l.Platform,
		PlatformName: platformName(l.Platform),
		CanonicalURL: l.CanonicalURL,

		ChannelTitle: l.ChannelTitle,
		ChannelURL:   l.ChannelURL,
		ChannelEmail: l.ChannelEmail,

		Views:      compactPtr(l.ViewCount),
		ViewsExact: exact(l.ViewCount),
		Likes:      compactPtr(l.LikeCount),
		Comments:   compactPtr(l.CommentCount),
		Shares:     compactPtr(l.ShareCount),
		Saves:      compactPtr(l.SaveCount),

		When:   ago(&l.LookedUpAt),
		WhenAt: &l.LookedUpAt,
	}
	if l.ChannelTitle != "" {
		row.Caption = l.ChannelTitle
	}
	return row
}

// toLookupView maps a stored lookup into the detail page.
func toLookupView(l domain.Lookup) LookupView {
	return LookupView{
		Row:                toRow(l),
		Published:          ago(l.PublishedAt),
		PublishedAt:        l.PublishedAt,
		ChannelID:          l.ChannelID,
		ChannelDescription: l.ChannelDescription,
	}
}
