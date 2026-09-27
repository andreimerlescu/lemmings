package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type BrowserReport struct {
	Engine        string         `json:"engine"`
	Sessions      int            `json:"sessions"`
	Visits        int64          `json:"visits"`
	Failed        int64          `json:"failed"`
	Cancelled     int64          `json:"cancelled"`
	ResponseCodes map[int]int64  `json:"response_codes"`
	Samples       []BrowserVisit `json:"samples"`
	Omitted       int64          `json:"omitted"`
	Error         string         `json:"error,omitempty"`
	Output        string         `json:"output"`
}
type BrowserVisit struct {
	Session         string             `json:"session"`
	URL             string             `json:"url"`
	FinalURL        string             `json:"final_url"`
	Step            string             `json:"step"`
	Status          int                `json:"status"`
	Title           string             `json:"title"`
	Failed          bool               `json:"failed"`
	Cancelled       bool               `json:"cancelled"`
	Checks          []CheckResult      `json:"checks"`
	ConsoleErrors   []string           `json:"console_errors"`
	PageErrors      []string           `json:"page_errors"`
	FailedResources []string           `json:"failed_resources"`
	BlockedExternal int                `json:"blocked_external"`
	Screenshot      string             `json:"screenshot"`
	DurationMS      float64            `json:"duration_ms"`
	Performance     map[string]float64 `json:"performance"`
}

// boundedOutput prevents a broken/custom runner from exhausting Go memory.
type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	space := b.limit - b.Len()
	if space > 0 {
		if len(p) > space {
			p = p[:space]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

func checkBrowser(ctx context.Context, cfg SwarmConfig) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", cfg.Experience.BrowserScript, "--check")
	out := &boundedOutput{limit: 16 << 10}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, out.String())
	}
	return nil
}
func runBrowser(ctx context.Context, cfg SwarmConfig, ready chan struct{}) BrowserReport {
	var readyOnce sync.Once
	notify := func() { readyOnce.Do(func() { close(ready) }) }
	defer notify()
	x := cfg.Experience
	dir, err := filepath.Abs(x.BrowserOutput)
	if err != nil {
		return BrowserReport{Error: err.Error()}
	}
	input := map[string]any{"url": cfg.Hit, "users": x.BrowserUsers, "duration_ms": x.BrowserUntil.Milliseconds(), "think_min_ms": x.ThinkMin.Milliseconds(), "think_max_ms": x.ThinkMax.Milliseconds(), "timeout_ms": x.RequestTimeout.Milliseconds(), "max_pages": x.MaxPages, "scenario": x.Scenario, "output": dir, "stream_ready": true}
	data, _ := json.Marshal(input)
	cmd := exec.CommandContext(ctx, "node", x.BrowserScript)
	cmd.Stdin = bytes.NewReader(data)
	stderr := &boundedOutput{limit: 16 << 10}
	cmd.Stderr = stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return BrowserReport{Error: err.Error()}
	}
	if err = cmd.Start(); err != nil {
		return BrowserReport{Error: err.Error()}
	}
	decoder := json.NewDecoder(io.LimitReader(stdout, 16<<20))
	var handshake struct {
		Ready bool `json:"ready"`
	}
	if err = decoder.Decode(&handshake); err != nil || !handshake.Ready {
		_ = cmd.Wait()
		return BrowserReport{Error: fmt.Sprintf("browser startup did not become ready: %v; %s", err, clip(stderr.String(), 2048))}
	}
	notify()
	var report BrowserReport
	decodeErr := decoder.Decode(&report)
	err = cmd.Wait()
	if decodeErr != nil {
		report.Error = fmt.Sprintf("browser runner did not produce a report: %v; %s", decodeErr, clip(stderr.String(), 2048))
	}
	if err != nil && report.Error == "" && ctx.Err() == nil {
		report.Error = fmt.Sprintf("browser runner: %v; %s", err, clip(stderr.String(), 2048))
	}
	return report
}
