// Command lemmings unleashes stateful simulated visitors onto an HTTP(S) origin,
// streams live error forensics (screenshots, JS console output, session state,
// cookie jar status, and extracted error bodies) into a hardened localhost admin
// portal, and delivers HTML, Markdown, and JSON reports with CI gates.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andreimerlescu/figtree/v2"
	"github.com/andreimerlescu/lemmings/v2/internal/cli"
	"github.com/andreimerlescu/lemmings/v2/internal/dashboard"
	"github.com/andreimerlescu/lemmings/v2/internal/delivery"
	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/forensics"
	"github.com/andreimerlescu/lemmings/v2/internal/metrics"
	"github.com/andreimerlescu/lemmings/v2/internal/plan"
	"github.com/andreimerlescu/lemmings/v2/internal/report"
	"github.com/andreimerlescu/lemmings/v2/internal/secrets"
	"github.com/andreimerlescu/lemmings/v2/internal/swarm"
	"github.com/andreimerlescu/lemmings/v2/internal/version"
)

const (
	// CLI flag names and unique aliases (strictly verified collision-free).
	argHit, aliasHit                         = "hit", "h"
	argTerrain, aliasTerrain                 = "terrain", "t"
	argPack, aliasPack                       = "pack", "p"
	argLimit, aliasLimit                     = "limit", "l"
	argUntil, aliasUntil                     = "until", "u"
	argRamp, aliasRamp                       = "ramp", "r"
	argRampShape, aliasRampShape             = "ramp-shape", "rs"
	argCrawl, aliasCrawl                     = "crawl", "c"
	argCrawlDepth, aliasCrawlDepth           = "crawl-depth", "cd"
	argSaveTo, aliasSaveTo                   = "save-to", "s"
	argUniqueReport, aliasUniqueReport       = "unique-report", "ur"
	argReportName, aliasReportName           = "report-name", "rn"
	argDashboardPort, aliasDashboardPort     = "dashboard-port", "dp"
	argTTY                                   = "tty"
	argColor                                 = "color"
	argObserve, aliasObserve                 = "observe", "obs"
	argMetricsPort, aliasMetricsPort         = "metrics-port", "mp"
	argMetricsURLLabel, aliasMetricsURLLabel = "metrics-url-label", "mul"
	argVersion, aliasVersion                 = "version", "v"
	argSMTPHost, aliasSMTPHost               = "smtp-host", "sh"
	argSMTPPort, aliasSMTPPort               = "smtp-port", "sp"
	argSMTPFrom, aliasSMTPFrom               = "smtp-from", "sf"
	argSMTPUser, aliasSMTPUser               = "smtp-user", "su"
	argSMTPPassFile, aliasSMTPPassFile       = "smtp-pass-file", "spf"
	argSMTPPassStdin                         = "smtp-pass-stdin"
	argSMTPRequireTLS                        = "smtp-require-tls"
	argThinkMin, aliasThinkMin               = "think-min", "tmin"
	argThinkMax, aliasThinkMax               = "think-max", "tmax"
	argRequestTimeout, aliasRequestTimeout   = "request-timeout", "rt"
	argMaxBodyBytes, aliasMaxBodyBytes       = "max-body-bytes", "mbb"
	argMaxPages, aliasMaxPages               = "max-pages", "mpg"
	argNavigation, aliasNavigation           = "navigation", "nav"
	argStrictChecksum, aliasStrictChecksum   = "strict-checksum", "sc"
	argAssets                                = "assets"
	argAssetsPerPage                         = "assets-per-page"
	argPlan, aliasPlan                       = "plan", "pl"
	argMaxPlanItems, aliasMaxPlanItems       = "max-plan-items", "mpi"
	argRogueRate, aliasRogueRate             = "rogue-rate", "rr"
	argMinPlanPassRate, aliasMinPlanPassRate = "min-plan-pass-rate", "mppr"
	argTraceFile, aliasTraceFile             = "trace-file", "tf"
	argMaxFailureRate, aliasMaxFailureRate   = "max-failure-rate", "mfr"
	argP95Budget, aliasP95Budget             = "p95-budget", "p95"

	// v2.0 forensics, scouts, admin review, and dry-run flags.
	argCapture                               = "capture"
	argCaptureMax, aliasCaptureMax           = "capture-max", "cmax"
	argCapturePerError, aliasCapturePerError = "capture-per-error", "cpe"
	argCaptureWorkers, aliasCaptureWorkers   = "capture-workers", "cw"
	argCaptureTimeout, aliasCaptureTimeout   = "capture-timeout", "ct"
	argCaptureFormat, aliasCaptureFormat     = "capture-format", "cf"
	argChromeBin                             = "chrome-bin"
	argCDPURL                                = "cdp-url"
	argAllowRemoteCDP                        = "allow-remote-cdp"
	argIncludeRemoteScreenshots              = "include-remote-screenshots"
	argScouts                                = "scouts"
	argRevealSessionCookies                  = "reveal-session-cookies"
	argReview                                = "review"
	argDryRun                                = "dry-run"

	// Defaults.
	defaultHit             = "http://localhost:8080/"
	defaultTerrain         = int64(50)
	defaultPack            = int64(50)
	defaultLimit           = 100
	defaultUntil           = 30 * time.Second
	defaultRamp            = 5 * time.Minute
	defaultCrawl           = false
	defaultCrawlDepth      = 3
	defaultSaveTo          = "."
	defaultDashboardPort   = 4000
	defaultTTY             = true
	defaultObserve         = false
	defaultMetricsPort     = 9090
	defaultMetricsURLLabel = "path"
	defaultThinkMin        = 300 * time.Millisecond
	defaultThinkMax        = 1200 * time.Millisecond
	defaultCaptureMax      = 50
	defaultCapturePerError = 3
	defaultCaptureWorkers  = 2
	defaultCaptureTimeout  = 8 * time.Second
)

// Exit codes allow CI pipelines to distinguish operational errors from failed gates.
const (
	exitOK          = 0   // run (or compare) completed and all gates passed
	exitError       = 1   // configuration, indexing, browser setup, or delivery error
	exitGateFailed  = 2   // run completed but CI gate or regression threshold tripped
	exitInterrupted = 130 // stopped by SIGINT/SIGTERM during swarm run; partial report saved
)

var assureStringInSet = func(set ...string) figtree.FigValidatorFunc {
	return func(value any) error {
		v := figtree.NewFlesh(value)
		if v.IsString() {
			s := v.ToString()
			for _, allowed := range set {
				if s == allowed {
					return nil
				}
			}
			return fmt.Errorf("string must be one of %v, got %q", set, s)
		}
		return figtree.ErrInvalidType{Wanted: "String", Got: value}
	}
}

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) > 1 && os.Args[1] == "compare" {
		return runCompare(os.Args[2:], os.Stdout, os.Stderr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, showVersion, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return exitError
	}
	if showVersion {
		fmt.Println(version.String())
		return exitOK
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return exitError
	}

	ui := cli.New(cfg, os.Stdout)

	// Initialize forensics capturer (pure-Go CDP Chromium or HTTP-only fallback).
	capturer, browserStatus, err := initCapturer(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forensics error:", err)
		return exitError
	}

	// Dry-run validates config, probes Chromium/CDP, indexes origin, prints plan, and exits.
	if cfg.DryRun {
		defer func() { _ = capturer.Close() }()
		pool, err := discovery.BuildURLPool(ctx, cfg.Hit, discovery.BuildOptions{
			Crawl:      cfg.Crawl,
			CrawlDepth: cfg.CrawlDepth,
			OnProgress: ui.PrintProgress,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "dry-run index error: %v\n", err)
			return exitError
		}
		ui.PrintDryRun(cfg, pool, browserStatus)
		return exitOK
	}

	ui.PrintBootSummary(cfg, browserStatus)

	bus := events.NewBus()
	store := forensics.NewStore(cfg.CaptureMax, cfg.CapturePerError)
	coord := forensics.NewCoordinator(ctx, store, capturer, bus, forensics.CoordinatorConfig{
		Workers: cfg.CaptureWorkers,
		Queue:   max(cfg.CaptureMax*2, 64),
	})
	// Safety net if we exit early before normal DrainAndClose.
	drained := false
	defer func() {
		if !drained {
			drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = coord.DrainAndClose(drainCtx)
		}
	}()

	rep := report.New(cfg)
	rep.SetForensics(store)
	rep.SetDeliveryErrorHook(ui.PrintDeliveryError)

	if cfg.TraceFile != "" {
		tw, err := report.NewTraceWriter(cfg.TraceFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "trace file error:", err)
			return exitError
		}
		rep.SetTrace(tw)
	}

	for _, uri := range cfg.SaveTo {
		t, err := delivery.ParseTarget(uri, ui.PrintDelivered)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid -save-to target %q: %v\n", uri, err)
			return exitError
		}
		if mt, ok := t.(*delivery.MailTarget); ok {
			delivery.ApplyFlagSMTPOverrides(mt, cfg)
			mt.SetIncludeScreenshots(cfg.IncludeRemoteScreenshots)
		}
		if st, ok := t.(*delivery.S3Target); ok {
			st.SetIncludeScreenshots(cfg.IncludeRemoteScreenshots)
		}
		rep.AddTarget(t)
	}

	sw, err := swarm.New(ctx, cfg, swarm.Options{
		Bus:        bus,
		Forensics:  coord,
		LifeSink:   rep,
		OnProgress: ui.PrintProgress,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to initialize swarm:", err)
		return exitError
	}

	rep.Attach(bus)

	// Generate dashboard token once here; Swarm never holds the raw token (SEC-05).
	rawToken, err := dashboard.GenerateToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to generate dashboard token:", err)
		return exitError
	}
	dash, err := dashboard.New(cfg, bus, sw.Metrics(), rawToken, rep, store)
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to initialize dashboard:", err)
		return exitError
	}
	if _, err := dash.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "dashboard start error:", err)
		return exitError
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = dash.Shutdown(shutCtx)
	}()

	ui.PrintDashboard(cfg, rawToken)

	if cfg.Observe {
		promObs := metrics.NewPrometheusObserver(cfg)
		if err := sw.RegisterObserver(ctx, promObs); err != nil {
			fmt.Fprintln(os.Stderr, "prometheus observer error:", err)
			return exitError
		}
	}

	startedAt := time.Now()
	rep.MarkStarted(startedAt)
	dash.MarkStarted(startedAt)

	tickDone := make(chan struct{})
	var tickerWg sync.WaitGroup
	tickerWg.Add(1)
	go func() {
		defer tickerWg.Done()
		ui.Tick(ctx, tickDone, startedAt, sw.Metrics(), func() report.LiveStats {
			return rep.Live(0, 0)
		})
	}()

	runErr := sw.Run(ctx)
	close(tickDone)
	tickerWg.Wait()

	// Drain any in-flight screenshot captures using a fresh bounded context so
	// screenshots queued near the end (or before Ctrl-C) still land in the store.
	drainCtx, drainCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	_ = coord.DrainAndClose(drainCtx)
	drainCancel()
	drained = true

	if runErr != nil {
		fmt.Fprintln(os.Stderr, "swarm error:", runErr)
		return exitError
	}

	interrupted := ctx.Err() != nil

	// Optional post-run admin review hold mode: keep portal alive until the admin
	// clicks "Finalize & Write Report" or sends Ctrl-C.
	if cfg.ReviewMode && !interrupted {
		finCh := dash.EnterReviewMode()
		ui.PrintReviewPrompt(cfg.DashboardPort, store.Stats().Captured)
		select {
		case <-finCh:
		case <-ctx.Done():
		}
	}

	rep.MarkEnded(time.Now(), sw.Metrics())
	dash.Finish()

	result := rep.Result()
	ui.PrintFinalSummary(cfg, startedAt, sw.Metrics(), result)

	reportCtx, reportCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	deliveryErr := rep.Write(reportCtx)
	reportCancel()

	ui.PrintVerdict(result, interrupted)

	switch {
	case interrupted:
		return exitInterrupted
	case deliveryErr != nil:
		fmt.Fprintln(os.Stderr, "report error:", deliveryErr)
		return exitError
	case result.GatesFailed():
		return exitGateFailed
	}
	return exitOK
}

func initCapturer(ctx context.Context, cfg swarm.Config) (forensics.Capturer, string, error) {
	if !cfg.CaptureEnabled && cfg.Scouts == 0 {
		msg := "disabled (-capture=false; HTTP-only error forensics)"
		return forensics.NewHTTPFallbackCapturer(msg), msg, nil
	}
	explicitBrowser := strings.TrimSpace(cfg.CDPURL) != "" || strings.TrimSpace(cfg.ChromeBin) != ""
	capCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cdp, err := forensics.NewCDPCapturer(capCtx, forensics.CDPOptions{
		ChromeBin:      cfg.ChromeBin,
		CDPURL:         cfg.CDPURL,
		AllowRemoteCDP: cfg.AllowRemoteCDP,
		Format:         cfg.CaptureFormat,
		Timeout:        cfg.CaptureTimeout,
	})
	if err == nil {
		return cdp, cdp.BrowserInfo(), nil
	}
	if explicitBrowser || cfg.Scouts > 0 {
		return nil, "", fmt.Errorf("headless browser required (%v)", err)
	}
	fallbackMsg := "HTTP-only fallback (no local Chrome/Chromium found)"
	return forensics.NewHTTPFallbackCapturer(fallbackMsg), fallbackMsg, nil
}

func runCompare(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(stderr)

	maxFailDelta := fs.Float64("max-failure-delta", -1, "fail (exit 2) if failure rate increased by more than this share (0..1); negative disables")
	maxP95Delta := fs.Duration("max-p95-delta", 0, "fail (exit 2) if p95 latency increased by more than this duration; 0 disables")
	maxRescuedDrop := fs.Float64("max-rescued-drop", -1, "fail (exit 2) if rescued percentage dropped by more than this many points (0..100); negative disables")
	asJSON := fs.Bool("json", false, "output comparison as JSON")
	tty := fs.Bool("tty", true, "enable TTY formatting")
	color := fs.Bool("color", true, "enable color output")

	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: lemmings compare [flags] <baseline.json> <candidate.json>")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitError
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return exitError
	}

	opts := report.CompareOptions{
		MaxFailureDelta: *maxFailDelta,
		MaxP95DeltaMS:   report.Millis(*maxP95Delta),
		MaxRescuedDrop:  *maxRescuedDrop,
	}
	res, err := report.CompareFiles(fs.Arg(0), fs.Arg(1), opts)
	if err != nil {
		fmt.Fprintln(stderr, "compare error:", err)
		return exitError
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			fmt.Fprintln(stderr, "encode error:", err)
			return exitError
		}
	} else {
		ui := cli.New(swarm.Config{TTY: *tty, Color: *color}, stdout)
		ui.PrintCompare(res)
	}

	if res.Regressed {
		return exitGateFailed
	}
	return exitOK
}

func loadConfig() (swarm.Config, bool, error) {
	figs := figtree.Grow()

	figs.NewString(argHit, defaultHit, "Origin URL for lemmings to traverse").
		WithAlias(argHit, aliasHit).
		WithValidator(argHit, figtree.AssureStringNotEmpty).
		WithValidator(argHit, figtree.AssureStringHasPrefix("http"))

	figs.NewInt64(argTerrain, defaultTerrain, "Number of terrain goroutine groups").
		WithAlias(argTerrain, aliasTerrain).
		WithValidator(argTerrain, figtree.AssureInt64InRange(1, 10_000))

	figs.NewInt64(argPack, defaultPack, "Number of lemmings per terrain group").
		WithAlias(argPack, aliasPack).
		WithValidator(argPack, figtree.AssureInt64InRange(0, 100_000))

	figs.NewInt(argLimit, defaultLimit, "Semaphore ceiling for concurrent lemmings (-1 disables ceiling)").
		WithAlias(argLimit, aliasLimit).
		WithValidator(argLimit, figtree.AssureIntInRange(-1, 1_000_000))

	figs.NewDuration(argUntil, defaultUntil, "How long each lemming lives").
		WithAlias(argUntil, aliasUntil).
		WithValidator(argUntil, figtree.AssureDurationGreaterThan(0)).
		WithValidator(argUntil, figtree.AssureDurationLessThan(24*time.Hour))

	figs.NewDuration(argRamp, defaultRamp, "Duration to bring all terrain groups online").
		WithAlias(argRamp, aliasRamp).
		WithValidator(argRamp, figtree.AssureDurationLessThan(24*time.Hour))

	figs.NewString(argRampShape, swarm.RampLinear, "Terrain ramp curve: linear, fibonacci, or step").
		WithAlias(argRampShape, aliasRampShape).
		WithValidator(argRampShape, assureStringInSet(swarm.RampLinear, swarm.RampFibonacci, swarm.RampStep))

	figs.NewBool(argCrawl, defaultCrawl, "Crawl origin for links when sitemap unavailable").
		WithAlias(argCrawl, aliasCrawl)

	figs.NewInt(argCrawlDepth, defaultCrawlDepth, "How many links deep to crawl").
		WithAlias(argCrawlDepth, aliasCrawlDepth).
		WithValidator(argCrawlDepth, figtree.AssureIntInRange(1, 100))

	figs.NewDuration(argThinkMin, defaultThinkMin, "Minimum pause between pages (0 with -think-max 0 saturates)").
		WithAlias(argThinkMin, aliasThinkMin)

	figs.NewDuration(argThinkMax, defaultThinkMax, "Maximum pause between pages").
		WithAlias(argThinkMax, aliasThinkMax)

	figs.NewString(argNavigation, plan.NavigationLinks, "Page selection: links (follow page links) or random (pick from pool)").
		WithAlias(argNavigation, aliasNavigation).
		WithValidator(argNavigation, assureStringInSet(plan.NavigationLinks, plan.NavigationRandom))

	figs.NewString(argPlan, "", "YAML plan file, comma-separated list, or directory of *.yaml/*.yml user personas").
		WithAlias(argPlan, aliasPlan)

	figs.NewInt(argMaxPlanItems, plan.DefaultMaxPlanItems, "Maximum steps allowed per YAML plan (1..1000000)").
		WithAlias(argMaxPlanItems, aliasMaxPlanItems).
		WithValidator(argMaxPlanItems, figtree.AssureIntInRange(1, 1_000_000))

	figs.NewFloat64(argRogueRate, -1.0, "Fraction of lemmings wandering sitemap/crawled links instead of following -plan (-1=auto: 0.05 with -plan, 0 otherwise)").
		WithAlias(argRogueRate, aliasRogueRate).
		WithValidator(argRogueRate, figtree.AssureFloat64InRange(-1, 1))

	figs.NewFloat64(argMinPlanPassRate, -1.0, "CI gate: exit 2 if plan cohort pass share (0..1) falls below this; negative disables").
		WithAlias(argMinPlanPassRate, aliasMinPlanPassRate).
		WithValidator(argMinPlanPassRate, figtree.AssureFloat64InRange(-1, 1))

	figs.NewInt(argMaxPages, 0, "Pages per lemming before exit (0 browses until -until)").
		WithAlias(argMaxPages, aliasMaxPages).
		WithValidator(argMaxPages, figtree.AssureIntInRange(0, 1_000_000))

	figs.NewDuration(argRequestTimeout, swarm.DefaultRequestTimeout, "Timeout for a single request, including redirects and body").
		WithAlias(argRequestTimeout, aliasRequestTimeout).
		WithValidator(argRequestTimeout, figtree.AssureDurationGreaterThan(0))

	figs.NewInt64(argMaxBodyBytes, swarm.DefaultMaxBodyBytes, "Largest decoded response body read per page").
		WithAlias(argMaxBodyBytes, aliasMaxBodyBytes).
		WithValidator(argMaxBodyBytes, figtree.AssureInt64InRange(1, 64<<20))

	figs.NewBool(argStrictChecksum, false, "Fail a visit when its body differs from the body seen at index time").
		WithAlias(argStrictChecksum, aliasStrictChecksum)

	figs.NewBool(argAssets, false, "Fetch up to -assets-per-page uncached same-origin static assets per HTML page")

	figs.NewInt(argAssetsPerPage, swarm.DefaultAssetsPerPage, "Maximum uncached same-origin static assets fetched per HTML page when -assets is enabled (1..64)").
		WithValidator(argAssetsPerPage, figtree.AssureIntInRange(0, 64))

	figs.NewString(argTraceFile, "", "Write every visit to this new JSONL file (mode 0600, never overwrites)").
		WithAlias(argTraceFile, aliasTraceFile)

	figs.NewFloat64(argMaxFailureRate, -1, "CI gate: exit 2 if failure share (0..1) exceeds this; negative disables").
		WithAlias(argMaxFailureRate, aliasMaxFailureRate).
		WithValidator(argMaxFailureRate, figtree.AssureFloat64InRange(-1, 1))

	figs.NewDuration(argP95Budget, 0, "CI gate: exit 2 if p95 visit latency exceeds this; 0 disables").
		WithAlias(argP95Budget, aliasP95Budget)

	figs.NewList(argSaveTo, []string{defaultSaveTo}, "Report destinations: comma-separated local paths, s3:// URIs, or mailto: URIs").
		WithAlias(argSaveTo, aliasSaveTo).
		WithValidator(argSaveTo, figtree.AssureListNotEmpty)

	figs.NewBool(argUniqueReport, false, "Include UTC timestamp in report filenames to avoid same-day overwrites").
		WithAlias(argUniqueReport, aliasUniqueReport)

	figs.NewString(argReportName, "", "Custom base slug for report filenames ([a-zA-Z0-9._-]+)").
		WithAlias(argReportName, aliasReportName)

	// SMTP configuration — password is NEVER accepted as a CLI flag (S4).
	figs.NewString(argSMTPHost, "", "SMTP server hostname (overrides LEMMINGS_SMTP_HOST)").
		WithAlias(argSMTPHost, aliasSMTPHost)

	figs.NewInt(argSMTPPort, 0, "SMTP server port (465=implicit TLS, else STARTTLS; overrides LEMMINGS_SMTP_PORT)").
		WithAlias(argSMTPPort, aliasSMTPPort).
		WithValidator(argSMTPPort, figtree.AssureIntInRange(0, 65535))

	figs.NewString(argSMTPUser, "", "SMTP username (overrides LEMMINGS_SMTP_USER)").
		WithAlias(argSMTPUser, aliasSMTPUser)

	figs.NewString(argSMTPPassFile, "", "Path to 0600 file containing SMTP password (or set LEMMINGS_SMTP_PASS_FILE / LEMMINGS_SMTP_PASS)").
		WithAlias(argSMTPPassFile, aliasSMTPPassFile)

	figs.NewBool(argSMTPPassStdin, false, "Read SMTP password from a single line on stdin")

	figs.NewBool(argSMTPRequireTLS, false, "Fail SMTP delivery if server does not negotiate TLS/STARTTLS (default true for remote hosts)")

	figs.NewString(argSMTPFrom, "", "SMTP from address (overrides LEMMINGS_SMTP_FROM)").
		WithAlias(argSMTPFrom, aliasSMTPFrom)

	// Forensics, CDP screenshots, Scouts, Admin Review, Dry Run.
	figs.NewBool(argCapture, true, "Capture live error forensics & screenshots via headless Chromium when available")

	figs.NewInt(argCaptureMax, defaultCaptureMax, "Maximum error artifacts captured per run (1..500)").
		WithAlias(argCaptureMax, aliasCaptureMax).
		WithValidator(argCaptureMax, figtree.AssureIntInRange(1, 500))

	figs.NewInt(argCapturePerError, defaultCapturePerError, "Maximum artifacts captured per unique error fingerprint (1..50)").
		WithAlias(argCapturePerError, aliasCapturePerError).
		WithValidator(argCapturePerError, figtree.AssureIntInRange(1, 50))

	figs.NewInt(argCaptureWorkers, defaultCaptureWorkers, "Concurrent headless browser capture workers (1..8)").
		WithAlias(argCaptureWorkers, aliasCaptureWorkers).
		WithValidator(argCaptureWorkers, figtree.AssureIntInRange(1, 8))

	figs.NewDuration(argCaptureTimeout, defaultCaptureTimeout, "Timeout per browser error capture").
		WithAlias(argCaptureTimeout, aliasCaptureTimeout).
		WithValidator(argCaptureTimeout, figtree.AssureDurationGreaterThan(0))

	figs.NewString(argCaptureFormat, swarm.CaptureFormatJPEG, "Screenshot format: jpeg or png").
		WithAlias(argCaptureFormat, aliasCaptureFormat).
		WithValidator(argCaptureFormat, assureStringInSet(swarm.CaptureFormatJPEG, swarm.CaptureFormatPNG))

	figs.NewString(argChromeBin, "", "Explicit path to local Chrome/Chromium executable (overrides auto-detection)")

	figs.NewString(argCDPURL, "", "Remote Chrome DevTools Protocol ws:// or http:// endpoint instead of local Chrome")

	figs.NewBool(argAllowRemoteCDP, false, "Allow non-loopback -cdp-url endpoints (cloud metadata and link-local IPs remain blocked)")

	figs.NewBool(argIncludeRemoteScreenshots, false, "Include unscrubbed error screenshots in remote reports (mailto: and s3:// targets); local reports and Admin Portal always include screenshots")

	figs.NewInt(argScouts, 0, "Real-browser Scout lemmings running alongside HTTP swarm to catch JS/render errors (0..8)").
		WithValidator(argScouts, figtree.AssureIntInRange(0, 8))

	figs.NewBool(argRevealSessionCookies, false, "Allow authenticated localhost portal to inspect raw session cookie values (never saved to reports)")

	figs.NewBool(argReview, false, "Keep Admin Portal open after swarm ends until Finalize & Write Report is clicked")

	figs.NewBool(argDryRun, false, "Validate config, index target URL pool, probe browser, and exit without sending swarm load")

	figs.NewInt(argDashboardPort, defaultDashboardPort, "Loopback port for the live Admin Portal").
		WithAlias(argDashboardPort, aliasDashboardPort).
		WithValidator(argDashboardPort, figtree.AssureIntInRange(1024, 65535))

	figs.NewBool(argObserve, defaultObserve, "Enable Prometheus /metrics exporter on -metrics-port").
		WithAlias(argObserve, aliasObserve)

	figs.NewInt(argMetricsPort, defaultMetricsPort, "Loopback port for Prometheus /metrics").
		WithAlias(argMetricsPort, aliasMetricsPort).
		WithValidator(argMetricsPort, figtree.AssureIntInRange(1024, 65535))

	figs.NewString(argMetricsURLLabel, defaultMetricsURLLabel, "URL label granularity on visit duration histogram: full, path, or none").
		WithAlias(argMetricsURLLabel, aliasMetricsURLLabel).
		WithValidator(argMetricsURLLabel, assureStringInSet("full", "path", "none"))

	figs.NewBool(argVersion, false, "Show version").WithAlias(argVersion, aliasVersion)
	figs.NewBool(argTTY, defaultTTY, "Use carriage return for live STDOUT updates (set false for CI)")
	figs.NewBool(argColor, true, "Color terminal output (also disabled by NO_COLOR or -tty=false)")

	if problems := figs.Problems(); len(problems) > 0 {
		return swarm.Config{}, false, errors.Join(problems...)
	}
	if err := figs.Load(); err != nil {
		return swarm.Config{}, false, fmt.Errorf("failed to load configuration: %w", err)
	}
	if *figs.Bool(argVersion) {
		return swarm.Config{}, true, nil
	}

	smtpPass, _, err := secrets.ResolveOptional(
		*figs.String(argSMTPPassFile),
		"LEMMINGS_SMTP_PASS",
		*figs.Bool(argSMTPPassStdin),
		os.Stdin,
	)
	if err != nil {
		return swarm.Config{}, false, fmt.Errorf("smtp password: %w", err)
	}

	failureRate := *figs.Float64(argMaxFailureRate)
	minPlanPass := *figs.Float64(argMinPlanPassRate)
	cfg := swarm.Config{
		Hit:                      *figs.String(argHit),
		Terrain:                  *figs.Int64(argTerrain),
		Pack:                     *figs.Int64(argPack),
		Limit:                    *figs.Int(argLimit),
		Until:                    *figs.Duration(argUntil),
		Ramp:                     *figs.Duration(argRamp),
		RampShape:                *figs.String(argRampShape),
		Crawl:                    *figs.Bool(argCrawl),
		CrawlDepth:               *figs.Int(argCrawlDepth),
		SaveTo:                   delivery.SaveTargets(*figs.List(argSaveTo), os.Getenv("LEMMINGS_SAVE_TO")),
		UniqueReport:             *figs.Bool(argUniqueReport),
		ReportName:               *figs.String(argReportName),
		SMTPHost:                 *figs.String(argSMTPHost),
		SMTPPort:                 *figs.Int(argSMTPPort),
		SMTPUser:                 *figs.String(argSMTPUser),
		SMTPPass:                 smtpPass,
		SMTPFrom:                 *figs.String(argSMTPFrom),
		SMTPRequireTLS:           *figs.Bool(argSMTPRequireTLS),
		DashboardPort:            *figs.Int(argDashboardPort),
		TTY:                      *figs.Bool(argTTY),
		Color:                    *figs.Bool(argColor),
		Version:                  version.Raw(),
		Observe:                  *figs.Bool(argObserve),
		MetricsPort:              *figs.Int(argMetricsPort),
		MetricsURLLabel:          *figs.String(argMetricsURLLabel),
		ThinkMin:                 *figs.Duration(argThinkMin),
		ThinkMax:                 *figs.Duration(argThinkMax),
		RequestTimeout:           *figs.Duration(argRequestTimeout),
		MaxBodyBytes:             *figs.Int64(argMaxBodyBytes),
		MaxPages:                 *figs.Int(argMaxPages),
		Navigation:               *figs.String(argNavigation),
		StrictChecksum:           *figs.Bool(argStrictChecksum),
		Assets:                   *figs.Bool(argAssets),
		AssetsPerPage:            *figs.Int(argAssetsPerPage),
		PlanFile:                 *figs.String(argPlan),
		MaxPlanItems:             *figs.Int(argMaxPlanItems),
		RogueRate:                *figs.Float64(argRogueRate),
		TraceFile:                *figs.String(argTraceFile),
		CaptureEnabled:           *figs.Bool(argCapture),
		CaptureMax:               *figs.Int(argCaptureMax),
		CapturePerError:          *figs.Int(argCapturePerError),
		CaptureWorkers:           *figs.Int(argCaptureWorkers),
		CaptureTimeout:           *figs.Duration(argCaptureTimeout),
		CaptureFormat:            *figs.String(argCaptureFormat),
		ChromeBin:                *figs.String(argChromeBin),
		CDPURL:                   *figs.String(argCDPURL),
		AllowRemoteCDP:           *figs.Bool(argAllowRemoteCDP),
		IncludeRemoteScreenshots: *figs.Bool(argIncludeRemoteScreenshots),
		Scouts:                   *figs.Int(argScouts),
		RevealSessionCookies:     *figs.Bool(argRevealSessionCookies),
		ReviewMode:               *figs.Bool(argReview),
		DryRun:                   *figs.Bool(argDryRun),
		FailureGate:              failureRate >= 0,
		MaxFailureRate:           max(failureRate, 0),
		P95Budget:                *figs.Duration(argP95Budget),
		PlanPassGate:             minPlanPass >= 0,
		MinPlanPassRate:          max(minPlanPass, 0),
	}
	if cfg.Assets && (cfg.AssetsPerPage < 1 || cfg.AssetsPerPage > 64) {
		return swarm.Config{}, false, fmt.Errorf("-assets-per-page must be 1..64 when -assets is enabled, got %d", cfg.AssetsPerPage)
	}
	return cfg, false, nil
}
