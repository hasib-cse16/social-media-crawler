package web

import (
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

// Charts are generated here as inline SVG rather than drawn by a JavaScript
// library. The trade is deliberate: no CDN request, no library release can
// break the dashboard, the charts render with scripting disabled, and they are
// in the HTML the server already had to produce. What it costs is interaction —
// there is no zoom or hover-scrub — which is the moment to reconsider, not
// before.
//
// Everything interpolated into the markup below is a number this package
// computed. No caller-supplied text reaches the SVG, which is what makes
// template.HTML safe here.

// series is a run of readings prepared for drawing.
type series struct {
	points []point
	minY   float64
	maxY   float64
	minX   time.Time
	maxX   time.Time
}

type point struct {
	at    time.Time
	value float64
}

// newSeries extracts the view counts from snapshots, dropping readings where
// the platform reported nothing.
//
// Dropping rather than zeroing matters: a gap where a counter was not reported
// must not be drawn as a fall to zero and back, which would look like the video
// lost every view it had.
func newSeries(snapshots []domain.Snapshot) series {
	s := series{minY: math.MaxFloat64, maxY: -math.MaxFloat64}

	for _, snap := range snapshots {
		if snap.ViewCount == nil {
			continue
		}
		v := float64(*snap.ViewCount)
		s.points = append(s.points, point{at: snap.CapturedAt, value: v})
		s.minY = math.Min(s.minY, v)
		s.maxY = math.Max(s.maxY, v)
	}

	if len(s.points) > 0 {
		s.minX = s.points[0].at
		s.maxX = s.points[len(s.points)-1].at
	}
	return s
}

func (s series) empty() bool { return len(s.points) < 2 }

// scaleY maps a value onto a vertical position.
//
// The band is padded and never zero-height: a flat series — which is the normal
// state of a video nobody is watching this week — would otherwise divide by
// zero, and drawing it along the top or bottom edge would imply a trend it does
// not have. A flat line through the middle is the honest picture.
func (s series) scaleY(v float64, top, height float64) float64 {
	span := s.maxY - s.minY
	if span <= 0 {
		return top + height/2
	}
	return top + height - ((v-s.minY)/span)*height
}

func (s series) scaleX(at time.Time, left, width float64) float64 {
	span := s.maxX.Sub(s.minX)
	if span <= 0 {
		return left + width/2
	}
	return left + (float64(at.Sub(s.minX))/float64(span))*width
}

// Sparkline draws a small trend line for a list row.
func Sparkline(snapshots []domain.Snapshot) template.HTML {
	const (
		w, h    = 120.0, 32.0
		padding = 3.0
	)

	s := newSeries(snapshots)
	if s.empty() {
		// A single reading is not a trend, and a line drawn through one point
		// would suggest one. Say so instead.
		return template.HTML(fmt.Sprintf(
			`<svg class="sparkline sparkline-empty" viewBox="0 0 %.0f %.0f" role="img" `+
				`aria-label="Not enough readings to chart yet" focusable="false">`+
				`<line x1="0" y1="%.1f" x2="%.0f" y2="%.1f"/></svg>`,
			w, h, h/2, w, h/2))
	}

	inner := h - 2*padding
	coords := make([]string, 0, len(s.points))
	for _, p := range s.points {
		coords = append(coords, fmt.Sprintf("%.1f,%.1f",
			s.scaleX(p.at, 0, w), s.scaleY(p.value, padding, inner)))
	}

	last := s.points[len(s.points)-1]
	lastX := s.scaleX(last.at, 0, w)
	lastY := s.scaleY(last.value, padding, inner)

	// The area fill closes the path along the baseline, which gives the eye the
	// shape of the trend before it reads the line.
	area := fmt.Sprintf("0,%.1f %s %.1f,%.1f", h, strings.Join(coords, " "), lastX, h)

	return template.HTML(fmt.Sprintf(
		`<svg class="sparkline" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" `+
			`role="img" aria-label="%d readings, trending to %s" focusable="false">`+
			`<polygon class="spark-area" points="%s"/>`+
			`<polyline class="spark-line" points="%s"/>`+
			`<circle class="spark-end" cx="%.1f" cy="%.1f" r="2.5"/>`+
			`</svg>`,
		w, h, len(s.points), compact(uint64(last.value)), area, strings.Join(coords, " "), lastX, lastY))
}

// Chart draws the full-size history chart for a video page.
//
// It carries axis labels and a grid because this one is read rather than
// glanced at: without a scale, a rising line says nothing about whether the
// video gained fifty views or fifty thousand.
func Chart(snapshots []domain.Snapshot) template.HTML {
	const (
		w, h                              = 720.0, 240.0
		padLeft, padRight, padTop, padBot = 64.0, 12.0, 16.0, 28.0
	)

	s := newSeries(snapshots)
	if s.empty() {
		return template.HTML(
			`<div class="chart-empty"><p>Not enough readings to chart yet.</p>` +
				`<p class="muted">A line needs at least two, and the poller adds one each cycle.</p></div>`)
	}

	plotW := w - padLeft - padRight
	plotH := h - padTop - padBot

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %.0f %.0f" role="img" `+
		`aria-label="View count over time, %s to %s" preserveAspectRatio="xMidYMid meet">`,
		w, h, compact(uint64(s.minY)), compact(uint64(s.maxY)))

	// Horizontal gridlines with value labels.
	const rows = 4
	for i := 0; i <= rows; i++ {
		frac := float64(i) / rows
		y := padTop + plotH*frac
		value := s.maxY - (s.maxY-s.minY)*frac

		fmt.Fprintf(&b, `<line class="grid" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`,
			padLeft, y, w-padRight, y)
		fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="%.1f" text-anchor="end">%s</text>`,
			padLeft-8, y+4, compact(uint64(math.Max(value, 0))))
	}

	coords := make([]string, 0, len(s.points))
	for _, p := range s.points {
		coords = append(coords, fmt.Sprintf("%.1f,%.1f",
			s.scaleX(p.at, padLeft, plotW), s.scaleY(p.value, padTop, plotH)))
	}

	last := s.points[len(s.points)-1]
	lastX := s.scaleX(last.at, padLeft, plotW)
	lastY := s.scaleY(last.value, padTop, plotH)

	fmt.Fprintf(&b, `<polygon class="chart-area" points="%.1f,%.1f %s %.1f,%.1f"/>`,
		padLeft, padTop+plotH, strings.Join(coords, " "), lastX, padTop+plotH)
	fmt.Fprintf(&b, `<polyline class="chart-line" points="%s"/>`, strings.Join(coords, " "))

	// Individual readings are marked, so a chart drawn from four points does not
	// pretend to the smoothness of one drawn from four hundred.
	if len(s.points) <= 60 {
		for i, p := range s.points {
			fmt.Fprintf(&b, `<circle class="chart-dot" cx="%.1f" cy="%.1f" r="2"><title>%s on %s</title></circle>`,
				s.scaleX(p.at, padLeft, plotW), s.scaleY(p.value, padTop, plotH),
				comma(int64(p.value)), template.HTMLEscapeString(p.at.Format("2 Jan 15:04")))
			_ = i
		}
	}
	fmt.Fprintf(&b, `<circle class="chart-end" cx="%.1f" cy="%.1f" r="4"/>`, lastX, lastY)

	// Time axis: just the ends, which is what a reader needs to orient.
	fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="%.1f" text-anchor="start">%s</text>`,
		padLeft, h-8, template.HTMLEscapeString(s.minX.Format("2 Jan 15:04")))
	fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="%.1f" text-anchor="end">%s</text>`,
		w-padRight, h-8, template.HTMLEscapeString(s.maxX.Format("2 Jan 15:04")))

	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
