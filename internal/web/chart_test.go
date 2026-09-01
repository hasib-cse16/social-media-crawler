package web

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

func snaps(values ...*uint64) []domain.Snapshot {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	out := make([]domain.Snapshot, 0, len(values))
	for i, v := range values {
		out = append(out, domain.Snapshot{
			CapturedAt: base.Add(time.Duration(i) * time.Hour),
			Counters:   domain.Counters{ViewCount: v},
		})
	}
	return out
}

func u(v uint64) *uint64 { return &v }

func TestSparklineDrawsATrend(t *testing.T) {
	got := string(Sparkline(snaps(u(100), u(150), u(220))))

	if !strings.Contains(got, "spark-line") {
		t.Fatalf("no line drawn: %s", got)
	}
	points := regexp.MustCompile(`class="spark-line" points="([^"]*)"`).FindStringSubmatch(got)
	if points == nil || len(strings.Fields(points[1])) != 3 {
		t.Errorf("expected three points, got %v", points)
	}
	// Screen readers get the same information the line conveys.
	if !strings.Contains(got, `aria-label="3 readings, trending to 220"`) {
		t.Errorf("aria-label missing or wrong: %s", got)
	}
}

// One reading is not a trend, and a line through a single point would suggest
// one that is not there.
func TestSparklineWithTooFewPoints(t *testing.T) {
	for name, in := range map[string][]domain.Snapshot{
		"none":  nil,
		"one":   snaps(u(100)),
		"nulls": snaps(nil, nil),
	} {
		t.Run(name, func(t *testing.T) {
			got := string(Sparkline(in))
			if !strings.Contains(got, "sparkline-empty") {
				t.Errorf("expected the empty treatment, got %s", got)
			}
			if strings.Contains(got, "spark-line") {
				t.Error("a trend line was drawn without a trend")
			}
		})
	}
}

// A gap where the platform reported nothing must not be drawn as a fall to zero
// and back, which would look like the video lost every view it had.
func TestUnreportedCountersAreSkippedNotZeroed(t *testing.T) {
	s := newSeries(snaps(u(1000), nil, u(1100)))

	if len(s.points) != 2 {
		t.Fatalf("%d points, want the unreported reading dropped", len(s.points))
	}
	if s.minY != 1000 {
		t.Errorf("minimum = %v; a missing counter was treated as zero", s.minY)
	}
}

// A flat series is the normal state of a video nobody is watching this week.
// It must not divide by zero, and must not be drawn along an edge as though it
// were at an extreme.
func TestFlatSeriesDrawsThroughTheMiddle(t *testing.T) {
	s := newSeries(snaps(u(500), u(500), u(500)))

	y := s.scaleY(500, 0, 100)
	if y != 50 {
		t.Errorf("flat series drawn at y=%v, want the midpoint 50", y)
	}

	got := string(Sparkline(snaps(u(500), u(500), u(500))))
	if strings.Contains(got, "NaN") || strings.Contains(got, "Inf") {
		t.Errorf("flat series produced invalid coordinates: %s", got)
	}
}

func TestChartHasScaleAndMarkers(t *testing.T) {
	got := string(Chart(snaps(u(100), u(150), u(190), u(300))))

	if !strings.Contains(got, "chart-line") {
		t.Fatalf("no line: %s", got)
	}
	// Without a scale a rising line says nothing about how much it rose.
	if n := strings.Count(got, `class="grid"`); n < 4 {
		t.Errorf("%d gridlines, want a labelled scale", n)
	}
	if n := strings.Count(got, `class="axis"`); n < 5 {
		t.Errorf("%d axis labels", n)
	}
	// Individual readings are marked, so a chart of four points does not
	// pretend to the smoothness of one drawn from four hundred.
	if n := strings.Count(got, `class="chart-dot"`); n != 4 {
		t.Errorf("%d dots, want one per reading", n)
	}
}

func TestChartWithTooFewPointsExplainsItself(t *testing.T) {
	got := string(Chart(snaps(u(100))))

	if strings.Contains(got, "<svg") {
		t.Error("a chart was drawn from one reading")
	}
	if !strings.Contains(got, "Not enough readings") {
		t.Errorf("no explanation offered: %s", got)
	}
}

// Every value interpolated into the SVG is a number this package computed, and
// the one place text appears it is escaped. A stray quote would break out of an
// attribute.
func TestChartMarkupIsWellFormed(t *testing.T) {
	got := string(Chart(snaps(u(1), u(2), u(3))))

	if strings.Count(got, "<svg") != strings.Count(got, "</svg>") {
		t.Error("unbalanced svg tags")
	}
	if strings.Contains(got, "NaN") || strings.Contains(got, "+Inf") {
		t.Errorf("invalid coordinates: %s", got)
	}
	for _, attr := range regexp.MustCompile(`points="([^"]*)"`).FindAllStringSubmatch(got, -1) {
		for _, pair := range strings.Fields(attr[1]) {
			if strings.Count(pair, ",") != 1 {
				t.Errorf("malformed coordinate %q", pair)
			}
		}
	}
}

// Very large counters must not overflow into scientific notation inside a
// coordinate, which would produce markup a browser silently drops.
func TestChartHandlesLargeCounters(t *testing.T) {
	got := string(Chart(snaps(u(1), u(9_000_000_000))))

	if strings.Contains(got, "e+") {
		t.Errorf("scientific notation reached the markup: %s", got)
	}
}
