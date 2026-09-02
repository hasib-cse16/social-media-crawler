package web

import (
	"strings"
	"testing"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

func TestCompact(t *testing.T) {
	tests := []struct {
		in   uint64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1_000, "1K"},
		{1_500, "1.5K"},
		{220_600, "220.6K"},
		{999_999, "1000K"},
		{1_000_000, "1M"},
		{1_238_411, "1.2M"},
		{1_500_000_000, "1.5B"},
	}
	for _, tc := range tests {
		if got := compact(tc.in); got != tc.want {
			t.Errorf("compact(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A counter the platform does not report must never render as zero: "0 views"
// and "this platform has no view count" are different facts.
func TestAbsentCountersRenderAsADash(t *testing.T) {
	if got := compactPtr(nil); got != "—" {
		t.Errorf("compactPtr(nil) = %q, want an em dash", got)
	}
	if got := exact(nil); got != "not reported" {
		t.Errorf("exact(nil) = %q", got)
	}

	zero := uint64(0)
	if got := compactPtr(&zero); got != "0" {
		t.Errorf("a genuine zero rendered as %q, want 0", got)
	}
}

func TestComma(t *testing.T) {
	tests := map[int64]string{
		0: "0", 1: "1", 999: "999", 1_000: "1,000",
		220_600: "220,600", 1_238_411: "1,238,411",
		-50_000: "-50,000",
	}
	for in, want := range tests {
		if got := comma(in); got != want {
			t.Errorf("comma(%d) = %q, want %q", in, got, want)
		}
	}
}

// Negative growth is a real measurement — platforms revise view counts down —
// so it has to render as one rather than being hidden or clamped.
func TestAgo(t *testing.T) {
	now := time.Now()
	tests := []struct {
		at   *time.Time
		want string
	}{
		{nil, "never"},
		{ptr(now.Add(-10 * time.Second)), "just now"},
		{ptr(now.Add(time.Minute)), "just now"}, // clock skew must not print "in -1 minutes"
		{ptr(now.Add(-1 * time.Minute)), "1 minute ago"},
		{ptr(now.Add(-5 * time.Minute)), "5 minutes ago"},
		{ptr(now.Add(-1 * time.Hour)), "1 hour ago"},
		{ptr(now.Add(-26 * time.Hour)), "1 day ago"},
	}
	for _, tc := range tests {
		if got := ago(tc.at); got != tc.want {
			t.Errorf("ago(%v) = %q, want %q", tc.at, got, tc.want)
		}
	}
}

// A row must never be blank when a platform reports no title.
func TestLookupTitleFallsBack(t *testing.T) {
	l := domain.Lookup{VideoID: "abc123", Title: "A caption", ChannelTitle: "A channel"}

	if got := lookupTitle(l); got != "A caption" {
		t.Errorf("with a title = %q", got)
	}

	noTitle := domain.Lookup{VideoID: "abc123", ChannelTitle: "A channel"}
	if got := lookupTitle(noTitle); got != "A channel — abc123" {
		t.Errorf("with only a channel = %q", got)
	}

	bare := domain.Lookup{VideoID: "abc123"}
	if got := lookupTitle(bare); got != "abc123" {
		t.Errorf("with no metadata = %q, want the id", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 20); got != "short" {
		t.Errorf("truncate left it alone? got %q", got)
	}
	got := truncate("a much longer caption than the limit", 10)
	if !strings.HasSuffix(got, "…") || len([]rune(got)) > 11 {
		t.Errorf("truncate = %q", got)
	}
	// Counted in runes, so a multi-byte caption is not cut mid-character.
	if got := truncate("日本語のキャプションです", 5); len([]rune(got)) != 6 {
		t.Errorf("multibyte truncate = %q (%d runes)", got, len([]rune(got)))
	}
}

func ptr[T any](v T) *T { return &v }
