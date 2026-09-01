// Package web is the server-rendered dashboard.
//
// It renders html/template parsed once at boot from an embedded filesystem, and
// draws its own charts as inline SVG. There is no build step, no node_modules
// and nothing loaded from a CDN: the binary is the whole frontend, which means
// the dashboard cannot be broken by an upstream package release and works with
// JavaScript disabled.
//
// The one small script it does ship is progressive enhancement only — sorting
// and the range picker are query parameters, and every form submits without it.
package web

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

// Presentation helpers, exposed to templates.
//
// They live in Go rather than in the templates because formatting a count or a
// duration is logic, and logic in a template is logic nobody can test.

// compact renders a count the way a dashboard reads best: 1.2M rather than
// 1,238,411.
//
// The exact figure is never lost — every place this is used also puts the full
// number in a title attribute — but a column of nine-digit numbers is a column
// nobody can scan.
func compact(n uint64) string {
	switch {
	case n < 1_000:
		return strconv.FormatUint(n, 10)
	case n < 1_000_000:
		return trimZero(float64(n)/1_000) + "K"
	case n < 1_000_000_000:
		return trimZero(float64(n)/1_000_000) + "M"
	default:
		return trimZero(float64(n)/1_000_000_000) + "B"
	}
}

// compactPtr renders an optional counter. A counter the platform does not
// report is shown as an em dash, never as zero: "0 views" and "this platform
// has no view count" are different facts and the dashboard must not merge them.
func compactPtr(n *uint64) string {
	if n == nil {
		return "—"
	}
	return compact(*n)
}

// exact renders a counter in full with thousands separators, for title
// attributes and detail pages.
func exact(n *uint64) string {
	if n == nil {
		return "not reported"
	}
	return comma(int64(*n))
}

// comma groups digits in threes.
func comma(n int64) string {
	negative := n < 0
	digits := strconv.FormatInt(n, 10)
	if negative {
		digits = digits[1:]
	}

	var b strings.Builder
	if negative {
		b.WriteByte('-')
	}
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// signed renders a delta with an explicit sign.
//
// Negatives are real: platforms revise view counts downward, so a minus here is
// a measurement rather than a bug, and the display has to be able to say so.
func signed(n *int64) string {
	if n == nil {
		return "—"
	}
	switch {
	case *n > 0:
		return "+" + comma(*n)
	case *n < 0:
		return comma(*n) // comma already carries the minus
	default:
		return "0"
	}
}

// deltaClass names the CSS class for a delta, so colour is applied from one
// place rather than in three templates.
func deltaClass(n *int64) string {
	switch {
	case n == nil:
		return "delta-none"
	case *n > 0:
		return "delta-up"
	case *n < 0:
		return "delta-down"
	default:
		return "delta-flat"
	}
}

// ago renders a timestamp as a rough age.
//
// Rough on purpose: "3 hours ago" is what somebody scanning a list actually
// wants, and the precise timestamp is in the title attribute for when it is not.
func ago(t *time.Time) string {
	if t == nil {
		return "never"
	}

	d := time.Since(*t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day") + " ago"
	default:
		return t.Format("2 Jan 2006")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// timestamp renders a full, unambiguous time for title attributes.
func timestamp(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format("2 Jan 2006 15:04 MST")
}

// duration renders a configured interval readably.
func duration(d time.Duration) string {
	switch {
	case d <= 0:
		return "—"
	case d < time.Minute:
		return plural(int(d.Seconds()), "second")
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d%(24*time.Hour) == 0 && d >= 24*time.Hour:
		return plural(int(d.Hours()/24), "day")
	default:
		return plural(int(d.Hours()), "hour")
	}
}

// platformName is the display name for a platform.
func platformName(p domain.Platform) string {
	switch p {
	case domain.PlatformYouTube:
		return "YouTube"
	case domain.PlatformTikTok:
		return "TikTok"
	case domain.PlatformMeta:
		return "Meta"
	default:
		return string(p)
	}
}

// statusLabel turns a fetch status into something a person can act on.
//
// The wording matters here: "blocked" and "gone" look similar in a table but
// mean opposite things — one is our problem and will likely clear, the other is
// the video actually being deleted — and a dashboard that blurs them teaches
// people to ignore both.
func statusLabel(v *domain.Video) string {
	if v.Schedule.UnavailableSince != nil {
		return "Removed by the platform"
	}
	switch v.Schedule.LastFetchStatus {
	case domain.FetchOK:
		return "OK"
	case domain.FetchPending:
		return "Not fetched yet"
	case domain.FetchBlocked:
		return "Blocked — will retry"
	case domain.FetchNotFound:
		return "Not found — will retry"
	case domain.FetchError:
		return "Failed — will retry"
	default:
		return string(v.Schedule.LastFetchStatus)
	}
}

// statusTone names the visual severity, kept separate from the accent colour so
// "needs attention" reads at a glance without depending on hue alone.
func statusTone(v *domain.Video) string {
	switch {
	case v.Schedule.UnavailableSince != nil:
		return "critical"
	case v.Schedule.LastFetchStatus == domain.FetchOK:
		return "ok"
	case v.Schedule.LastFetchStatus == domain.FetchPending:
		return "neutral"
	default:
		return "warning"
	}
}

// trimZero drops a trailing ".0" so 1.0M reads as 1M.
func trimZero(f float64) string {
	s := strconv.FormatFloat(round1(f), 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// title falls back through the names a video might have, so a row is never
// blank while a fetch is pending.
func videoTitle(v *domain.Video, label string) string {
	if label != "" {
		return label
	}
	if v.Title != "" {
		return v.Title
	}
	if v.ChannelTitle != "" {
		return v.ChannelTitle + " — " + v.PlatformVideoID
	}
	return v.PlatformVideoID
}

// truncate shortens a caption for a single-line cell. TikTok and Instagram
// titles are whole paragraphs.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return strings.TrimSpace(string(runes[:n])) + "…"
}

// pluralise is exposed for counts in copy.
func pluralise(n int, unit string) string { return plural(n, unit) }

// percent renders a ratio as a whole-number percentage.
func percent(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }
