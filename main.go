package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/andreimerlescu/figtree/v2"
)

const (
	defaultHit           string        = "http://localhost:8080/"
	defaultTerrain       int64         = 50
	defaultPack          int64         = 50
	defaultLimit         int           = 100
	defaultUntil         time.Duration = 30 * time.Second
	defaultRamp          time.Duration = 5 * time.Minute
	defaultCrawl         bool          = false
	defaultCrawlDepth    int           = 3
	defaultSaveTo        string        = "."
	defaultDashboardPort int           = 4000
	defaultTTY           bool          = true  // ← new

	defaultObserve         bool   = false
	defaultMetricsPort     int    = 9090
	defaultMetricsURLLabel string = "path"


	warnTotalLemmings   int64 = 100_000
	warnTotalGoroutines int64 = 10_000

	version = "1.0.0"
)

func main() {
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer cancel()

	figs := figtree.Grow()

	figs.NewString("hit", defaultHit, "Origin URL for lemmings to traverse").
		WithAlias("hit", "h").
		WithValidator("hit", figtree.AssureStringNotEmpty).
		WithValidator("hit", figtree.AssureStringHasPrefix("http"))

	figs.NewInt64("terrain", defaultTerrain, "Number of terrain goroutine groups (states)").
		WithAlias("terrain", "t").
		WithValidator("terrain", figtree.AssureInt64InRange(1, 10_000))

	figs.NewInt64("pack", defaultPack, "Number of lemmings per terrain group (cities)").
		WithAlias("pack", "p").
		WithValidator("pack", figtree.AssureInt64InRange(1, 100_000))

	figs.NewInt("limit", defaultLimit,
		"Semaphore ceiling for concurrent goroutines. -1 disables the ceiling entirely (dangerous)").
		WithAlias("limit", "l").
		WithValidator("limit", figtree.AssureIntInRange(-1, 1_000_000))

	figs.NewDuration("until", defaultUntil, "How long each lemming lives").
		WithAlias("until", "u").
		WithValidator("until", figtree.AssureDurationGreaterThan(0)).
		WithValidator("until", figtree.AssureDurationLessThan(24*time.Hour))

	figs.NewDuration("ramp", defaultRamp, "Duration to bring all terrain groups online").
		WithAlias("ramp", "r").
		WithValidator("ramp", figtree.AssureDurationGreaterThan(0)).
		WithValidator("ramp", figtree.AssureDurationLessThan(24*time.Hour))

	figs.NewBool("crawl", defaultCrawl, "Crawl origin for links when sitemap unavailable").
		WithAlias("crawl", "c")

	figs.NewInt("crawl-depth", defaultCrawlDepth, "How many links deep to crawl").
		WithAlias("crawl-depth", "cd").
		WithValidator("crawl-depth", figtree.AssureIntInRange(1, 100))

	// Remove:
figs.NewString("save-to", defaultSaveTo, ...)

// Replace with:
figs.NewList("save-to", []string{defaultSaveTo},
    "Report destinations — comma-separated list of local paths, "+
        "s3:// URIs, or mailto: URIs. "+
        "Env: LEMMINGS_SAVE_TO").
    WithAlias("save-to", "s").
    WithValidator("save-to", figtree.AssureListNotEmpty)

figs.NewString("smtp-host", "", "SMTP server hostname. Overrides LEMMINGS_SMTP_HOST").
    WithAlias("smtp-host", "sh")

figs.NewInt("smtp-port", 0, "SMTP server port (465=TLS, 587=STARTTLS, 25=plain). Overrides LEMMINGS_SMTP_PORT").
    WithAlias("smtp-port", "sp").
    WithValidator("smtp-port", figtree.AssureIntInRange(0, 65535))

figs.NewString("smtp-user", "", "SMTP username. Overrides LEMMINGS_SMTP_USER").
    WithAlias("smtp-user", "su")

figs.NewString("smtp-pass", "", "SMTP password. Overrides LEMMINGS_SMTP_PASS").
    WithAlias("smtp-pass", "spw")

figs.NewString("smtp-from", "", "SMTP from address. Overrides LEMMINGS_SMTP_FROM").
    WithAlias("smtp-from", "sf")

	
	figs.NewInt("dashboard-port", defaultDashboardPort, "Port for the live dashboard").
		WithAlias("dashboard-port", "dp").
		WithValidator("dashboard-port", figtree.AssureIntInRange(1024, 65535))

	figs.NewBool("observe", defaultObserve, "Enable Prometheus metrics exporter on -metrics-port").
         WithAlias("observe", "obs")

	figs.NewInt("metrics-port", defaultMetricsPort, "Port for the Prometheus /metrics endpoint").
	     WithAlias("metrics-port", "mp").
	     WithValidator("metrics-port", figtree.AssureIntInRange(1024, 65535))

	figs.NewString("metrics-url-label", defaultMetricsURLLabel, "URL label granularity on visit duration histogram: full, path, or none. Capped at 999 distinct values. Use none for sites with large URL surfaces.").
         WithAlias("metrics-url-label", "mul").
         WithValidator("metrics-url-label", figtree.AssureStringInSet("full", "path", "none"))

	figs.NewBool("tty", defaultTTY,
		"Use carriage return for live STDOUT updates. Set false for CI pipelines")

	if problems := figs.Problems(); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "config error:", p)
		}
		os.Exit(1)
	}

	if err := figs.Load(); err != nil {
		log.Fatalf("failed to load configuration: %v", err)
	}

	saveTo := *figs.List("save-to")
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
		Hit:           *figs.String("hit"),
		Terrain:       *figs.Int64("terrain"),
		Pack:          *figs.Int64("pack"),
		Limit:         *figs.Int("limit"),
		Until:         *figs.Duration("until"),
		Ramp:          *figs.Duration("ramp"),
		Crawl:         *figs.Bool("crawl"),
		CrawlDepth:    *figs.Int("crawl-depth"),
		SaveTo:   saveTo,
		SMTPHost: *figs.String("smtp-host"),
		SMTPPort: *figs.Int("smtp-port"),
		SMTPUser: *figs.String("smtp-user"),
		SMTPPass: *figs.String("smtp-pass"),
		SMTPFrom: *figs.String("smtp-from"),
		DashboardPort: *figs.Int("dashboard-port"),
		TTY:           *figs.Bool("tty"), // ← new
		Version:       version,
		Observe:         *figs.Bool("observe"),
		MetricsPort:     *figs.Int("metrics-port"),
		MetricsURLLabel: *figs.String("metrics-url-label"),
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

	fmt.Printf("  tty:           %v\n", cfg.TTY)          // ← new
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
