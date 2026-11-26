package report

import (
	"fmt"
	htmltemplate "html/template"
	"math"
	"strings"
	"time"
)

const (
	chartW      = 1000.0
	chartH      = 220.0
	chartLeft   = 56.0
	chartRight  = 64.0
	chartTop    = 30.0
	chartBottom = 28.0
)

type chartSeries struct {
	values []float64
	class  string
	kind   string // area, line, dash, bars
	right  bool
}

// TimelineSVG renders traffic and latency charts as static inline SVG.
func TimelineSVG(points []TimelinePoint) htmltemplate.HTML {
	if len(points) < 2 {
		return htmltemplate.HTML(`<p class="muted">The run was too short to chart.</p>`)
	}
	xs := make([]float64, len(points))
	rate := make([]float64, len(points))
	fail := make([]float64, len(points))
	alive := make([]float64, len(points))
	queued := make([]float64, len(points))
	p50 := make([]float64, len(points))
	p95 := make([]float64, len(points))
	width := 1.0
	if len(points) > 1 {
		width = points[1].OffsetSeconds - points[0].OffsetSeconds
		if width <= 0 {
			width = 1.0
		}
	}
	hasQueued := false
	for i, p := range points {
		xs[i] = p.OffsetSeconds
		rate[i] = p.RatePerSecond
		fail[i] = float64(p.Failed) / width
		alive[i] = float64(p.Alive)
		queued[i] = float64(p.Queued)
		if p.Queued > 0 {
			hasQueued = true
		}
		p50[i] = p.P50Millis
		p95[i] = p.P95Millis
	}

	trafficSeries := []chartSeries{
		{values: rate, class: "s-rate", kind: "area"},
		{values: fail, class: "s-fail", kind: "bars"},
		{values: alive, class: "s-alive", kind: "dash", right: true},
	}
	rightUnit := "alive"
	if hasQueued {
		trafficSeries = append(trafficSeries, chartSeries{values: queued, class: "s-queued", kind: "dash", right: true})
		rightUnit = "alive / queued"
	}

	var b strings.Builder
	b.WriteString(chart("Traffic over time", "visits/s", rightUnit, xs, trafficSeries...))
	b.WriteString(chart("Latency over time", "ms", "", xs,
		chartSeries{values: p95, class: "s-p95", kind: "line"},
		chartSeries{values: p50, class: "s-p50", kind: "line"},
	))
	return htmltemplate.HTML(b.String())
}

func chart(title, leftUnit, rightUnit string, xs []float64, series ...chartSeries) string {
	plotW := chartW - chartLeft - chartRight
	plotH := chartH - chartTop - chartBottom
	xMax := xs[len(xs)-1]
	if xMax <= 0 {
		xMax = 1
	}
	var leftMax, rightMax float64
	for _, s := range series {
		for _, v := range s.values {
			if s.right {
				rightMax = math.Max(rightMax, v)
			} else {
				leftMax = math.Max(leftMax, v)
			}
		}
	}
	leftTicks, leftTop := niceTicks(leftMax)
	rightTicks, rightTop := niceTicks(rightMax)

	x := func(sec float64) float64 { return chartLeft + sec/xMax*plotW }
	y := func(v, top float64) float64 { return chartTop + plotH - v/top*plotH }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, chartW, chartH, title)
	fmt.Fprintf(&b, `<title>%s</title>`, title)

	for _, t := range leftTicks {
		yy := y(t, leftTop)
		fmt.Fprintf(&b, `<line class="grid" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, chartLeft, chartW-chartRight, yy, yy)
		fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="%.1f" text-anchor="end">%s</text>`, chartLeft-8, yy+4, tickLabel(t))
	}
	if rightUnit != "" && rightMax > 0 {
		for _, t := range rightTicks {
			fmt.Fprintf(&b, `<text class="axis axis-r" x="%.1f" y="%.1f">%s</text>`, chartW-chartRight+8, y(t, rightTop)+4, tickLabel(t))
		}
		fmt.Fprintf(&b, `<text class="unit axis-r" x="%.1f" y="12" text-anchor="end">%s</text>`, chartW-2, rightUnit)
	}
	fmt.Fprintf(&b, `<text class="unit" x="2" y="12">%s</text>`, leftUnit)

	for _, t := range timeTicks(xMax) {
		fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, x(t), chartH-8, formatOffset(t))
	}

	barW := math.Max(plotW/float64(len(xs))-1, 1)
	for _, s := range series {
		top := leftTop
		if s.right {
			top = rightTop
		}
		switch s.kind {
		case "bars":
			for i, v := range s.values {
				if v <= 0 {
					continue
				}
				yy := y(v, top)
				fmt.Fprintf(&b, `<rect class="%s" x="%.1f" y="%.1f" width="%.1f" height="%.1f"/>`,
					s.class, x(xs[i])-barW/2, yy, barW, chartTop+plotH-yy)
			}
		default:
			var pts strings.Builder
			for i, v := range s.values {
				fmt.Fprintf(&pts, "%.1f,%.1f ", x(xs[i]), y(v, top))
			}
			line := strings.TrimSpace(pts.String())
			if s.kind == "area" {
				fmt.Fprintf(&b, `<polygon class="%s area" points="%.1f,%.1f %s %.1f,%.1f"/>`,
					s.class, x(xs[0]), chartTop+plotH, line, x(xs[len(xs)-1]), chartTop+plotH)
			}
			cls := s.class + " line"
			if s.kind == "dash" {
				cls += " dash"
			}
			fmt.Fprintf(&b, `<polyline class="%s" points="%s"/>`, cls, line)
		}
	}
	b.WriteString(`</svg>`)
	return b.String()
}

func niceTicks(maxV float64) ([]float64, float64) {
	if maxV <= 0 {
		return []float64{0, 0.5, 1}, 1
	}
	raw := maxV / 4
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	var step float64
	switch r := raw / mag; {
	case r <= 1:
		step = mag
	case r <= 2:
		step = 2 * mag
	case r <= 5:
		step = 5 * mag
	default:
		step = 10 * mag
	}
	top := math.Ceil(maxV/step) * step
	var ticks []float64
	for t := 0.0; t <= top+step/2; t += step {
		ticks = append(ticks, t)
	}
	return ticks, top
}

func timeTicks(maxSec float64) []float64 {
	steps := []float64{1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200}
	step := steps[len(steps)-1]
	for _, s := range steps {
		if maxSec/s <= 6 {
			step = s
			break
		}
	}
	var ticks []float64
	for t := 0.0; t <= maxSec+0.001; t += step {
		ticks = append(ticks, t)
	}
	return ticks
}

func tickLabel(v float64) string {
	switch {
	case v >= 1_000_000:
		return trimZero(fmt.Sprintf("%.1f", v/1_000_000)) + "M"
	case v >= 10_000:
		return trimZero(fmt.Sprintf("%.1f", v/1_000)) + "k"
	case v >= 10 || v == 0:
		return fmt.Sprintf("%.0f", v)
	}
	return trimZero(fmt.Sprintf("%.2f", v))
}

func trimZero(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func formatOffset(sec float64) string {
	d := time.Duration(sec * float64(time.Second)).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m, s := int(d.Minutes()), int(d.Seconds())%60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}
