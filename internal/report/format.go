// Package report aggregates streaming swarm telemetry, latency distributions,
// timelines, trace logs, and redacted error forensics into HTML, Markdown, and
// JSON reports, and compares JSON reports across deploys for regression gating.
package report

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// SparkBlocks are the eight vertical bar runes used for terminal and Markdown sparklines.
var SparkBlocks = []rune("▁▂▃▄▅▆▇█")

// Millis converts d to fractional milliseconds.
func Millis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// FormatMillis renders a duration in milliseconds with sensible precision.
func FormatMillis(d time.Duration) string {
	ms := Millis(d)
	switch {
	case d == 0:
		return "0 ms"
	case ms < 1:
		return fmt.Sprintf("%.2f ms", ms)
	case ms < 100:
		return fmt.Sprintf("%.1f ms", ms)
	case ms < 10_000:
		return fmt.Sprintf("%.0f ms", ms)
	}
	return d.Round(100 * time.Millisecond).String()
}

// RoundDuration rounds d to seconds above 10s and milliseconds below.
func RoundDuration(d time.Duration) time.Duration {
	if d >= 10*time.Second {
		return d.Round(time.Second)
	}
	return d.Round(time.Millisecond)
}

// FormatInt renders n with thousands separators (e.g. -1234567 -> "-1,234,567").
func FormatInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	sign := ""
	if s[0] == '-' {
		sign, s = "-", s[1:]
	}
	if len(s) <= 3 {
		return sign + s
	}
	var b strings.Builder
	b.WriteString(sign)
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > len(sign) {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// FormatBytes renders a byte count as human-readable B/KB/MB/GB.
func FormatBytes(b int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.2f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.2f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.2f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// StatusClass returns "2xx", "3xx", "4xx", "5xx", or "unknown".
func StatusClass(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500 && code < 600:
		return "5xx"
	default:
		return "unknown"
	}
}

// Sparkline renders visits/sec across points as at most 60 block characters.
func Sparkline(points []TimelinePoint) string {
	if len(points) == 0 {
		return ""
	}
	const width = 60
	values := make([]float64, 0, width)
	step := math.Max(1, float64(len(points))/width)
	for x := 0.0; int(x) < len(points); x += step {
		lo, hi := int(x), min(int(x+step), len(points))
		var sum float64
		for _, p := range points[lo:max(hi, lo+1)] {
			sum += p.RatePerSecond
		}
		values = append(values, sum/float64(max(hi-lo, 1)))
	}
	var top float64
	for _, v := range values {
		top = math.Max(top, v)
	}
	var b strings.Builder
	for _, v := range values {
		i := 0
		if top > 0 {
			i = int(math.Round(v / top * float64(len(SparkBlocks)-1)))
		}
		b.WriteRune(SparkBlocks[i])
	}
	return b.String()
}

// RescueLine returns the game-inspired end-of-run verdict headline.
func RescueLine(d Data) string {
	switch {
	case d.Sessions == 0:
		return "No lemmings made it home to report."
	case d.Rescued == d.Sessions:
		return "All lemmings accounted for."
	case d.RescuedPercent >= 90:
		return "So close. A few lemmings hit trouble."
	case d.RescuedPercent >= 50:
		return "Oh no! Many lemmings hit trouble."
	}
	return "Oh no! Most lemmings hit trouble."
}

// Percentile computes the nearest-rank Nth percentile of a pre-sorted slice.
func Percentile(sorted []time.Duration, n int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(float64(n)/100.0*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
