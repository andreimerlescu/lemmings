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
