package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/forensics"
	"github.com/andreimerlescu/lemmings/v2/internal/report"
	"github.com/andreimerlescu/lemmings/v2/internal/swarm"
)

func TestUI_PlainOutputNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	cfg := swarm.Config{
		Hit:             "https://" + "alice:" + "redactedcheck@" + "example.com/",
		Terrain:         2,
		Pack:            3,
		Limit:           6,
		Until:           10 * time.Second,
		Ramp:            2 * time.Second,
		RampShape:       swarm.RampFibonacci,
		Version:         "2.0.0",
		SaveTo:          []string{"."},
		CaptureEnabled:  true,
		CaptureMax:      50,
		CapturePerError: 3,
		CaptureWorkers:  2,
		CaptureFormat:   swarm.CaptureFormatJPEG,
		TTY:             false,
		Color:           true,
	}
	u := New(cfg, &buf)
	u.PrintDryRun(cfg, &discovery.URLPool{Origin: "https://example.com", URLs: []string{"https://example.com/"}}, "Chrome 124")
	u.PrintCompare(report.CompareResult{
		Target:          "https://example.com/",
		OldRescuedPct:   100,
		NewRescuedPct:   95,
		RescuedDeltaPct: -5,
		Regressed:       true,
	})
	u.PrintFinalSummary(cfg, time.Now().Add(-time.Second), &swarm.Metrics{}, report.Data{
		TotalVisits: 10,
		Forensics:   report.ForensicsReport{Stats: forensics.Stats{Captured: 2, WithScreenshots: 2, Bug: 1, Mode: forensics.ModeCDPReplay}},
	})
	u.PrintVerdict(report.Data{Sessions: 6, Rescued: 6, RescuedPercent: 100, Verdict: "PASSED"}, false)

	out := buf.String()
	if strings.Contains(out, "\x1b[") {
		t.Errorf("ANSI escapes leaked when NO_COLOR=1 / non-TTY:\n%s", out)
	}
	if strings.Contains(out, "redactedcheck") {
		t.Error("URL credentials leaked into CLI output")
	}
	if !strings.Contains(out, "DRY RUN") || !strings.Contains(out, "REGRESSED") || !strings.Contains(out, "All lemmings accounted for.") {
		t.Errorf("expected sections missing from CLI output:\n%s", out)
	}
}
