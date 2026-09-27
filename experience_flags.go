package main

import (
	"github.com/andreimerlescu/figtree/v2"
	"time"
)

// Kept alongside the experience engine so main.go retains its existing CLI and
// destination configuration. Existing flags continue to work unchanged.
func registerExperienceFlags(f figtree.Plant) {
	f.NewDuration("think-min", 300*time.Millisecond, "Minimum pause between page visits; 0 for a saturation test")
	f.NewDuration("think-max", 1200*time.Millisecond, "Maximum randomized pause between page visits")
	f.NewDuration("request-timeout", 10*time.Second, "Independent per-request timeout")
	f.NewInt64("max-body-bytes", 2<<20, "Maximum decoded bytes read per page")
	f.NewInt("max-pages", 20, "Maximum pages per HTTP session; 0 runs until lifespan expires")
	f.NewString("navigation", "links", "Navigation mode: links or random")
	f.NewBool("strict-checksum", false, "Treat an index-time body checksum change as a failure")
	f.NewString("scenario", "", "JSON file containing an ordered GET journey and assertions")
	f.NewString("trace-file", "", "Write every HTTP visit to a new JSONL file; existing files are not overwritten")
	f.NewString("max-failure-rate", "-1", "CI failure-rate budget 0..1; -1 disables this gate")
	f.NewDuration("p95-budget", 0, "CI HTTP visit p95 budget; 0 disables this gate")
	f.NewInt("browser-users", 0, "Additional real Chromium sessions (0..32); requires browser dependencies")
	f.NewDuration("browser-until", 30*time.Second, "Maximum lifespan of each real browser session")
	f.NewString("browser-script", "browser/runner.cjs", "Path to the bundled Playwright runner")
	f.NewString("browser-output", "browser-results", "Directory for browser evidence and failure screenshots")
}
