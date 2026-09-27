package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// These end-to-end tests build the real binary and drive it the way an
// engineer or a CI pipeline would: flags in, exit code and report files
// out, with the dashboard exercised while the swarm runs. They are skipped
// with -short.

// buildBinary compiles lemmings once per test binary run.
var buildBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "lemmings-e2e-")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "lemmings")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go build: %v\n%s", err, out)
	}
	return bin, nil
})

// e2eBinary returns the built binary or skips the test.
func e2eBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("end-to-end tests are skipped with -short")
	}
	bin, err := buildBinary()
	if err != nil {
		t.Fatal(err)
	}
	return bin
}

// e2eSite is a small target with a sitemap, linked pages and one page
// that always fails its journey check.
func e2eSite(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	page := func(title, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<!doctype html><html><head><title>%s</title></head><body><nav><a href="/">home</a><a href="/a">a</a><a href="/b">b</a></nav><main>%s</main></body></html>`, title, body)
		}
	}
	mux.HandleFunc("/{$}", page("Home", "Welcome home"))
	mux.HandleFunc("/a", page("Page A", "Alpha"))
	mux.HandleFunc("/b", page("Page B", "Bravo"))
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<urlset><url><loc>http://%[1]s/</loc></url><url><loc>http://%[1]s/a</loc></url><url><loc>http://%[1]s/b</loc></url></urlset>`, r.Host)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// writeJourney writes a journey file and returns its path.
func writeJourney(t *testing.T, j string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journey.json")
	if err := os.WriteFile(path, []byte(j), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// e2eArgs returns flags for a run of six lemmings living for until.
func e2eArgs(t *testing.T, hit, saveTo, until string, extra ...string) []string {
	l, port := freePort(t)
	l.Close()
	return append([]string{
		"-hit", hit + "/", "-terrain", "2", "-pack", "3", "-limit", "6",
		"-until", until, "-ramp", "100ms", "-think-min", "10ms", "-think-max", "30ms",
		"-tty=false", "-save-to", saveTo, "-dashboard-port", fmt.Sprint(port),
	}, extra...)
}

// readReport loads the JSON report the run wrote under dir.
func readReport(t *testing.T, dir string) ReportData {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "lemmings", "*", "lemmings.*.json"))
	if len(matches) != 1 {
		t.Fatalf("expected one JSON report under %s, found %v", dir, matches)
	}
	for _, ext := range []string{".md", ".html"} {
		if _, err := os.Stat(strings.TrimSuffix(matches[0], ".json") + ext); err != nil {
			t.Fatalf("missing %s report: %v", ext, err)
		}
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var d ReportData
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("JSON report does not parse: %v", err)
	}
	return d
}

// exitCode runs cmd and returns its exit code and combined output.
func exitCode(t *testing.T, cmd *exec.Cmd) (int, string) {
	t.Helper()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, out.String()
	case errors.As(err, &exit):
		return exit.ExitCode(), out.String()
	}
	t.Fatalf("run: %v\n%s", err, out.String())
	return -1, ""
}

// TestE2E_StartsAndReportsVersion verifies the binary starts with every
// flag registered — v0.0.2 exited on startup because two flags shared an
// alias — and that -version prints the embedded version.
func TestE2E_StartsAndReportsVersion(t *testing.T) {
	bin := e2eBinary(t)
	code, out := exitCode(t, exec.Command(bin, "-version", "-cd", "2"))
	if code != exitOK || strings.TrimSpace(out) != Version() {
		t.Fatalf("exit %d, output %q, want version %q", code, out, Version())
	}
}

// TestE2E_PassingRunWritesEveryFormat runs a healthy journey with gates
// and expects exit 0 and a complete report in all three formats.
func TestE2E_PassingRunWritesEveryFormat(t *testing.T) {
	bin := e2eBinary(t)
	site := e2eSite(t)
	dir := t.TempDir()
	journey := writeJourney(t, `{"name":"tour","steps":[
		{"name":"home","path":"/","expect":{"status":[200],"contains":["Welcome"],"selectors":["nav","main"]}},
		{"name":"a","path":"/a","expect":{"title_contains":"Page A"}}]}`)
	trace := filepath.Join(t.TempDir(), "visits.jsonl")

	code, out := exitCode(t, exec.Command(bin, e2eArgs(t, site.URL, dir, "3s",
		"-journey", journey, "-max-failure-rate", "0", "-p95-budget", "5s", "-trace-file", trace)...))
	if code != exitOK {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	d := readReport(t, dir)
	if d.Verdict != "PASSED" || d.Sessions != 6 || d.TotalVisits != 12 || d.FailedVisits != 0 || d.RescuedPercent != 100 {
		t.Fatalf("verdict=%s sessions=%d visits=%d failed=%d rescued=%.1f", d.Verdict, d.Sessions, d.TotalVisits, d.FailedVisits, d.RescuedPercent)
	}
	if d.Journey != "tour" || len(d.Gates) != 2 || len(d.Notable) == 0 || len(d.Timeline) == 0 {
		t.Fatalf("journey=%q gates=%d notable=%d timeline=%d", d.Journey, len(d.Gates), len(d.Notable), len(d.Timeline))
	}
	lines, err := os.ReadFile(trace)
	if err != nil || bytes.Count(lines, []byte("\n")) != 12 {
		t.Fatalf("trace should hold 12 visits: err=%v lines=%d", err, bytes.Count(lines, []byte("\n")))
	}
	if !strings.Contains(out, "All lemmings accounted for.") || !strings.Contains(out, "PASSED") {
		t.Fatalf("terminal verdict missing:\n%s", out)
	}
}

// TestE2E_FailingGateExitsTwo runs a journey with a failing check and
// expects exit code 2 with the failure explained in the report.
func TestE2E_FailingGateExitsTwo(t *testing.T) {
	bin := e2eBinary(t)
	site := e2eSite(t)
	dir := t.TempDir()
	journey := writeJourney(t, `{"name":"broken","steps":[
		{"name":"home","path":"/"},
		{"name":"missing","path":"/nope","expect":{"status":[200]}}]}`)

	code, out := exitCode(t, exec.Command(bin, e2eArgs(t, site.URL, dir, "3s", "-journey", journey, "-max-failure-rate", "0.1")...))
	if code != exitGateFailed {
		t.Fatalf("exit %d, want %d\n%s", code, exitGateFailed, out)
	}
	d := readReport(t, dir)
	if d.Verdict != "FAILED" || d.FailedVisits != 6 || d.RescuedPercent != 0 {
		t.Fatalf("verdict=%s failed=%d rescued=%.1f", d.Verdict, d.FailedVisits, d.RescuedPercent)
	}
	found := false
	for _, c := range d.FailedChecks {
		found = found || (c.Label == "status" && c.Count == 6)
	}
	if !found {
		t.Fatalf("the failed status check should be reported: %+v", d.FailedChecks)
	}
}

// TestE2E_DashboardAndInterrupt runs a long swarm, signs in to the
// dashboard with the one-click link, reads live state and a stage frame,
// then presses Ctrl-C and expects exit 130 with the report still saved.
func TestE2E_DashboardAndInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Interrupt cannot be sent to a process on Windows")
	}
	bin := e2eBinary(t)
	site := e2eSite(t)
	dir := t.TempDir()
	cmd := exec.Command(bin, e2eArgs(t, site.URL, dir, "60s")...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	linkRe := regexp.MustCompile(`dashboard:\s+(http://localhost:\d+)/#token=([0-9a-f]{64})`)
	found := make(chan []string, 1)
	var output bytes.Buffer
	var mu sync.Mutex
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			mu.Lock()
			output.WriteString(sc.Text() + "\n")
			mu.Unlock()
			if m := linkRe.FindStringSubmatch(sc.Text()); m != nil {
				found <- m
			}
		}
	}()
	var base, token string
	select {
	case m := <-found:
		base, token = m[1], m[2]
	case <-time.After(20 * time.Second):
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("no dashboard link printed:\n%s", output.String())
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	waitForServer(t, base+"/", 40, 50*time.Millisecond)
	resp, err := client.PostForm(base+"/auth", url.Values{"token": {token}})
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), `id="scene"`) || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("signing in should land on the dashboard with a CSP (status %d)", resp.StatusCode)
	}

	var state struct {
		Stage []stageSlot    `json:"stage"`
		Stats dashboardStats `json:"stats"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/api/state")
		if err != nil {
			t.Fatal(err)
		}
		_ = json.NewDecoder(resp.Body).Decode(&state)
		resp.Body.Close()
		if len(state.Stage) == 6 && state.Stats.Metrics.TotalVisits > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(state.Stage) != 6 || state.Stats.Metrics.TotalVisits == 0 {
		t.Fatalf("expected 6 lemmings on stage with visits, got %d on stage, %d visits", len(state.Stage), state.Stats.Metrics.TotalVisits)
	}

	resp, err = client.Get(base + "/api/lemming?id=" + state.Stage[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var life lemmingLife
	_ = json.NewDecoder(resp.Body).Decode(&life)
	resp.Body.Close()
	if life.ID != state.Stage[0].ID || life.Name == "" || life.Persona == "" {
		t.Fatalf("inspector returned %+v", life)
	}

	events, err := client.Get(base + "/events")
	if err != nil {
		t.Fatal(err)
	}
	gotFrame := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(events.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") && strings.Contains(sc.Text(), `"kind":"frame"`) {
				gotFrame <- true
				return
			}
		}
		gotFrame <- false
	}()
	select {
	case ok := <-gotFrame:
		if !ok {
			t.Fatal("event stream ended without a frame")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no stage frame arrived on /events")
	}
	events.Body.Close()

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != exitInterrupted {
			mu.Lock()
			defer mu.Unlock()
			t.Fatalf("expected exit %d after Ctrl-C, got %v\n%s", exitInterrupted, err, output.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("lemmings did not exit after Ctrl-C")
	}
	d := readReport(t, dir)
	if d.TotalVisits == 0 || d.Sessions != 6 {
		t.Fatalf("interrupted run should still report its visits: visits=%d sessions=%d", d.TotalVisits, d.Sessions)
	}
}
