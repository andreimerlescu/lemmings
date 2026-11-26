// Package swarm implements Lemmings' core load simulation bounded context:
// Swarm orchestration, Terrain goroutine groups, stateful Lemming visitors
// with isolated InspectingJar cookie jars, configurable ramp shapes, and exact
// atomic Metrics.
package swarm

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/plan"
	"github.com/andreimerlescu/lemmings/v2/internal/secrets"
)

// Ramp shapes supported by -ramp-shape.
const (
	RampLinear    = "linear"
	RampFibonacci = "fibonacci"
	RampStep      = "step"
)

// Capture image formats supported by -capture-format.
const (
	CaptureFormatJPEG = "jpeg"
	CaptureFormatPNG  = "png"
)

// DefaultAssetsPerPage bounds the number of uncached same-origin assets fetched
// per HTML page visit when -assets is enabled.
const DefaultAssetsPerPage = 8

var validReportSlug = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// DefaultConfig returns a Config populated with safe default values.
func DefaultConfig() Config {
	return Config{
		Terrain:                  1,
		Pack:                     1,
		Limit:                    1,
		Until:                    time.Minute,
		RampShape:                RampLinear,
		RequestTimeout:           DefaultRequestTimeout,
		MaxBodyBytes:             DefaultMaxBodyBytes,
		Navigation:               plan.NavigationLinks,
		Assets:                   false,
		AssetsPerPage:            DefaultAssetsPerPage,
		MaxPlanItems:             plan.DefaultMaxPlanItems,
		RogueRate:                -1,
		CaptureMax:               50,
		CapturePerError:          3,
		CaptureWorkers:           2,
		CaptureTimeout:           8 * time.Second,
		CaptureFormat:            CaptureFormatJPEG,
		AllowRemoteCDP:           false,
		IncludeRemoteScreenshots: false,
	}
}

// Config is the validated configuration value object shared across bounded contexts.
// Delivery credentials and paths are marked json:"-" so they can never serialize
// into shared JSON reports.
type Config struct {
	Hit             string        `json:"hit"`
	Terrain         int64         `json:"terrain"`
	Pack            int64         `json:"pack"`
	Limit           int           `json:"limit"`
	Until           time.Duration `json:"until_ns"`
	Ramp            time.Duration `json:"ramp_ns"`
	RampShape       string        `json:"ramp_shape"`
	Crawl           bool          `json:"crawl"`
	CrawlDepth      int           `json:"crawl_depth"`
	DashboardPort   int           `json:"-"`
	TTY             bool          `json:"-"`
	Color           bool          `json:"-"`
	Version         string        `json:"version"`
	Observe         bool          `json:"observe"`
	MetricsPort     int           `json:"-"`
	MetricsURLLabel string        `json:"metrics_url_label"`

	// Delivery settings never appear in reports (S5/S6).
	SaveTo         []string       `json:"-"`
	UniqueReport   bool           `json:"-"`
	ReportName     string         `json:"-"`
	SMTPHost       string         `json:"-"`
	SMTPPort       int            `json:"-"`
	SMTPUser       string         `json:"-"`
	SMTPPass       secrets.Secret `json:"-"`
	SMTPFrom       string         `json:"-"`
	SMTPRequireTLS bool           `json:"-"`

	// Lemming behaviour.
	ThinkMin       time.Duration `json:"think_min_ns"`
	ThinkMax       time.Duration `json:"think_max_ns"`
	RequestTimeout time.Duration `json:"request_timeout_ns"`
	MaxBodyBytes   int64         `json:"max_body_bytes"`
	MaxPages       int           `json:"max_pages"`
	Navigation     string        `json:"navigation"`
	StrictChecksum bool          `json:"strict_checksum"`
	Assets         bool          `json:"assets"`
	AssetsPerPage  int           `json:"assets_per_page"`
	PlanFile       string        `json:"-"`
	Plan           *plan.Suite   `json:"-"`
	MaxPlanItems   int           `json:"max_plan_items,omitempty"`
	RogueRate      float64       `json:"rogue_rate,omitempty"`
	TraceFile      string        `json:"-"`

	// Error forensics, CDP screenshots, browser Scouts & Admin Portal review.
	CaptureEnabled           bool          `json:"capture_enabled"`
	CaptureMax               int           `json:"capture_max"`
	CapturePerError          int           `json:"capture_per_error"`
	CaptureWorkers           int           `json:"capture_workers"`
	CaptureTimeout           time.Duration `json:"capture_timeout_ns"`
	CaptureFormat            string        `json:"capture_format"`
	ChromeBin                string        `json:"-"`
	CDPURL                   string        `json:"-"`
	AllowRemoteCDP           bool          `json:"-"`
	IncludeRemoteScreenshots bool          `json:"-"`
	Scouts                   int           `json:"scouts"`
	RevealSessionCookies     bool          `json:"-"`
	ReviewMode               bool          `json:"-"`
	DryRun                   bool          `json:"-"`

	// CI gates.
	FailureGate     bool          `json:"failure_gate"`
	MaxFailureRate  float64       `json:"max_failure_rate"`
	P95Budget       time.Duration `json:"p95_budget_ns"`
	PlanPassGate    bool          `json:"plan_pass_gate,omitempty"`
	MinPlanPassRate float64       `json:"min_plan_pass_rate,omitempty"`
	MinQueueHeld    int64         `json:"min_queue_held,omitempty"`
	MaxQueueWait    time.Duration `json:"max_queue_wait_ns,omitempty"`
}

// Validate applies safe defaults and rejects impossible configurations.
func (c *Config) Validate() error {
	if _, err := discovery.ParseOrigin(c.Hit); err != nil {
		return fmt.Errorf("-hit %q: %w", discovery.SafeURL(c.Hit), err)
	}
	switch {
	case c.Terrain < 1 || c.Terrain > 10_000:
		return fmt.Errorf("-terrain must be 1..10000, got %d", c.Terrain)
	case c.Pack < 0 || c.Pack > 100_000:
		return fmt.Errorf("-pack must be 0..100000, got %d", c.Pack)
	case c.Limit == 0 || c.Limit < -1:
		return fmt.Errorf("-limit must be positive or -1, got %d", c.Limit)
	case c.Until <= 0:
		return fmt.Errorf("-until must be positive, got %s", c.Until)
	case c.Ramp < 0:
		return fmt.Errorf("-ramp must not be negative, got %s", c.Ramp)
	case c.ThinkMin < 0 || c.ThinkMax < 0:
		return errors.New("-think-min and -think-max must not be negative")
	case c.ThinkMax < c.ThinkMin:
		return fmt.Errorf("-think-max (%s) must be at least -think-min (%s)", c.ThinkMax, c.ThinkMin)
	case c.RequestTimeout < 0:
		return errors.New("-request-timeout must not be negative")
	case c.MaxBodyBytes < 0 || c.MaxBodyBytes > 64<<20:
		return errors.New("-max-body-bytes must be 0..64MiB")
	case c.MaxPages < 0:
		return errors.New("-max-pages must not be negative")
	case c.AssetsPerPage < 0 || c.AssetsPerPage > 64:
		return fmt.Errorf("-assets-per-page must be 0..64, got %d", c.AssetsPerPage)
	case c.MaxPlanItems < 0 || c.MaxPlanItems > 1_000_000:
		return fmt.Errorf("-max-plan-items must be 0..1000000, got %d", c.MaxPlanItems)
	case c.P95Budget < 0:
		return errors.New("-p95-budget must not be negative")
	case c.MinQueueHeld < 0:
		return errors.New("-min-queue-held must not be negative")
	case c.MaxQueueWait < 0:
		return errors.New("-max-queue-wait must not be negative")
	case c.FailureGate && (math.IsNaN(c.MaxFailureRate) || c.MaxFailureRate < 0 || c.MaxFailureRate > 1):
		return fmt.Errorf("-max-failure-rate must be 0..1, got %v", c.MaxFailureRate)
	case c.CaptureMax < 0 || c.CaptureMax > 500:
		return fmt.Errorf("-capture-max must be 0..500, got %d", c.CaptureMax)
	case c.CapturePerError < 0 || c.CapturePerError > 50:
		return fmt.Errorf("-capture-per-error must be 0..50, got %d", c.CapturePerError)
	case c.CaptureWorkers < 0 || c.CaptureWorkers > 8:
		return fmt.Errorf("-capture-workers must be 0..8, got %d", c.CaptureWorkers)
	case c.CaptureTimeout < 0:
		return errors.New("-capture-timeout must not be negative")
	case c.Scouts < 0 || c.Scouts > 8:
		return fmt.Errorf("-scouts must be 0..8, got %d", c.Scouts)
	}

	switch c.Navigation {
	case "":
		c.Navigation = plan.NavigationLinks
	case plan.NavigationLinks, plan.NavigationRandom:
	default:
		return fmt.Errorf("-navigation must be %q or %q, got %q", plan.NavigationLinks, plan.NavigationRandom, c.Navigation)
	}

	switch strings.ToLower(c.RampShape) {
	case "", RampLinear:
		c.RampShape = RampLinear
	case RampFibonacci:
		c.RampShape = RampFibonacci
	case RampStep:
		c.RampShape = RampStep
	default:
		return fmt.Errorf("-ramp-shape must be linear, fibonacci, or step, got %q", c.RampShape)
	}

	switch strings.ToLower(c.CaptureFormat) {
	case "", CaptureFormatJPEG:
		c.CaptureFormat = CaptureFormatJPEG
	case CaptureFormatPNG:
		c.CaptureFormat = CaptureFormatPNG
	default:
		return fmt.Errorf("-capture-format must be jpeg or png, got %q", c.CaptureFormat)
	}

	if c.AssetsPerPage == 0 {
		c.AssetsPerPage = DefaultAssetsPerPage
	}
	if c.MaxPlanItems == 0 {
		c.MaxPlanItems = plan.DefaultMaxPlanItems
	}
	if c.CaptureMax == 0 {
		c.CaptureMax = 50
	}
	if c.CapturePerError == 0 {
		c.CapturePerError = 3
	}
	if c.CaptureWorkers == 0 {
		c.CaptureWorkers = 2
	}
	if c.CaptureTimeout == 0 {
		c.CaptureTimeout = 8 * time.Second
	}

	if c.ReportName != "" {
		if !validReportSlug.MatchString(c.ReportName) || strings.Contains(c.ReportName, "..") {
			return fmt.Errorf("-report-name %q must match [a-zA-Z0-9._-] without '..'", c.ReportName)
		}
	}

	if c.PlanFile != "" && c.Plan == nil {
		suite, err := plan.LoadSuite(c.PlanFile, c.Hit, c.MaxPlanItems)
		if err != nil {
			return err
		}
		c.Plan = suite
	}

	if math.IsNaN(c.RogueRate) || c.RogueRate < -1 || c.RogueRate > 1 {
		return fmt.Errorf("-rogue-rate must be -1 (auto) or 0..1, got %v", c.RogueRate)
	}
	if c.RogueRate < 0 {
		if c.Plan != nil {
			c.RogueRate = 0.05
		} else {
			c.RogueRate = 0
		}
	}

	if c.PlanPassGate {
		if c.Plan == nil {
			return errors.New("-min-plan-pass-rate requires -plan")
		}
		if math.IsNaN(c.MinPlanPassRate) || c.MinPlanPassRate < 0 || c.MinPlanPassRate > 1 {
			return fmt.Errorf("-min-plan-pass-rate must be 0..1, got %v", c.MinPlanPassRate)
		}
	}
	return nil
}

// EstimateWallClock predicts total wall-clock runtime: the ramp plus as many
// back-to-back lifespan waves as the concurrency ceiling forces.
func EstimateWallClock(cfg Config) time.Duration {
	total := max(cfg.Terrain*cfg.Pack, 1)
	limit := int64(cfg.Limit)
	if limit <= 0 || limit > total {
		limit = total
	}
	waves := (total + limit - 1) / limit
	return cfg.Ramp + time.Duration(waves)*cfg.Until
}
