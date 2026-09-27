package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// ui renders terminal output. Colour is used only when stdout is an
// interactive terminal, -color and -tty are on, and NO_COLOR is unset.
type ui struct {
	color     bool
	truecolor bool
}

// newUI decides how to render for this process.
func newUI(cfg SwarmConfig) ui {
	_, noColor := os.LookupEnv("NO_COLOR")
	on := cfg.Color && cfg.TTY && !noColor && term.IsTerminal(int(os.Stdout.Fd()))
	ct := os.Getenv("COLORTERM")
	return ui{color: on, truecolor: on && (strings.Contains(ct, "truecolor") || strings.Contains(ct, "24bit"))}
}

// Synthwave '84 palette: RGB for truecolor terminals, a basic ANSI code
// for everything else.
type tone struct {
	r, g, b uint8
	basic   string
}

var (
	tonePink   = tone{255, 126, 219, "35"}
	toneCyan   = tone{54, 249, 246, "36"}
	toneYellow = tone{254, 222, 93, "33"}
	toneGreen  = tone{114, 241, 184, "32"}
	toneRed    = tone{254, 68, 80, "31"}
	toneOrange = tone{255, 139, 57, "33"}
	toneMuted  = tone{132, 139, 189, "90"}
)

// paint wraps s in the escape codes for t, or returns s unchanged.
func (u ui) paint(t tone, s string) string {
	if !u.color {
		return s
	}
	if u.truecolor {
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[0m", t.r, t.g, t.b, s)
	}
	return "\x1b[" + t.basic + "m" + s + "\x1b[0m"
}

// bold wraps s in bold, or returns s unchanged.
func (u ui) bold(s string) string {
	if !u.color {
		return s
	}
	return "\x1b[1m" + s + "\x1b[22m"
}

// wordmark renders LEMMINGS in a sunset gradient, pink to yellow.
func (u ui) wordmark() string {
	letters := []rune("L E M M I N G S")
	if !u.color {
		return string(letters)
	}
	var b strings.Builder
	for i, r := range letters {
		f := float64(i) / float64(len(letters)-1)
		t := tone{
			r:     uint8(249 + f*(254-249)),
			g:     uint8(42 + f*(222-42)),
			b:     uint8(173 + f*(93-173)),
			basic: map[bool]string{true: "35", false: "33"}[f < .5],
		}
		b.WriteString(u.paint(t, string(r)))
	}
	return u.bold(b.String())
}

// rule is the horizontal divider used throughout the output.
func (u ui) rule() string {
	return u.paint(toneMuted, "─────────────────────────────────────────")
}

// key renders a left-column label padded to width.
func (u ui) key(label string) string {
	return u.paint(toneMuted, fmt.Sprintf("  %-16s", label))
}

// printBootSummary writes the pre-run plan to STDOUT.
func (u ui) printBootSummary(cfg SwarmConfig) {
	total := cfg.Terrain * cfg.Pack

	fmt.Println()
	fmt.Printf("  %s %s %s   %s\n", u.paint(tonePink, "░▒▓"), u.wordmark(), u.paint(toneOrange, "▓▒░"), u.paint(toneMuted, "v"+cfg.Version))
	fmt.Println("  " + u.paint(toneMuted, "simulated visitors · real consequences"))
	fmt.Println(u.rule())
	fmt.Printf("%s%s\n", u.key("target:"), u.paint(toneCyan, cfg.Hit))
	fmt.Println()
	fmt.Printf("%s%s groups\n", u.key("terrain:"), formatInt(cfg.Terrain))
	fmt.Printf("%s%s lemmings per terrain\n", u.key("pack:"), formatInt(cfg.Pack))
	fmt.Printf("%s%s lemmings\n", u.key("total:"), u.bold(formatInt(total)))
	fmt.Println()
	fmt.Printf("%s%s per lemming\n", u.key("until:"), cfg.Until)
	fmt.Printf("%s%s to full concurrency\n", u.key("ramp:"), cfg.Ramp)
	fmt.Printf("%s~%s wall clock\n", u.key("est. duration:"), estimateWallClock(cfg).Round(time.Second))
	fmt.Println()

	limitStr := fmt.Sprintf("%d lemmings at once", cfg.Limit)
	if cfg.Limit == -1 {
		limitStr = u.paint(toneRed, "UNLIMITED (⚠ danger zone)")
	}
	fmt.Printf("%s%s\n", u.key("limit:"), limitStr)
	if cfg.Journey != nil {
		fmt.Printf("%s%s (%d steps)\n", u.key("journey:"), cfg.Journey.Name, len(cfg.Journey.Steps))
	} else {
		fmt.Printf("%s%s\n", u.key("navigation:"), cfg.Navigation)
	}
	fmt.Printf("%s%s – %s between pages\n", u.key("think:"), cfg.ThinkMin, cfg.ThinkMax)
	if cfg.MaxPages > 0 {
		fmt.Printf("%s%d per lemming\n", u.key("max pages:"), cfg.MaxPages)
	}
	fmt.Printf("%s%v (depth: %d)\n", u.key("crawl:"), cfg.Crawl, cfg.CrawlDepth)
	fmt.Printf("%s\n", u.key("save-to:"))
	for _, dest := range cfg.SaveTo {
		fmt.Printf("    → %s\n", dest)
	}
	fmt.Printf("%s%v\n", u.key("tty:"), cfg.TTY)
	fmt.Printf("%s%v\n", u.key("observe:"), cfg.Observe)
	if cfg.Observe {
		fmt.Printf("%shttp://localhost:%d/metrics\n", u.key("metrics:"), cfg.MetricsPort)
		fmt.Printf("%s%s\n", u.key("url-label:"), cfg.MetricsURLLabel)
	}
	var gates []string
	if cfg.FailureGate {
		gates = append(gates, fmt.Sprintf("failure rate ≤ %.2f%%", cfg.MaxFailureRate*100))
	}
	if cfg.P95Budget > 0 {
		gates = append(gates, "p95 ≤ "+cfg.P95Budget.String())
	}
	if len(gates) > 0 {
		fmt.Printf("%s%s\n", u.key("gates:"), strings.Join(gates, ", "))
	}
	fmt.Println()

	warned := false
	warn := func(format string, args ...any) {
		fmt.Println(u.paint(toneYellow, fmt.Sprintf(format, args...)))
		warned = true
	}
	if total > warnTotalLemmings {
		warn("  ⚠  WARNING: %s total lemmings is a large run.\n              ensure your system and target can handle this load.", formatInt(total))
	}
	if cfg.Terrain > warnTotalGoroutines {
		warn("  ⚠  WARNING: %s terrain groups means %s scheduler goroutines\n              before a single lemming is born.", formatInt(cfg.Terrain), formatInt(cfg.Terrain))
	}
	if cfg.Limit == -1 {
		warn("  ⚠  WARNING: -limit=-1 disables the semaphore entirely.\n              %s lemmings may run simultaneously.", formatInt(total))
	}
	if cfg.ThinkMax == 0 {
		warn("  ⚠  think time is zero: lemmings will not pause between pages.\n              this is a saturation test, not a model of real visitors.")
	}
	if warned {
		fmt.Println()
	}

	fmt.Println(u.rule())
	fmt.Println("  lemmings are gathering...")
	fmt.Println()
}

// estimateWallClock predicts how long a run takes: the ramp, plus as many
// back-to-back lifespans as the concurrency limit forces.
func estimateWallClock(cfg SwarmConfig) time.Duration {
	total := max(cfg.Terrain*cfg.Pack, 1)
	limit := int64(cfg.Limit)
	if limit <= 0 || limit > total {
		limit = total
	}
	waves := (total + limit - 1) / limit
	return cfg.Ramp + time.Duration(waves)*cfg.Until
}

// printDashboard announces the dashboard. The token travels in the URL
// fragment, which browsers never send to a server, and the dashboard
// removes it from the address bar as soon as it has signed in.
func (u ui) printDashboard(cfg SwarmConfig, token string) {
	link := fmt.Sprintf("http://localhost:%d/#token=%s", cfg.DashboardPort, token)
	fmt.Printf("%s%s\n", u.key("dashboard:"), u.paint(toneCyan, link))
	fmt.Printf("%s%s\n\n", u.key("token:"), u.paint(toneMuted, token))
}

// ansiPattern matches terminal escape sequences, for measuring width.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// visibleLen returns the printed width of s, ignoring escape codes.
func visibleLen(s string) int {
	return len([]rune(ansiPattern.ReplaceAllString(s, "")))
}

// tickSTDOUT prints live metrics every second until done is closed.
// Uses \r in TTY mode to overwrite the line; newline in CI mode.
func (s *Swarm) tickSTDOUT(done <-chan struct{}) {
	u := newUI(s.cfg)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var lastVisits int64
	rates := make([]float64, 0, 12)

	for {
		select {
		case <-done:
			if s.cfg.TTY {
				fmt.Print("\r\x1b[K")
			}
			return
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			visits := s.metrics.TotalVisits.Load()
			rates = append(rates, float64(visits-lastVisits))
			if len(rates) > 12 {
				rates = rates[1:]
			}
			lastVisits = visits

			line := s.tickerLine(u, visits, rates)
			if s.cfg.TTY {
				if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 10 && visibleLen(line) >= w {
					line = ansiPattern.ReplaceAllString(line, "")
					line = string([]rune(line)[:w-2])
				}
				fmt.Print("\r\x1b[K" + line)
			} else {
				fmt.Println(line)
			}
			_ = os.Stdout.Sync()
		}
	}
}

// tickerLine renders one live status line.
func (s *Swarm) tickerLine(u ui, visits int64, rates []float64) string {
	m := &s.metrics
	elapsed := time.Since(s.startedAt).Round(time.Second)
	rate := 0.0
	if len(rates) > 0 {
		rate = rates[len(rates)-1]
	}
	live := s.reporter.Live(0, 0)

	line := fmt.Sprintf("  [%s] %s %s/s | terrains: %d | alive: %s | done: %s | visits: %s | 2xx: %s 4xx: %s 5xx: %s | p95: %s",
		elapsed,
		u.paint(toneCyan, miniSpark(rates)),
		formatInt(int64(rate)),
		m.TerrainsOnline.Load(),
		u.paint(tonePink, formatInt(m.LemmingsAlive.Load())),
		formatInt(m.LemmingsCompleted.Load()),
		formatInt(visits),
		u.paint(toneGreen, formatInt(m.Total2xx.Load())),
		u.paint(toneYellow, formatInt(m.Total4xx.Load())),
		u.paint(toneRed, formatInt(m.Total5xx.Load())),
		formatMillis(live.P95),
	)
	if failed := m.FailedVisits.Load(); failed > 0 {
		line += " | " + u.paint(toneRed, "failed: "+formatInt(failed))
	}
	if wr := m.TotalWaitingRoom.Load(); wr > 0 {
		line += " | " + u.paint(toneOrange, "queued: "+formatInt(wr))
	}
	// Only show overflow/dropped if they're non-zero —
	// no need to alarm engineers who sized their runs correctly
	if overflow := m.OverflowLogs.Load(); overflow > 0 {
		line += fmt.Sprintf(" | overflow: %s", formatInt(overflow))
	}
	if dropped := m.DroppedLogs.Load(); dropped > 0 {
		line += u.paint(toneYellow, fmt.Sprintf(" | ⚠ dropped: %s", formatInt(dropped)))
	}
	return line
}

// miniSpark renders recent per-second rates as block characters.
func miniSpark(rates []float64) string {
	var top float64
	for _, r := range rates {
		top = max(top, r)
	}
	var b strings.Builder
	for _, r := range rates {
		i := 0
		if top > 0 {
			i = int(r / top * float64(len(sparkBlocks)-1))
		}
		b.WriteRune(sparkBlocks[i])
	}
	return b.String()
}

// printFinalSummary writes the post-run block to STDOUT.
func (s *Swarm) printFinalSummary() {
	u := newUI(s.cfg)
	d := s.reporter.Result()
	m := &s.metrics
	elapsed := time.Since(s.startedAt).Round(time.Second)

	fmt.Printf("\n  all lemmings have died. elapsed: %s\n", elapsed)
	fmt.Println()
	fmt.Println(u.rule())
	fmt.Printf("  lemmings v%s — final summary\n", s.cfg.Version)
	fmt.Println(u.rule())
	fmt.Printf("%s%s\n", u.key("target:"), s.cfg.Hit)
	fmt.Printf("%s%s\n", u.key("total lemmings:"), formatInt(s.cfg.Terrain*s.cfg.Pack))
	fmt.Printf("%s%s\n", u.key("completed:"), formatInt(m.LemmingsCompleted.Load()))
	fmt.Printf("%s%s\n", u.key("failed:"), formatInt(m.LemmingsFailed.Load()))
	fmt.Println()
	fmt.Printf("%s%s  %s\n", u.key("total visits:"), formatInt(d.TotalVisits), u.paint(toneCyan, sparkline(d.Timeline)))
	fmt.Printf("%s%s\n", u.key("total bytes:"), formatBytes(d.TotalBytes))
	fmt.Printf("%s%s (%.2f%%)\n", u.key("failed visits:"), formatInt(d.FailedVisits), d.FailureRate*100)
	if d.CancelledVisits > 0 {
		fmt.Printf("%s%s (life ended mid-visit)\n", u.key("cancelled:"), formatInt(d.CancelledVisits))
	}
	fmt.Println()
	fmt.Printf("%s%s\n", u.key("2xx:"), u.paint(toneGreen, formatInt(d.Total2xx)))
	fmt.Printf("%s%s\n", u.key("3xx:"), u.paint(toneCyan, formatInt(d.Total3xx)))
	fmt.Printf("%s%s\n", u.key("4xx:"), u.paint(toneYellow, formatInt(d.Total4xx)))
	fmt.Printf("%s%s\n", u.key("5xx:"), u.paint(toneRed, formatInt(d.Total5xx)))
	fmt.Println()
	fmt.Printf("%sp50 %s · p95 %s · p99 %s\n", u.key("latency:"), formatMillis(d.P50), formatMillis(d.P95), formatMillis(d.P99))
	fmt.Printf("%s%s lemmings held\n", u.key("waiting room:"), formatInt(d.Room.Held))
	fmt.Println()

	// Only surface channel pressure metrics if they occurred
	if overflow, dropped := m.OverflowLogs.Load(), m.DroppedLogs.Load(); overflow > 0 || dropped > 0 {
		fmt.Println("  channel pressure:")
		fmt.Printf("    overflow logs:  %s\n", formatInt(overflow))
		fmt.Printf("    dropped logs:   %s\n", formatInt(dropped))
		if dropped > 0 {
			fmt.Println()
			fmt.Println(u.paint(toneYellow, "  ⚠  life records were dropped because the collector fell behind."))
			fmt.Println(u.paint(toneYellow, "     visit totals are still exact; only per-lemming detail was lost."))
		}
		fmt.Println()
	}
	fmt.Println(u.rule())
}

// printVerdict ends the run with the game's own scorecard.
func (u ui) printVerdict(d ReportData, interrupted bool) {
	fmt.Println()
	score := fmt.Sprintf("%.1f%%", d.RescuedPercent)
	switch {
	case d.RescuedPercent >= 99.95:
		score = u.paint(toneGreen, score)
	case d.RescuedPercent >= 50:
		score = u.paint(toneYellow, score)
	default:
		score = u.paint(toneRed, score)
	}
	fmt.Printf("  %s\n", u.bold(rescueLine(d)))
	fmt.Printf("  you rescued %s — %s of %s lemmings lived without a failed page.\n",
		u.bold(score), formatInt(d.Rescued), formatInt(d.Sessions))
	for _, g := range d.Gates {
		mark := u.paint(toneGreen, "✓")
		if !g.Passed {
			mark = u.paint(toneRed, "✗")
		}
		fmt.Printf("  %s %s %s (limit %s)\n", mark, g.Name, g.Observed, g.Limit)
	}
	switch {
	case interrupted:
		fmt.Println("  " + u.paint(toneYellow, "■ interrupted — the partial report above was still saved."))
	case d.Verdict == "FAILED":
		fmt.Println("  " + u.paint(toneRed, u.bold("✗ FAILED")) + u.paint(toneMuted, " — exit code 2"))
	case d.Verdict == "PASSED":
		fmt.Println("  " + u.paint(toneGreen, u.bold("✓ PASSED")))
	}
	fmt.Println()
}

// formatTotal renders terrain × pack with thousands separators.
func formatTotal(terrain, pack int64) string {
	return formatInt(terrain * pack)
}

// formatInt renders n with thousands separators: 1234567 → 1,234,567.
func formatInt(n int64) string {
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
