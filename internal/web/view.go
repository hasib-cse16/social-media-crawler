package web

import (
	"html/template"
	"net/url"
	"strconv"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/tracking"
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

// DashboardView backs the main list.
type DashboardView struct {
	Videos    []VideoRow
	Summary   SummaryView
	Filters   Filters
	Platforms []domain.Platform
	Empty     bool
}

// VideoRow is one line of the dashboard.
type VideoRow struct {
	ID           string
	Title        string
	Caption      string
	Platform     domain.Platform
	PlatformName string
	CanonicalURL string
	ChannelTitle string

	Views      string
	ViewsExact string
	Likes      string
	Comments   string

	Gained      string
	GainedClass string
	BaselineAt  *time.Time

	Sparkline  template.HTML
	Fresh      bool
	Status     string
	Tone       string
	LastRead   string
	LastReadAt *time.Time
}

// SummaryView is the headline strip.
type SummaryView struct {
	Tracked     int
	TotalViews  string
	TotalExact  string
	Gained      string
	GainedClass string
	Stale       int
	Unavailable int
	Window      string
	ByPlatform  []PlatformCount
}

type PlatformCount struct {
	Platform domain.Platform
	Name     string
	Count    int
}

// Filters is the current list query, echoed back so the controls show what is
// actually applied rather than their defaults.
type Filters struct {
	Sort            string
	Platform        string
	Window          string
	Windows         []Option
	Sorts           []Option
	PlatformOptions []Option
}

// Option is one choice in a select control.
type Option struct {
	Value    string
	Label    string
	Selected bool
}

// VideoView backs the detail page.
type VideoView struct {
	Row      VideoRow
	Notes    string
	Chart    template.HTML
	Range    string
	Ranges   []Option
	Points   int
	Source   string
	Attempts []AttemptView
	Schedule ScheduleView
}

// ScheduleView is the "why does this number look like that?" panel.
//
// It is on the page because the honest answer to a stale figure is usually
// about the poller rather than the video, and a dashboard that cannot explain
// itself gets distrusted for the wrong reasons.
type ScheduleView struct {
	Interval    string
	NextFetch   string
	NextFetchAt *time.Time
	Failures    int
	LastError   string
	Retired     bool
	RetiredAt   *time.Time
	Trackers    int
}

// AttemptView is one row of the fetch log.
type AttemptView struct {
	When     string
	WhenAt   time.Time
	Status   string
	Tone     string
	Duration string
	Error    string
}

// SettingsView backs the account page.
type SettingsView struct {
	Timezones []Option
}

// toRow maps a tracking entry into its row.
func toRow(e tracking.Entry) VideoRow {
	v := e.Video

	row := VideoRow{
		ID:           v.PublicID,
		Title:        videoTitle(v, e.Label),
		Platform:     v.Platform,
		PlatformName: platformName(v.Platform),
		CanonicalURL: v.CanonicalURL,
		ChannelTitle: v.ChannelTitle,

		Views:      compactPtr(v.Latest.ViewCount),
		ViewsExact: exact(v.Latest.ViewCount),
		Likes:      compactPtr(v.Latest.LikeCount),
		Comments:   compactPtr(v.Latest.CommentCount),

		Gained:      signed(e.ViewsGained),
		GainedClass: deltaClass(e.ViewsGained),
		BaselineAt:  e.BaselineAt,

		Fresh:      e.Fresh,
		Status:     statusLabel(v),
		Tone:       statusTone(v),
		LastRead:   ago(v.LatestCapturedAt),
		LastReadAt: v.LatestCapturedAt,
	}

	// The caption is shown under the title only when it is not already the
	// title, so a row does not repeat itself.
	if e.Label != "" && v.Title != "" {
		row.Caption = truncate(v.Title, 90)
	} else if v.Title != "" && v.ChannelTitle != "" {
		row.Caption = v.ChannelTitle
	}

	if len(e.Sparkline) > 0 {
		row.Sparkline = Sparkline(e.Sparkline)
	}
	return row
}

// windowOptions are the ranges the growth figure can be measured over.
var windowOptions = []struct {
	value string
	label string
}{
	{"24h", "24 hours"},
	{"168h", "7 days"},
	{"720h", "30 days"},
	{"2160h", "90 days"},
}

var sortOptions = []struct {
	value string
	label string
}{
	{"views", "Most views"},
	{"gained", "Fastest growing"},
	{"recent", "Recently added"},
	{"title", "Name"},
	{"fetched", "Last updated"},
}

func buildFilters(sort, platform, window string) Filters {
	f := Filters{Sort: sort, Platform: platform, Window: window}

	for _, o := range windowOptions {
		f.Windows = append(f.Windows, Option{Value: o.value, Label: o.label, Selected: o.value == window})
	}
	for _, o := range sortOptions {
		f.Sorts = append(f.Sorts, Option{Value: o.value, Label: o.label, Selected: o.value == sort})
	}
	return f
}

// queryString rebuilds the list query with one parameter changed, so the
// controls preserve the rest of the filters instead of resetting them.
func queryString(f Filters, key, value string) string {
	q := url.Values{}
	set := func(k, v string) {
		if k == key {
			v = value
		}
		if v != "" {
			q.Set(k, v)
		}
	}
	set("sort", f.Sort)
	set("platform", f.Platform)
	set("window", f.Window)

	if key != "sort" && key != "platform" && key != "window" && value != "" {
		q.Set(key, value)
	}
	if len(q) == 0 {
		return "/"
	}
	return "/?" + q.Encode()
}

// windowLabel turns a duration string back into the words used in the picker.
func windowLabel(window string) string {
	for _, o := range windowOptions {
		if o.value == window {
			return o.label
		}
	}
	return window
}

func itoa(n int) string { return strconv.Itoa(n) }
