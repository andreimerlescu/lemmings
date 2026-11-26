package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/forensics"
	"github.com/andreimerlescu/lemmings/v2/internal/plan"
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

func TestUI_ConcertAssetsScoutsAndDataDroppedSummary(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	cfg := swarm.Config{
		Hit:            "http://127.0.0.1:8080/",
		Terrain:        2,
		Pack:           3,
		Limit:          6,
		Until:          10 * time.Second,
		Ramp:           time.Second,
		RampShape:      swarm.RampLinear,
		Version:        "2.0.0",
		SaveTo:         []string{"."},
		Assets:         true,
		AssetsPerPage:  12,
		AllowRemoteCDP: true,
		Scouts:         2,
		TTY:            false,
	}
	u := New(cfg, &buf)
	u.PrintBootSummary(cfg, "remote CDP")
	u.PrintFinalSummary(cfg, time.Now().Add(-2*time.Second), &swarm.Metrics{}, report.Data{
		TotalLemmings:     6,
		LemmingsStarted:   5,
		LemmingsCompleted: 4,
		LemmingsUnborn:    1,
		TotalVisits:       20,
		TotalBytes:        4096,
		P50:               12 * time.Millisecond,
		P95:               35 * time.Millisecond,
		P99:               50 * time.Millisecond,
		Assets: report.AssetReport{
			Requests:  16,
			Failed:    1,
			Denied403: 0,
			Bytes:     8192,
			P50:       4 * time.Millisecond,
			P95:       9 * time.Millisecond,
			P99:       11 * time.Millisecond,
		},
		Room: report.RoomReport{
			Held:        4,
			Admitted:    3,
			Expired:     1,
			Denied:      0,
			Rejected503: 2,
			TotalPolls:  9,
			MaxPosition: 7,
			P95Wait:     2 * time.Second,
			LongestWait: 3 * time.Second,
		},
		Concert: report.ConcertReport{
			AdmitPasses: 3,
			RoomTickets: 4,
			Bans429:     1,
			Bans403:     0,
		},
		Scouts: report.ScoutReport{
			Visits:   6,
			Failures: 1,
		},
		DroppedLogs:  2,
		TraceDropped: 3,
		Forensics: report.ForensicsReport{
			Stats: forensics.Stats{
				Captured:     2,
				DroppedQueue: 4,
				Mode:         forensics.ModeCDPReplay,
			},
		},
	})

	out := buf.String()
	for _, want := range []string{
		"assets:",
		"up to 12 uncached per page",
		"-allow-remote-cdp",
		"started:",
		"completed:",
		"unborn:",
		"page latency:",
		"asset latency:",
		"16 requests",
		"4 held",
		"3 admitted",
		"9 polls",
		"3 admit passes",
		"4 room tickets",
		"1 bans (429)",
		"6 visits",
		"1 failures",
		"DATA DROPPED",
		"2 life records dropped",
		"3 JSONL trace entries dropped",
		"4 forensic error captures dropped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in UI output:\n%s", want, out)
		}
	}
}

func TestUI_SinglePlanAndMultiPersonaBootDryRunAndSummary(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	chill := 250 * time.Millisecond

	singleSuite, err := plan.NewSuite([]*plan.Plan{
		{
			Name:       "auth-flow",
			Weight:     1,
			StopOnFail: true,
			Loop:       false,
			Items: []plan.PlanItem{
				{Name: "home", Path: "/", Method: "GET"},
				{
					Name:   "submit-login",
					Path:   "/login?secret=should-strip",
					Method: "POST",
					Chill:  &chill,
					WithBody: []plan.PlanBody{
						{Key: "password", Value: "top-secret-password"},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cfg := swarm.Config{
		Hit:             "http://127.0.0.1:8080/",
		Terrain:         2,
		Pack:            10,
		Limit:           20,
		Until:           30 * time.Second,
		Ramp:            time.Second,
		RampShape:       swarm.RampLinear,
		Version:         "2.0.0",
		SaveTo:          []string{"."},
		Plan:            singleSuite,
		MaxPlanItems:    250,
		RogueRate:       0.05,
		PlanPassGate:    true,
		MinPlanPassRate: 0.95,
		TTY:             false,
	}
	u := New(cfg, &buf)
	u.PrintDryRun(cfg, &discovery.URLPool{Origin: "http://127.0.0.1:8080", URLs: []string{"http://127.0.0.1:8080/"}}, "HTTP-only")

	singleOut := buf.String()
	for _, want := range []string{
		"plan:",
		"auth-flow (2 steps)",
		"rogue rate:",
		"5.0% rogue · 95.0% plan",
		"max plan items:",
		"250",
		"plan pass rate ≥ 95.00%",
		"weight 1, 2 steps, stop-on-fail=true, loop=false",
		"1. GET",
		"2. POST",
		"/login",
		"submit-login",
		"(chill 250ms)",
	} {
		if !strings.Contains(singleOut, want) {
			t.Errorf("expected %q in single-plan dry-run output:\n%s", want, singleOut)
		}
	}
	for _, forbidden := range []string{"top-secret-password", "should-strip"} {
		if strings.Contains(singleOut, forbidden) {
			t.Errorf("secret %q leaked into dry-run output:\n%s", forbidden, singleOut)
		}
	}

	multiSuite, err := plan.NewSuite([]*plan.Plan{
		{
			Name:       "casual-browser",
			Weight:     5,
			StopOnFail: false,
			Loop:       true,
			Items: []plan.PlanItem{
				{Name: "home", Path: "/"},
				{Name: "games", Path: "/games"},
			},
		},
		{
			Name:       "token-buyer",
			Weight:     3,
			StopOnFail: true,
			Loop:       false,
			Items: []plan.PlanItem{
				{Name: "checkout", Path: "/checkout", Method: "POST"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	buf.Reset()
	cfg.Plan = multiSuite
	cfg.MaxPlanItems = plan.DefaultMaxPlanItems
	cfg.RogueRate = 0.0
	u.PrintDryRun(cfg, &discovery.URLPool{Origin: "http://127.0.0.1:8080", URLs: []string{"http://127.0.0.1:8080/"}}, "HTTP-only")
	u.PrintFinalSummary(cfg, time.Now().Add(-time.Second), &swarm.Metrics{}, report.Data{
		TotalLemmings:     20,
		LemmingsStarted:   20,
		LemmingsCompleted: 20,
		TotalVisits:       40,
		Plan: &report.PlanReport{
			RogueRate:     0.05,
			PlanLemmings:  19,
			PlanPassed:    17,
			PlanFailed:    2,
			PlanPassRate:  89.4736842,
			RogueLemmings: 1,
			RogueFlawless: 1,
			Personas: []report.PersonaReport{
				{
					Name:       "casual-browser",
					Weight:     5,
					TotalSteps: 2,
					Lemmings:   12,
					Passed:     12,
					Failed:     0,
					PassRate:   100.0,
					Steps: []report.PlanStepReport{
						{Index: 1, Name: "home", Method: "GET", Path: "/", Hits: 12, Passed: 12},
						{Index: 2, Name: "games", Method: "GET", Path: "/games", Hits: 12, Passed: 12},
					},
				},
				{
					Name:       "token-buyer",
					Weight:     3,
					TotalSteps: 1,
					Lemmings:   7,
					Passed:     5,
					Failed:     2,
					PassRate:   71.4285714,
					Steps: []report.PlanStepReport{
						{Index: 1, Name: "checkout", Method: "POST", Path: "/checkout", Hits: 7, Passed: 5, Failed: 2, TopFailure: "status: received 500 (want 200)"},
					},
				},
			},
		},
	})

	multiOut := buf.String()
	for _, want := range []string{
		"2 personas (casual-browser, token-buyer; 3 total steps)",
		"0.0% rogue · 100.0% plan",
		"casual-browser (weight 5, 2 steps, stop-on-fail=false, loop=true)",
		"token-buyer (weight 3, 1 step, stop-on-fail=true, loop=false)",
		"plan cohort:",
		"17/19 (89.5%) · rogue: 1 (1 flawless)",
		"casual-browser",
		"12/12 passed (100.0%)",
		"token-buyer",
		"5/7 passed (71.4%) · 2 failed",
		"[top failure: #1 checkout: status: received 500 (want 200)]",
	} {
		if !strings.Contains(multiOut, want) {
			t.Errorf("expected %q in multi-persona output:\n%s", want, multiOut)
		}
	}
	if strings.Contains(multiOut, "max plan items:") {
		t.Errorf("did not expect default max plan items line in output:\n%s", multiOut)
	}
}
