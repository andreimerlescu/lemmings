package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/andreimerlescu/figtree/v2"
)

const (
	// arguments
	argHit, aliasHit                         string = "hit", "h"
	argTerrain, aliasTerrain                 string = "terrain", "t"
	argPack, aliasPack                       string = "pack", "p"
	argLimit, aliasLimit                     string = "limit", "l"
	argUntil, aliasUntil                     string = "until", "u"
	argRamp, aliasRamp                       string = "ramp", "r"
	argCrawl, aliasCrawl                     string = "crawl", "c"
	argCrawlDepth, aliasCrawlDepth           string = "crawl-depth", "cd"
	argSaveTo, aliasSaveTo                   string = "save-to", "s"
	argDashboardPort, aliasDashboardPort     string = "dashboard-port", "dp"
	argTTY                                   string = "tty"
	argColor                                 string = "color"
	argObserve, aliasObserve                 string = "observe", "obs"
	argMetricsPort, aliasMetricsPort         string = "metrics-port", "mp"
	argMetricsUrlLabel, aliasMetricsUrlLabel string = "metrics-url-label", "mul"
	argVersion, aliasVersion                 string = "version", "v"
	argSmtpHost, aliasSmtpHost               string = "smtp-host", "sh"
	argSmtpPort, aliasSmtpPort               string = "smtp-port", "sp"
	argSmtpFrom, aliasSmtpFrom               string = "smtp-from", "sf"
	argSmtpUser, aliasSmtpUser               string = "smtp-user", "su"
	argSmtpPass, aliasSmtpPass               string = "smtp-pass", "spw"
	argThinkMin, aliasThinkMin               string = "think-min", "tmin"
	argThinkMax, aliasThinkMax               string = "think-max", "tmax"
	argRequestTimeout, aliasRequestTimeout   string = "request-timeout", "rt"
	argMaxBodyBytes, aliasMaxBodyBytes       string = "max-body-bytes", "mbb"
	argMaxPages, aliasMaxPages               string = "max-pages", "mpg"
	argNavigation, aliasNavigation           string = "navigation", "nav"
	argStrictChecksum, aliasStrictChecksum   string = "strict-checksum", "sc"
	argJourney, aliasJourney                 string = "journey", "j"
	argTraceFile, aliasTraceFile             string = "trace-file", "tf"
	argMaxFailureRate, aliasMaxFailureRate   string = "max-failure-rate", "mfr"
	argP95Budget, aliasP95Budget             string = "p95-budget", "p95"

	// defaults
	defaultHit             string        = "http://localhost:8080/"
	defaultTerrain         int64         = 50
	defaultPack            int64         = 50
	defaultLimit           int           = 100
	defaultUntil           time.Duration = 30 * time.Second
	defaultRamp            time.Duration = 5 * time.Minute
	defaultCrawl           bool          = false
	defaultCrawlDepth      int           = 3
	defaultSaveTo          string        = "."
	defaultDashboardPort   int           = 4000
	defaultTTY             bool          = true
	defaultObserve         bool          = false
	defaultMetricsPort     int           = 9090
	defaultMetricsURLLabel string        = "path"
	defaultThinkMin        time.Duration = 300 * time.Millisecond
	defaultThinkMax        time.Duration = 1200 * time.Millisecond

	// warnings
	warnTotalLemmings   int64 = 100_000
	warnTotalGoroutines int64 = 10_000
)

// Exit codes. CI pipelines can tell a broken run from a slow one.
const (
	exitOK          = 0   // the run finished and every gate passed
	exitError       = 1   // configuration, indexing or delivery failed
	exitGateFailed  = 2   // the run finished but a -max-failure-rate or -p95-budget gate tripped
	exitInterrupted = 130 // stopped by SIGINT/SIGTERM; the partial report was still delivered
)

//go:embed VERSION
var versionBytes embed.FS

var currentVersion string

// Version returns the embedded VERSION without surrounding whitespace.
func Version() string {
	if len(currentVersion) == 0 {
		versionBytes, err := versionBytes.ReadFile("VERSION")
		if err != nil {
			return ""
		}
		currentVersion = strings.TrimSpace(string(versionBytes))
	}
	return currentVersion
}

// AssureStringInSet is a figtree.FigValidatorFunc that is used in conjunction with figtree.Plant for fruit validations
var AssureStringInSet = func(set ...string) figtree.FigValidatorFunc {
	return func(value interface{}) error {
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

// run is the whole program. It returns the process exit code so that
// deferred cleanup always runs before the process exits.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, showVersion, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return exitError
	}
	if showVersion {
		fmt.Println(Version())
		return exitOK
	}
	if err := cfg.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return exitError
	}

	ui := newUI(cfg)
	ui.printBootSummary(cfg)

	swarm, err := NewSwarm(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to initialize swarm:", err)
		return exitError
	}
	ui.printDashboard(cfg, swarm.Token())

	if err := swarm.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "swarm error:", err)
		return exitError
	}

	// Deliver even after Ctrl-C: the swarm context is cancelled by then,
	// so reports get their own bounded context.
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	deliveryErr := swarm.Report(reportCtx)

	result := swarm.reporter.Result()
	ui.printVerdict(result, ctx.Err() != nil)

	switch {
	case ctx.Err() != nil:
		return exitInterrupted
	case deliveryErr != nil:
		fmt.Fprintln(os.Stderr, "report error:", deliveryErr)
		return exitError
	case result.GatesFailed():
		return exitGateFailed
	}
	return exitOK
}

// loadConfig registers every flag, parses the command line and builds
// the SwarmConfig. It reports whether -version was requested.
func loadConfig() (SwarmConfig, bool, error) {
	figs := figtree.Grow()

	figs.NewString(argHit, defaultHit, "Origin URL for lemmings to traverse").
		WithAlias(argHit, aliasHit).
		WithValidator(argHit, figtree.AssureStringNotEmpty).
		WithValidator(argHit, figtree.AssureStringHasPrefix("http"))

	figs.NewInt64(argTerrain, defaultTerrain, "Number of terrain goroutine groups (states)").
		WithAlias(argTerrain, aliasTerrain).
		WithValidator(argTerrain, figtree.AssureInt64InRange(1, 10_000))

	figs.NewInt64(argPack, defaultPack, "Number of lemmings per terrain group (cities)").
		WithAlias(argPack, aliasPack).
		WithValidator(argPack, figtree.AssureInt64InRange(1, 100_000))

	figs.NewInt(argLimit, defaultLimit,
		"Semaphore ceiling for concurrent lemmings. -1 disables the ceiling entirely (dangerous)").
		WithAlias(argLimit, aliasLimit).
		WithValidator(argLimit, figtree.AssureIntInRange(-1, 1_000_000))

	figs.NewDuration(argUntil, defaultUntil, "How long each lemming lives").
		WithAlias(argUntil, aliasUntil).
		WithValidator(argUntil, figtree.AssureDurationGreaterThan(0)).
		WithValidator(argUntil, figtree.AssureDurationLessThan(24*time.Hour))

	figs.NewDuration(argRamp, defaultRamp, "Duration to bring all terrain groups online").
		WithAlias(argRamp, aliasRamp).
		WithValidator(argRamp, figtree.AssureDurationGreaterThan(0)).
		WithValidator(argRamp, figtree.AssureDurationLessThan(24*time.Hour))

	figs.NewBool(argCrawl, defaultCrawl, "Crawl origin for links when sitemap unavailable").
		WithAlias(argCrawl, aliasCrawl)

	figs.NewInt(argCrawlDepth, defaultCrawlDepth, "How many links deep to crawl").
		WithAlias(argCrawlDepth, aliasCrawlDepth).
		WithValidator(argCrawlDepth, figtree.AssureIntInRange(1, 100))

	figs.NewDuration(argThinkMin, defaultThinkMin, "Minimum pause between pages, like a person reading. 0 with -think-max 0 saturates").
		WithAlias(argThinkMin, aliasThinkMin)

	figs.NewDuration(argThinkMax, defaultThinkMax, "Maximum pause between pages").
		WithAlias(argThinkMax, aliasThinkMax)

	figs.NewString(argNavigation, NavigationLinks, "How lemmings choose pages: links (follow links on the page) or random (pick from the pool)").
		WithAlias(argNavigation, aliasNavigation).
		WithValidator(argNavigation, AssureStringInSet(NavigationLinks, NavigationRandom))

	figs.NewString(argJourney, "", "JSON file describing an ordered journey with checks for every step").
		WithAlias(argJourney, aliasJourney)

	figs.NewInt(argMaxPages, 0, "Pages per lemming before it leaves. 0 means it browses until -until").
		WithAlias(argMaxPages, aliasMaxPages).
		WithValidator(argMaxPages, figtree.AssureIntInRange(0, 1_000_000))

	figs.NewDuration(argRequestTimeout, defaultRequestTimeout, "Timeout for a single request, including redirects and body").
		WithAlias(argRequestTimeout, aliasRequestTimeout).
		WithValidator(argRequestTimeout, figtree.AssureDurationGreaterThan(0))

	figs.NewInt64(argMaxBodyBytes, defaultMaxBodyBytes, "Largest decoded response body read per page; larger bodies fail the visit").
		WithAlias(argMaxBodyBytes, aliasMaxBodyBytes).
		WithValidator(argMaxBodyBytes, figtree.AssureInt64InRange(1, 64<<20))

	figs.NewBool(argStrictChecksum, false, "Fail a visit when its body differs from the body seen at index time").
		WithAlias(argStrictChecksum, aliasStrictChecksum)

	figs.NewString(argTraceFile, "", "Write every visit to this new JSONL file (never overwrites)").
		WithAlias(argTraceFile, aliasTraceFile)

	figs.NewFloat64(argMaxFailureRate, -1, "CI gate: exit 2 if more than this share (0..1) of visits fail. Negative disables").
		WithAlias(argMaxFailureRate, aliasMaxFailureRate).
		WithValidator(argMaxFailureRate, figtree.AssureFloat64InRange(-1, 1))

	figs.NewDuration(argP95Budget, 0, "CI gate: exit 2 if p95 visit latency exceeds this. 0 disables").
		WithAlias(argP95Budget, aliasP95Budget)

	figs.NewList(argSaveTo, []string{defaultSaveTo}, "Report destinations — comma-separated list of local paths, s3:// URIs, or mailto: URIs.").
		WithAlias(argSaveTo, aliasSaveTo).
		WithValidator(argSaveTo, figtree.AssureListNotEmpty)

	figs.NewString(argSmtpHost, "", "SMTP server hostname. Overrides LEMMINGS_SMTP_HOST").
		WithAlias(argSmtpHost, aliasSmtpHost)

	figs.NewInt(argSmtpPort, 0, "SMTP server port (465=implicit TLS, otherwise STARTTLS when offered). Overrides LEMMINGS_SMTP_PORT").
		WithAlias(argSmtpPort, aliasSmtpPort).
		WithValidator(argSmtpPort, figtree.AssureIntInRange(0, 65535))

	figs.NewString(argSmtpUser, "", "SMTP username. Overrides LEMMINGS_SMTP_USER").
		WithAlias(argSmtpUser, aliasSmtpUser)

	figs.NewString(argSmtpPass, "", "SMTP password. Overrides LEMMINGS_SMTP_PASS").
		WithAlias(argSmtpPass, aliasSmtpPass)

	figs.NewString(argSmtpFrom, "", "SMTP from address. Overrides LEMMINGS_SMTP_FROM").
		WithAlias(argSmtpFrom, aliasSmtpFrom)

	figs.NewInt(argDashboardPort, defaultDashboardPort, "Port for the live dashboard").
		WithAlias(argDashboardPort, aliasDashboardPort).
		WithValidator(argDashboardPort, figtree.AssureIntInRange(1024, 65535))

	figs.NewBool(argObserve, defaultObserve, "Enable Prometheus metrics exporter on -metrics-port").
		WithAlias(argObserve, aliasObserve)

	figs.NewInt(argMetricsPort, defaultMetricsPort, "Port for the Prometheus /metrics endpoint").
		WithAlias(argMetricsPort, aliasMetricsPort).
		WithValidator(argMetricsPort, figtree.AssureIntInRange(1024, 65535))

	figs.NewString(argMetricsUrlLabel, defaultMetricsURLLabel, "URL label granularity on visit duration histogram: full, path, or none - max urls 999.").
		WithAlias(argMetricsUrlLabel, aliasMetricsUrlLabel).
		WithValidator(argMetricsUrlLabel, AssureStringInSet("full", "path", "none"))

	figs.NewBool(argVersion, false, "Show version").WithAlias(argVersion, aliasVersion)

	figs.NewBool(argTTY, defaultTTY, "Use carriage return for live STDOUT updates. Set false for CI pipelines")

	figs.NewBool(argColor, true, "Colour terminal output. Also disabled by NO_COLOR or -tty=false")

	if problems := figs.Problems(); len(problems) > 0 {
		return SwarmConfig{}, false, errors.Join(problems...)
	}
	if err := figs.Load(); err != nil {
		return SwarmConfig{}, false, fmt.Errorf("failed to load configuration: %w", err)
	}
	if *figs.Bool(argVersion) {
		return SwarmConfig{}, true, nil
	}

	failureRate := *figs.Float64(argMaxFailureRate)
	cfg := SwarmConfig{
		Hit:             *figs.String(argHit),
		Terrain:         *figs.Int64(argTerrain),
		Pack:            *figs.Int64(argPack),
		Limit:           *figs.Int(argLimit),
		Until:           *figs.Duration(argUntil),
		Ramp:            *figs.Duration(argRamp),
		Crawl:           *figs.Bool(argCrawl),
		CrawlDepth:      *figs.Int(argCrawlDepth),
		SaveTo:          saveTargets(*figs.List(argSaveTo), os.Getenv("LEMMINGS_SAVE_TO")),
		SMTPHost:        *figs.String(argSmtpHost),
		SMTPPort:        *figs.Int(argSmtpPort),
		SMTPUser:        *figs.String(argSmtpUser),
		SMTPPass:        *figs.String(argSmtpPass),
		SMTPFrom:        *figs.String(argSmtpFrom),
		DashboardPort:   *figs.Int(argDashboardPort),
		TTY:             *figs.Bool(argTTY),
		Color:           *figs.Bool(argColor),
		Version:         strings.TrimPrefix(Version(), "v"),
		Observe:         *figs.Bool(argObserve),
		MetricsPort:     *figs.Int(argMetricsPort),
		MetricsURLLabel: *figs.String(argMetricsUrlLabel),
		ThinkMin:        *figs.Duration(argThinkMin),
		ThinkMax:        *figs.Duration(argThinkMax),
		RequestTimeout:  *figs.Duration(argRequestTimeout),
		MaxBodyBytes:    *figs.Int64(argMaxBodyBytes),
		MaxPages:        *figs.Int(argMaxPages),
		Navigation:      *figs.String(argNavigation),
		StrictChecksum:  *figs.Bool(argStrictChecksum),
		JourneyFile:     *figs.String(argJourney),
		TraceFile:       *figs.String(argTraceFile),
		FailureGate:     failureRate >= 0,
		MaxFailureRate:  max(failureRate, 0),
		P95Budget:       *figs.Duration(argP95Budget),
	}
	return cfg, false, nil
}

// saveTargets combines -save-to with the comma-separated LEMMINGS_SAVE_TO
// environment variable. Both contribute; duplicates are removed and the
// first occurrence keeps its position.
func saveTargets(flag []string, env string) []string {
	all := append([]string(nil), flag...)
	for _, s := range strings.Split(env, ",") {
		all = append(all, s)
	}
	seen := make(map[string]bool)
	var out []string
	for _, s := range all {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
