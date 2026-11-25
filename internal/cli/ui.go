// Package cli renders Lemmings' Synthwave '84 terminal interface: boot
// summary, live 1-second ticker, dry-run inspection, review-mode prompts,
// post-run scorecard, and JSON report comparison output.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/report"
	"github.com/andreimerlescu/lemmings/v2/internal/swarm"
	"golang.org/x/term"
)

const (
	// WarnTotalLemmings triggers a pre-run caution banner above this total.
	WarnTotalLemmings int64 = 100_000
	// WarnTotalGoroutines triggers a pre-run caution banner above this terrain count.
	WarnTotalGoroutines int64 = 10_000
)

// UI renders terminal output to out. Color is enabled only when out is a TTY,
// -color and -tty are true, and NO_COLOR is unset.
type UI struct {
	out       io.Writer
	file      *os.File
	tty       bool
	color     bool
	truecolor bool
}

// New constructs a UI targeting out (defaults to os.Stdout if nil).
func New(cfg swarm.Config, out io.Writer) *UI {
	if out == nil {
		out = os.Stdout
	}
	f, _ := out.(*os.File)
	isTerm := f != nil && term.IsTerminal(int(f.Fd()))
	_, noColor := os.LookupEnv("NO_COLOR")
	colorOn := cfg.Color && cfg.TTY && !noColor && isTerm && ansiCapable()
	ct := os.Getenv("COLORTERM")
	return &UI{
		out:       out,
		file:      f,
		tty:       cfg.TTY && isTerm,
		color:     colorOn,
		truecolor: colorOn && (strings.Contains(ct, "truecolor") || strings.Contains(ct, "24bit")),
	}
}

func ansiCapable() bool {
	if runtime.GOOS != "windows" {
		return true
	}
	for _, v := range []string{"WT_SESSION", "ANSICON", "ConEmuANSI", "TERM"} {
		if os.Getenv(v) != "" {
			return true
		}
	}
	return false
}

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

func (u *UI) paint(t tone, s string) string {
	if !u.color {
		return s
	}
	if u.truecolor {
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[0m", t.r, t.g, t.b, s)
	}
	return "\x1b[" + t.basic + "m" + s + "\x1b[0m"
}

func (u *UI) bold(s string) string {
	if !u.color {
		return s
	}
	return "\x1b[1m" + s + "\x1b[22m"
}

func (u *UI) wordmark() string {
	letters := []rune("L E M M I N G S")
	if !u.color {
		return string(letters)
	}
	var b strings.Builder
	for i, r := range letters {
		f := float64(i) / float64(len(letters)-1)
		basic := "33"
		if f < 0.5 {
			basic = "35"
		}
		t := tone{
			r:     uint8(249 + f*(254-249)),
			g:     uint8(42 + f*(222-42)),
			b:     uint8(173 + f*(93-173)),
			basic: basic,
		}
		b.WriteString(u.paint(t, string(r)))
	}
	return u.bold(b.String())
}

func (u *UI) rule() string {
	return u.paint(toneMuted, "─────────────────────────────────────────────────────")
}

func (u *UI) key(label string) string {
	return u.paint(toneMuted, fmt.Sprintf("  %-18s", label))
}

func (u *UI) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(u.out, format, args...)
}

func (u *UI) println(args ...any) {
	_, _ = fmt.Fprintln(u.out, args...)
}

// PrintBootSummary writes the pre-run execution plan to out.
func (u *UI) PrintBootSummary(cfg swarm.Config, browserStatus string) {
	total := cfg.Terrain * cfg.Pack

	u.println()
	u.printf("  %s %s %s   %s\n", u.paint(tonePink, "░▒▓"), u.wordmark(), u.paint(toneOrange, "▓▒░"), u.paint(toneMuted, "v"+cfg.Version))
	u.println("  " + u.paint(toneMuted, "simulated visitors · real consequences · live forensics"))
	u.println(u.rule())
	u.printf("%s%s\n", u.key("target:"), u.paint(toneCyan, discovery.SafeURL(cfg.Hit)))
	u.println()
	u.printf("%s%s groups\n", u.key("terrain:"), report.FormatInt(cfg.Terrain))
	u.printf("%s%s lemmings per terrain\n", u.key("pack:"), report.FormatInt(cfg.Pack))
	u.printf("%s%s lemmings\n", u.key("total:"), u.bold(report.FormatInt(total)))
	u.println()
	u.printf("%s%s per lemming\n", u.key("until:"), cfg.Until)
	u.printf("%s%s (%s)\n", u.key("ramp:"), cfg.Ramp, cfg.RampShape)
	u.printf("%s~%s wall clock\n", u.key("est. duration:"), swarm.EstimateWallClock(cfg).Round(time.Second))
	u.println()

	limitStr := fmt.Sprintf("%d lemmings at once", cfg.Limit)
	if cfg.Limit == -1 {
		limitStr = u.paint(toneRed, "UNLIMITED (⚠ danger zone)")
	}
	u.printf("%s%s\n", u.key("limit:"), limitStr)
	if cfg.Journey != nil {
		u.printf("%s%s (%d steps)\n", u.key("journey:"), cfg.Journey.Name, len(cfg.Journey.Steps))
	} else {
		u.printf("%s%s\n", u.key("navigation:"), cfg.Navigation)
	}
	u.printf("%s%s – %s between pages\n", u.key("think:"), cfg.ThinkMin, cfg.ThinkMax)
	if cfg.MaxPages > 0 {
		u.printf("%s%d per lemming\n", u.key("max pages:"), cfg.MaxPages)
	}
	if cfg.Assets {
		u.printf("%senabled (up to %d uncached per page)\n", u.key("assets:"), cfg.AssetsPerPage)
	}
	u.printf("%s%v (depth: %d)\n", u.key("crawl:"), cfg.Crawl, cfg.CrawlDepth)

	// Forensics & Scouts summary.
	if cfg.CaptureEnabled {
		u.printf("%s%s (max %d, %d/err, %d workers, %s)\n",
			u.key("forensics:"),
			u.paint(toneCyan, browserStatus),
			cfg.CaptureMax, cfg.CapturePerError, cfg.CaptureWorkers, cfg.CaptureFormat)
	} else {
		u.printf("%s%s\n", u.key("forensics:"), u.paint(toneMuted, browserStatus))
	}
	if cfg.AllowRemoteCDP {
		u.printf("%s%s\n", u.key("remote cdp:"), u.paint(toneYellow, "enabled (-allow-remote-cdp)"))
	}
	if cfg.Scouts > 0 {
		u.printf("%s%d real-browser scouts\n", u.key("scouts:"), cfg.Scouts)
	}
	if cfg.ReviewMode {
		u.printf("%s%s\n", u.key("review mode:"), u.paint(toneYellow, "hold portal open after run until finalized"))
	}

	u.println(u.paint(toneMuted, "  save-to:"))
	for _, dest := range cfg.SaveTo {
		u.printf("    → %s\n", dest)
	}
	u.printf("%s%v\n", u.key("tty:"), cfg.TTY)
	u.printf("%s%v\n", u.key("observe:"), cfg.Observe)
	if cfg.Observe {
		u.printf("%shttp://127.0.0.1:%d/metrics\n", u.key("metrics:"), cfg.MetricsPort)
		u.printf("%s%s\n", u.key("url-label:"), cfg.MetricsURLLabel)
	}

	var gates []string
	if cfg.FailureGate {
		gates = append(gates, fmt.Sprintf("failure rate ≤ %.2f%%", cfg.MaxFailureRate*100))
	}
	if cfg.P95Budget > 0 {
		gates = append(gates, "p95 ≤ "+cfg.P95Budget.String())
	}
	if len(gates) > 0 {
		u.printf("%s%s\n", u.key("gates:"), strings.Join(gates, ", "))
	}
	u.println()

	warned := false
	warn := func(format string, args ...any) {
		u.println(u.paint(toneYellow, fmt.Sprintf(format, args...)))
		warned = true
	}
	if total > WarnTotalLemmings {
		warn("  ⚠  WARNING: %s total lemmings is a large run.\n              ensure your system and target can handle this load.", report.FormatInt(total))
	}
	if cfg.Terrain > WarnTotalGoroutines {
		warn("  ⚠  WARNING: %s terrain groups means %s scheduler goroutines\n              before a single lemming is born.", report.FormatInt(cfg.Terrain), report.FormatInt(cfg.Terrain))
	}
	if cfg.Limit == -1 {
		warn("  ⚠  WARNING: -limit=-1 disables the semaphore entirely.\n              %s lemmings may run simultaneously.", report.FormatInt(total))
	}
	if cfg.ThinkMax == 0 {
		warn("  ⚠  think time is zero: lemmings will not pause between pages.\n              this is a saturation test, not a model of real visitors.")
	}
	if cfg.RevealSessionCookies {
		warn("  ⚠  -reveal-session-cookies is ON: raw session cookie values will be\n              visible inside the authenticated localhost portal (never written to reports).")
	}
	if cfg.AllowRemoteCDP {
		warn("  ⚠  -allow-remote-cdp is ON: non-loopback -cdp-url endpoints are permitted\n              (link-local, multicast, unspecified, and cloud-metadata IPs remain blocked).")
	}
	if warned {
		u.println()
	}

	u.println(u.rule())
	u.println("  lemmings are gathering...")
	u.println()
}

// PrintProgress emits a single pre-run discovery or ramp notice.
func (u *UI) PrintProgress(msg string) {
	u.printf("  %s %s\n", u.paint(toneCyan, "·"), msg)
}

// PrintDashboard announces the authenticated dashboard URL (token in fragment).
func (u *UI) PrintDashboard(cfg swarm.Config, rawToken string) {
	link := fmt.Sprintf("http://localhost:%d/#token=%s", cfg.DashboardPort, rawToken)
	u.printf("%s%s\n", u.key("dashboard:"), u.paint(toneCyan, link))
	u.printf("%s%s\n\n", u.key("token:"), u.paint(toneMuted, rawToken))
}

// PrintReviewPrompt tells the admin the swarm finished and is waiting for triage.
func (u *UI) PrintReviewPrompt(port int, artifactCount int64) {
	u.println()
	u.println(u.rule())
	u.printf("  %s  %s\n", u.paint(toneYellow, "◉ REVIEW MODE"), u.bold("swarm finished — portal open for artifact triage"))
	u.printf("  Inspect %s captured artifact(s) at %s\n",
		u.bold(report.FormatInt(artifactCount)),
		u.paint(toneCyan, fmt.Sprintf("http://localhost:%d/", port)))
	u.println("  Click \"Finalize & Write Report\" in the portal, or press Ctrl-C here to save.")
	u.println(u.rule())
}

// PrintDryRun prints the validated plan, discovered URL pool, and browser probe.
func (u *UI) PrintDryRun(cfg swarm.Config, pool *discovery.URLPool, browserStatus string) {
	u.PrintBootSummary(cfg, browserStatus)
	u.println(u.bold("  DRY RUN — no swarm traffic sent"))
	u.println()
	u.printf("%s%s\n", u.key("origin:"), u.paint(toneCyan, pool.Origin))
	u.printf("%s%d URLs indexed\n", u.key("url pool:"), len(pool.URLs))
	limit := min(len(pool.URLs), 25)
	for i := range limit {
		u.printf("    %2d. %s\n", i+1, discovery.SafeURL(pool.URLs[i]))
	}
	if len(pool.URLs) > limit {
		u.printf("    ... and %d more URLs\n", len(pool.URLs)-limit)
	}
	if cfg.Journey != nil {
		u.println()
		u.printf("%s%s (%d steps)\n", u.key("journey:"), cfg.Journey.Name, len(cfg.Journey.Steps))
		for i, st := range cfg.Journey.Steps {
			u.printf("    %2d. %-18s %s\n", i+1, st.Name, st.Path)
		}
	}
	u.println()
	u.println(u.rule())
	u.println("  " + u.paint(toneGreen, "✓ dry run valid — remove -dry-run to unleash the swarm."))
	u.println()
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func visibleLen(s string) int {
	return len([]rune(ansiPattern.ReplaceAllString(s, "")))
}

// Tick renders live metrics every second until done is closed or ctx cancels.
func (u *UI) Tick(
	ctx context.Context,
	done <-chan struct{},
	startedAt time.Time,
	m *swarm.Metrics,
	liveFn func() report.LiveStats,
) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var lastVisits int64
	rates := make([]float64, 0, 12)

	for {
		select {
		case <-done:
			if u.tty {
				u.printf("\r\x1b[K")
			}
			return
		case <-ctx.Done():
			if u.tty {
				u.printf("\r\x1b[K")
			}
			return
		case <-ticker.C:
			visits := m.TotalVisits.Load()
			rates = append(rates, float64(visits-lastVisits))
			if len(rates) > 12 {
				rates = rates[1:]
			}
			lastVisits = visits

			var live report.LiveStats
			if liveFn != nil {
				live = liveFn()
			}
			line := u.tickerLine(startedAt, m, visits, rates, live)
			if u.tty && u.file != nil {
				if w, _, err := term.GetSize(int(u.file.Fd())); err == nil && w > 10 && visibleLen(line) >= w {
					plain := ansiPattern.ReplaceAllString(line, "")
					line = string([]rune(plain)[:w-2])
				}
				u.printf("\r\x1b[K%s", line)
			} else {
				u.println(line)
			}
			if u.file != nil {
				_ = u.file.Sync()
			}
		}
	}
}

func (u *UI) tickerLine(startedAt time.Time, m *swarm.Metrics, visits int64, rates []float64, live report.LiveStats) string {
	elapsed := time.Since(startedAt).Round(time.Second)
	rate := 0.0
	if len(rates) > 0 {
		rate = rates[len(rates)-1]
	}

	line := fmt.Sprintf("  [%s] %s %s/s | terrains: %d | alive: %s | done: %s | visits: %s | 2xx: %s 4xx: %s 5xx: %s | p95: %s",
		elapsed,
		u.paint(toneCyan, miniSpark(rates)),
		report.FormatInt(int64(rate)),
		m.TerrainsOnline.Load(),
		u.paint(tonePink, report.FormatInt(m.LemmingsAlive.Load())),
		report.FormatInt(m.LemmingsCompleted.Load()),
		report.FormatInt(visits),
		u.paint(toneGreen, report.FormatInt(m.Total2xx.Load())),
		u.paint(toneYellow, report.FormatInt(m.Total4xx.Load())),
		u.paint(toneRed, report.FormatInt(m.Total5xx.Load())),
		report.FormatMillis(live.P95),
	)
	if failed := m.FailedVisits.Load(); failed > 0 {
		line += " | " + u.paint(toneRed, "failed: "+report.FormatInt(failed))
	}
	if arts := m.ArtifactsCaptured.Load(); arts > 0 {
		line += " | " + u.paint(toneCyan, "artifacts: "+report.FormatInt(arts))
	}
	if scouts := m.ScoutsAlive.Load(); scouts > 0 {
		line += " | " + u.paint(tonePink, "scouts: "+report.FormatInt(scouts))
	}
	if wr := m.TotalWaitingRoom.Load(); wr > 0 {
		line += " | " + u.paint(toneOrange, "room held: "+report.FormatInt(wr))
	}
	if polls := m.QueuePolls.Load(); polls > 0 {
		line += " | " + u.paint(toneOrange, "polls: "+report.FormatInt(polls))
	}
	if assets := m.AssetRequests.Load(); assets > 0 {
		line += " | " + u.paint(toneCyan, "assets: "+report.FormatInt(assets))
		if d403 := m.AssetDenied403.Load(); d403 > 0 {
			line += " " + u.paint(toneRed, "(403: "+report.FormatInt(d403)+")")
		}
	}
	if bans := m.Bans429.Load() + m.Bans403.Load(); bans > 0 {
		line += " | " + u.paint(toneRed, "bans: "+report.FormatInt(bans))
	}
	if overflow := m.OverflowLogs.Load(); overflow > 0 {
		line += fmt.Sprintf(" | overflow: %s", report.FormatInt(overflow))
	}
	if dropped := m.DroppedLogs.Load(); dropped > 0 {
		line += u.paint(toneYellow, fmt.Sprintf(" | ⚠ dropped: %s", report.FormatInt(dropped)))
	}
	return line
}

func miniSpark(rates []float64) string {
	var top float64
	for _, r := range rates {
		top = max(top, r)
	}
	var b strings.Builder
	for _, r := range rates {
		i := 0
		if top > 0 {
			i = int(r / top * float64(len(report.SparkBlocks)-1))
		}
		b.WriteRune(report.SparkBlocks[i])
	}
	return b.String()
}

// PrintFinalSummary writes the post-run telemetry and forensics summary.
func (u *UI) PrintFinalSummary(cfg swarm.Config, startedAt time.Time, m *swarm.Metrics, d report.Data) {
	elapsed := time.Since(startedAt).Round(time.Second)

	u.printf("\n  all lemmings have died. elapsed: %s\n\n", elapsed)
	u.println(u.rule())
	u.printf("  lemmings v%s — final summary\n", cfg.Version)
	u.println(u.rule())
	started := d.LemmingsStarted
	if started == 0 && m != nil {
		started = m.LemmingsStarted.Load()
	}
	completed := d.LemmingsCompleted
	if completed == 0 && m != nil {
		completed = m.LemmingsCompleted.Load()
	}
	unborn := d.LemmingsUnborn
	if unborn == 0 && m != nil {
		unborn = m.LemmingsFailed.Load()
	}

	u.printf("%s%s\n", u.key("target:"), discovery.SafeURL(cfg.Hit))
	u.printf("%s%s\n", u.key("total lemmings:"), report.FormatInt(cfg.Terrain*cfg.Pack))
	u.printf("%s%s\n", u.key("started:"), report.FormatInt(started))
	u.printf("%s%s\n", u.key("completed:"), report.FormatInt(completed))
	u.printf("%s%s\n", u.key("unborn:"), report.FormatInt(unborn))
	u.println()
	u.printf("%s%s  %s\n", u.key("page visits:"), report.FormatInt(d.TotalVisits), u.paint(toneCyan, report.Sparkline(d.Timeline)))
	u.printf("%s%s\n", u.key("page bytes:"), report.FormatBytes(d.TotalBytes))
	u.printf("%s%s (%.2f%%)\n", u.key("failed visits:"), report.FormatInt(d.FailedVisits), d.FailureRate*100)
	if d.CancelledVisits > 0 {
		u.printf("%s%s (life ended mid-visit)\n", u.key("cancelled:"), report.FormatInt(d.CancelledVisits))
	}
	u.println()
	u.printf("%s%s\n", u.key("2xx:"), u.paint(toneGreen, report.FormatInt(d.Total2xx)))
	u.printf("%s%s\n", u.key("3xx:"), u.paint(toneCyan, report.FormatInt(d.Total3xx)))
	u.printf("%s%s\n", u.key("4xx:"), u.paint(toneYellow, report.FormatInt(d.Total4xx)))
	u.printf("%s%s\n", u.key("5xx:"), u.paint(toneRed, report.FormatInt(d.Total5xx)))
	u.println()
	u.printf("%sp50 %s · p95 %s · p99 %s\n", u.key("page latency:"), report.FormatMillis(d.P50), report.FormatMillis(d.P95), report.FormatMillis(d.P99))

	if d.Assets.Requests > 0 || cfg.Assets {
		u.println()
		u.printf("%s%s requests · %s failed · %s denied (403) · %s\n",
			u.key("assets:"),
			report.FormatInt(d.Assets.Requests),
			report.FormatInt(d.Assets.Failed),
			report.FormatInt(d.Assets.Denied403),
			report.FormatBytes(d.Assets.Bytes),
		)
		if d.Assets.Requests > 0 {
			u.printf("%sp50 %s · p95 %s · p99 %s\n",
				u.key("asset latency:"),
				report.FormatMillis(d.Assets.P50),
				report.FormatMillis(d.Assets.P95),
				report.FormatMillis(d.Assets.P99),
			)
		}
	}

	rm := d.Room
	if rm.Held > 0 || rm.Admitted > 0 || rm.Expired > 0 || rm.Denied > 0 || rm.Rejected503 > 0 || rm.TotalPolls > 0 {
		u.println()
		u.printf("%s%s held · %s admitted · %s expired · %s denied · %s 503-rejected · %s polls\n",
			u.key("waiting room:"),
			report.FormatInt(rm.Held),
			report.FormatInt(rm.Admitted),
			report.FormatInt(rm.Expired),
			report.FormatInt(rm.Denied),
			report.FormatInt(rm.Rejected503),
			report.FormatInt(rm.TotalPolls),
		)
		if rm.Held > 0 {
			u.printf("%sp95 %s · max %s · max pos #%s · %s died in line\n",
				u.key("room wait:"),
				report.FormatMillis(rm.P95Wait),
				report.FormatMillis(rm.LongestWait),
				report.FormatInt(int64(rm.MaxPosition)),
				report.FormatInt(rm.DiedInLine),
			)
		}
	} else {
		u.printf("%s0 held\n", u.key("waiting room:"))
	}

	cc := d.Concert
	if cc.AdmitPasses > 0 || cc.RoomTickets > 0 || cc.Bans429 > 0 || cc.Bans403 > 0 {
		u.printf("%s%s admit passes · %s room tickets · %s bans (429) · %s bans (403)\n",
			u.key("concert:"),
			report.FormatInt(cc.AdmitPasses),
			report.FormatInt(cc.RoomTickets),
			report.FormatInt(cc.Bans429),
			report.FormatInt(cc.Bans403),
		)
	}

	if cfg.Scouts > 0 || d.Scouts.Visits > 0 || d.Scouts.Failures > 0 {
		u.printf("%s%s visits · %s failures\n",
			u.key("browser scouts:"),
			report.FormatInt(d.Scouts.Visits),
			report.FormatInt(d.Scouts.Failures),
		)
	}

	fs := d.Forensics.Stats
	if fs.Captured > 0 || fs.Deduplicated > 0 || fs.DroppedCap > 0 || fs.DroppedQueue > 0 {
		var jsErrors int64
		for i := range d.Forensics.Artifacts {
			jsErrors += int64(d.Forensics.Artifacts[i].ConsoleErrors)
		}
		reviewed := fs.Bug + fs.Flaky + fs.Expected + fs.Ignored
		u.println()
		u.printf("%s%s captured (%s screenshots, %s JS errors, %s mode)\n",
			u.key("forensics:"),
			u.paint(toneCyan, report.FormatInt(fs.Captured)),
			report.FormatInt(fs.WithScreenshots),
			report.FormatInt(jsErrors),
			fs.Mode,
		)
		if reviewed > 0 {
			u.printf("%s%s/%s reviewed\n", u.key("triaged:"), report.FormatInt(reviewed), report.FormatInt(fs.Captured))
		}
	}
	u.println()

	droppedLogs := d.DroppedLogs
	var overflowLogs int64
	if m != nil {
		droppedLogs = max(droppedLogs, m.DroppedLogs.Load())
		overflowLogs = m.OverflowLogs.Load()
	}
	if overflowLogs > 0 {
		u.printf("%s%s overflow logs\n", u.key("channel pressure:"), report.FormatInt(overflowLogs))
		u.println()
	}
	if droppedLogs > 0 || d.TraceDropped > 0 || fs.DroppedQueue > 0 {
		u.println(u.paint(toneRed, u.bold("  ⚠  DATA DROPPED — telemetry loss occurred during this run:")))
		if droppedLogs > 0 {
			u.println(u.paint(toneYellow, fmt.Sprintf("     • %s life records dropped (collector fell behind; aggregate visit totals remain exact)", report.FormatInt(droppedLogs))))
		}
		if d.TraceDropped > 0 {
			u.println(u.paint(toneYellow, fmt.Sprintf("     • %s JSONL trace entries dropped (trace writer fell behind)", report.FormatInt(d.TraceDropped))))
		}
		if fs.DroppedQueue > 0 {
			u.println(u.paint(toneYellow, fmt.Sprintf("     • %s forensic error captures dropped (capture queue saturated)", report.FormatInt(fs.DroppedQueue))))
		}
		u.println()
	}
	u.println(u.rule())
}

// PrintDelivered logs a successfully written report destination.
func (u *UI) PrintDelivered(label, location string) {
	u.printf("  %s %-10s → %s\n", u.paint(toneGreen, "✓"), label, location)
}

// PrintDeliveryError logs a report delivery failure.
func (u *UI) PrintDeliveryError(target string, err error) {
	u.printf("  %s delivery error (%s): %v\n", u.paint(toneRed, "✗"), target, err)
}

// PrintVerdict prints the rescued scorecard and final exit verdict.
func (u *UI) PrintVerdict(d report.Data, interrupted bool) {
	u.println()
	score := fmt.Sprintf("%.1f%%", d.RescuedPercent)
	switch {
	case d.RescuedPercent >= 99.95:
		score = u.paint(toneGreen, score)
	case d.RescuedPercent >= 50:
		score = u.paint(toneYellow, score)
	default:
		score = u.paint(toneRed, score)
	}
	u.printf("  %s\n", u.bold(report.RescueLine(d)))
	u.printf("  you rescued %s — %s of %s lemmings lived without a failed page.\n",
		u.bold(score), report.FormatInt(d.Rescued), report.FormatInt(d.Sessions))
	for _, g := range d.Gates {
		mark := u.paint(toneGreen, "✓")
		if !g.Passed {
			mark = u.paint(toneRed, "✗")
		}
		u.printf("  %s %s %s (limit %s)\n", mark, g.Name, g.Observed, g.Limit)
	}
	switch {
	case interrupted:
		u.println("  " + u.paint(toneYellow, "■ interrupted — the partial report above was still saved."))
	case d.Verdict == "FAILED":
		u.println("  " + u.paint(toneRed, u.bold("✗ FAILED")) + u.paint(toneMuted, " — exit code 2"))
	case d.Verdict == "PASSED":
		u.println("  " + u.paint(toneGreen, u.bold("✓ PASSED")))
	}
	u.println()
}

// PrintCompare renders a regression comparison table for `lemmings compare`.
func (u *UI) PrintCompare(res report.CompareResult) {
	u.println()
	u.println(u.rule())
	u.printf("  %s  %s\n", u.paint(tonePink, "LEMMINGS COMPARE"), u.paint(toneCyan, res.Target))
	u.println(u.rule())
	u.printf("%s%.1f%% → %.1f%% (%+.1f%%)\n", u.key("rescued:"), res.OldRescuedPct, res.NewRescuedPct, res.RescuedDeltaPct)
	u.printf("%s%.2f%% → %.2f%% (%+.2f%%)\n", u.key("failure rate:"), res.OldFailureRate*100, res.NewFailureRate*100, res.FailureRateDelta*100)
	u.printf("%s%.1f ms → %.1f ms\n", u.key("p50 latency:"), res.OldP50MS, res.NewP50MS)
	u.printf("%s%.1f ms → %.1f ms (%+.1f ms)\n", u.key("p95 latency:"), res.OldP95MS, res.NewP95MS, res.P95DeltaMS)
	u.printf("%s%.1f ms → %.1f ms\n", u.key("p99 latency:"), res.OldP99MS, res.NewP99MS)

	if len(res.NewFingerprints) > 0 {
		u.println()
		u.println("  " + u.paint(toneRed, fmt.Sprintf("new error signatures (%d):", len(res.NewFingerprints))))
		for _, fp := range res.NewFingerprints {
			u.printf("    + %s\n", fp)
		}
	}
	if len(res.ResolvedFingerprints) > 0 {
		u.println()
		u.println("  " + u.paint(toneGreen, fmt.Sprintf("resolved error signatures (%d):", len(res.ResolvedFingerprints))))
		for _, fp := range res.ResolvedFingerprints {
			u.printf("    - %s\n", fp)
		}
	}

	if len(res.Paths) > 0 {
		u.println()
		u.println(u.paint(toneMuted, "  top path latency shifts:"))
		limit := min(len(res.Paths), 10)
		for _, p := range res.Paths[:limit] {
			u.printf("    %-32s  p95 %6.1f → %6.1f ms (%+6.1f ms)  failed %+d\n",
				discovery.Clip(p.URL, 32), p.OldP95MS, p.NewP95MS, p.P95DeltaMS, p.FailedDelta)
		}
	}

	if len(res.Gates) > 0 {
		u.println()
		for _, g := range res.Gates {
			mark := u.paint(toneGreen, "✓")
			if !g.Passed {
				mark = u.paint(toneRed, "✗")
			}
			u.printf("  %s %s %s (limit %s)\n", mark, g.Name, g.Observed, g.Limit)
		}
	}
	u.println()
	if res.Regressed {
		u.println("  " + u.paint(toneRed, u.bold("✗ REGRESSED")) + u.paint(toneMuted, " — exit code 2"))
	} else {
		u.println("  " + u.paint(toneGreen, u.bold("✓ NO REGRESSION")))
	}
	u.println()
}
