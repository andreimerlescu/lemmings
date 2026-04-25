package main

import (
	"context"
	"embed"
	"fmt"
	"log"
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
	argSaveTo, aliasSaveTo                   string = "save-to", "st"
	argDashboardPort, aliasDashboardPort     string = "dashboard-port", "dp"
	argTTY                                   string = "tty"
	argObserve, aliasObserve                 string = "observe", "o"
	argMetricsPort, aliasMetricsPort         string = "metrics-port", "mp"
	argMetricsUrlLabel, aliasMetricsUrlLabel string = "metrics-url", "mul"
	argVersion, aliasVersion                 string = "version", "v"
	argSmtpHost, aliasSmtpHost               string = "smtp-host", "sho"
	argSmtpPort, aliasSmtpPort               string = "smtp-port", "spo"
	argSmtpFrom, aliasSmtpFrom               string = "smtp-from", "sfr"
	argSmtpUser, aliasSmtpUser               string = "smtp-user", "sus"
	argSmtpPass, aliasSmtpPass               string = "smtp-pass", "spa"

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
	defaultTTY             bool          = true // ← new
	defaultObserve         bool          = false
	defaultMetricsPort     int           = 9090
	defaultMetricsURLLabel string        = "path"

	// warnings
	warnTotalLemmings   int64 = 100_000
	warnTotalGoroutines int64 = 10_000
)

//go:embed VERSION
var versionBytes embed.FS

var currentVersion string

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
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer cancel()

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
		"Semaphore ceiling for concurrent goroutines. -1 disables the ceiling entirely (dangerous)").
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
		WithAlias(argCrawlDepth, aliasCrawl).
		WithValidator(argCrawlDepth, figtree.AssureIntInRange(1, 100))

	figs.NewList(argSaveTo, []string{defaultSaveTo}, "Report destinations — comma-separated list of local paths, s3:// URIs, or mailto: URIs.").
		WithAlias(argSaveTo, aliasSaveTo).
		WithValidator(argSaveTo, figtree.AssureListNotEmpty)

	figs.NewString(argSmtpHost, "", "SMTP server hostname. Overrides LEMMINGS_SMTP_HOST").
		WithAlias(argSmtpHost, aliasSmtpHost)

	figs.NewInt(argSmtpPort, 0, "SMTP server port (465=TLS, 587=STARTTLS, 25=plain). Overrides LEMMINGS_SMTP_PORT").
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

	if problems := figs.Problems(); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "config error:", p)
		}
		os.Exit(1)
	}

	if err := figs.Load(); err != nil {
		log.Fatalf("failed to load configuration: %v", err)
	}

	if *figs.Bool(argVersion) {
		fmt.Println(Version())
		os.Exit(0)
	}

	saveTo := *figs.List(argSaveTo)
	if env := os.Getenv("LEMMINGS_SAVE_TO"); env != "" {
		for _, s := range strings.Split(env, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				saveTo = append(saveTo, s)
			}
		}
	}

	// Deduplicate
	seen := make(map[string]bool)
	var dedupSaveTo []string
	for _, s := range saveTo {
		if !seen[s] {
			seen[s] = true
			dedupSaveTo = append(dedupSaveTo, s)
		}
	}
	saveTo = dedupSaveTo

	cfg := SwarmConfig{
		Hit:             *figs.String(argHit),
		Terrain:         *figs.Int64(argTerrain),
		Pack:            *figs.Int64(argPack),
		Limit:           *figs.Int(argLimit),
		Until:           *figs.Duration(argUntil),
		Ramp:            *figs.Duration(argRamp),
		Crawl:           *figs.Bool(argCrawl),
		CrawlDepth:      *figs.Int(argCrawlDepth),
		SaveTo:          saveTo,
		SMTPHost:        *figs.String(argSmtpHost),
		SMTPPort:        *figs.Int(argSmtpPort),
		SMTPUser:        *figs.String(argSmtpUser),
		SMTPPass:        *figs.String(argSmtpPass),
		SMTPFrom:        *figs.String(argSmtpFrom),
		DashboardPort:   *figs.Int(argDashboardPort),
		TTY:             *figs.Bool(argTTY), // ← new
		Version:         Version(),
		Observe:         *figs.Bool(argObserve),
		MetricsPort:     *figs.Int(argMetricsPort),
		MetricsURLLabel: *figs.String(argMetricsUrlLabel),
	}

	printBootSummary(cfg)

	swarm, err := NewSwarm(ctx, cfg)
	if err != nil {
		log.Fatalf("failed to initialize swarm: %v", err)
	}

	if err := swarm.Run(); err != nil {
		log.Fatalf("swarm error: %v", err)
	}

	if err := swarm.Report(ctx); err != nil {
		log.Fatalf("report error: %v", err)
	}
}

func printBootSummary(cfg SwarmConfig) {
	total := cfg.Terrain * cfg.Pack
	wallClock := cfg.Ramp + cfg.Until

	fmt.Printf("\nlemmings v%s\n", cfg.Version)
	fmt.Println("─────────────────────────────────────────")
	fmt.Printf("  target:        %s\n", cfg.Hit)
	fmt.Println()
	fmt.Printf("  terrain:       %s groups\n", formatInt(cfg.Terrain))
	fmt.Printf("  pack:          %s lemmings per terrain\n", formatInt(cfg.Pack))
	fmt.Printf("  total:         %s lemmings\n", formatInt(total))
	fmt.Println()
	fmt.Printf("  until:         %s per lemming\n", cfg.Until)
	fmt.Printf("  ramp:          %s to full concurrency\n", cfg.Ramp)
	fmt.Printf("  est. duration: ~%s wall clock\n", wallClock.Round(time.Second))
	fmt.Println()

	limitStr := fmt.Sprintf("%d goroutines", cfg.Limit)
	if cfg.Limit == -1 {
		limitStr = "UNLIMITED (⚠ danger zone)"
	}
	fmt.Printf("  limit:         %s\n", limitStr)
	fmt.Printf("  crawl:         %v (depth: %d)\n", cfg.Crawl, cfg.CrawlDepth)
	// Replace with:
	fmt.Printf("  save-to:\n")
	for _, dest := range cfg.SaveTo {
		fmt.Printf("    → %s\n", dest)
	}

	fmt.Printf("  tty:           %v\n", cfg.TTY) // ← new
	fmt.Printf("  dashboard:     http://localhost:%d\n", cfg.DashboardPort)
	fmt.Printf("  observe:       %v\n", cfg.Observe)
	if cfg.Observe {
		fmt.Printf("  metrics:     http://localhost:%d/metrics\n", cfg.MetricsPort)
		fmt.Printf("  url-label:   %s\n", cfg.MetricsURLLabel)
	}
	fmt.Println()

	warned := false

	if total > warnTotalLemmings {
		fmt.Printf("  ⚠  WARNING: %s total lemmings is a large run.\n", formatInt(total))
		fmt.Printf("              ensure your system and target can handle this load.\n")
		warned = true
	}

	if cfg.Terrain > warnTotalGoroutines {
		fmt.Printf("  ⚠  WARNING: %s terrain groups means %s goroutines\n",
			formatInt(cfg.Terrain), formatInt(cfg.Terrain))
		fmt.Printf("              at the terrain layer alone before packs spawn.\n")
		warned = true
	}

	if cfg.Limit == -1 {
		fmt.Printf("  ⚠  WARNING: -limit=-1 disables the semaphore entirely.\n")
		fmt.Printf("              %s goroutines may spawn simultaneously.\n",
			formatInt(total))
		warned = true
	}

	if warned {
		fmt.Println()
	}

	fmt.Println("─────────────────────────────────────────")
	fmt.Println("  lemmings are gathering...")
	fmt.Println()
}

func formatTotal(terrain, pack int64) string {
	return formatInt(terrain * pack)
}

func formatInt(n int64) string {
	str := fmt.Sprintf("%d", n)
	if len(str) <= 3 {
		return str
	}
	result := make([]byte, 0, len(str)+(len(str)-1)/3)
	offset := len(str) % 3
	if offset > 0 {
		result = append(result, str[:offset]...)
	}
	for i := offset; i < len(str); i += 3 {
		if len(result) > 0 {
			result = append(result, ',')
		}
		result = append(result, str[i:i+3]...)
	}
	return string(result)
}
