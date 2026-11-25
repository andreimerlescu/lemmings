package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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

	"github.com/andreimerlescu/lemmings/v2/internal/forensics"
	"github.com/andreimerlescu/lemmings/v2/internal/report"
	"github.com/andreimerlescu/lemmings/v2/internal/version"
)

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

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

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
	t.Fatalf("exec failed: %v\n%s", err, out.String())
	return -1, ""
}

func e2eSite(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	page := func(title, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "e2e-secret-val", Path: "/", HttpOnly: true})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<!doctype html><html><head><title>%s</title></head><body><nav><a href="/">home</a><a href="/a">a</a><a href="/b">b</a></nav><main>%s</main></body></html>`, title, body)
		}
	}
	mux.HandleFunc("/{$}", page("Home", "Welcome home"))
	mux.HandleFunc("/a", page("Page A", "Alpha"))
	mux.HandleFunc("/b", page("Page B", "Bravo"))
	mux.HandleFunc("/boom", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("X-Request-Id", "req-e2e-500")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"title":"Boom Fault","code":"E_E2E_BOOM","error":"Exploded during checkout","request_id":"req-e2e-500"}`)
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<urlset><url><loc>http://%[1]s/</loc></url><url><loc>http://%[1]s/a</loc></url><url><loc>http://%[1]s/b</loc></url></urlset>`, r.Host)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeJourney(t *testing.T, j string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "journey.json")
	if err := os.WriteFile(p, []byte(j), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func e2eArgs(t *testing.T, hit, saveTo, until string, extra ...string) []string {
	port := freePort(t)
	return append([]string{
		"-hit", hit + "/", "-terrain", "2", "-pack", "3", "-limit", "6",
		"-until", until, "-ramp", "100ms", "-think-min", "10ms", "-think-max", "30ms",
		"-tty=false", "-capture=false", "-save-to", saveTo, "-dashboard-port", fmt.Sprint(port),
	}, extra...)
}

func readReport(t *testing.T, dir string) (report.Data, string) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "lemmings", "*", "*.json"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 JSON report under %s, found %v", dir, matches)
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
	var d report.Data
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("JSON decode: %v", err)
	}
	return d, matches[0]
}

func TestE2E_VersionAndNoSecretCLIFlag(t *testing.T) {
	bin := e2eBinary(t)
	code, out := exitCode(t, exec.Command(bin, "-version", "-cd", "2"))
	if code != exitOK || strings.TrimSpace(out) != version.String() {
		t.Fatalf("exit %d, output %q, want %q", code, out, version.String())
	}

	_, helpOut := exitCode(t, exec.Command(bin, "-help"))
	if strings.Contains(helpOut, "-smtp-pass ") || strings.Contains(helpOut, "-spw") {
		t.Fatalf("S4 violation: insecure -smtp-pass/-spw flag still present in -help:\n%s", helpOut)
	}
	if !strings.Contains(helpOut, "smtp-pass-file") {
		t.Fatalf("expected -smtp-pass-file in -help:\n%s", helpOut)
	}
}

func TestE2E_DryRun(t *testing.T) {
	bin := e2eBinary(t)
	site := e2eSite(t)
	code, out := exitCode(t, exec.Command(bin, "-hit", site.URL+"/", "-dry-run", "-capture=false", "-tty=false"))
	if code != exitOK {
		t.Fatalf("dry-run exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "DRY RUN") || !strings.Contains(out, "3 URLs indexed") {
		t.Fatalf("unexpected dry-run output:\n%s", out)
	}
}

func TestE2E_PassingRunWritesEveryFormat(t *testing.T) {
	bin := e2eBinary(t)
	site := e2eSite(t)
	dir := t.TempDir()
	jPath := writeJourney(t, `{"name":"tour","steps":[
		{"name":"home","path":"/","expect":{"status":[200],"contains":["Welcome"],"selectors":["nav","main"]}},
		{"name":"a","path":"/a","expect":{"title_contains":"Page A"}}]}`)
	trace := filepath.Join(t.TempDir(), "visits.jsonl")

	code, out := exitCode(t, exec.Command(bin, e2eArgs(t, site.URL, dir, "3s",
		"-journey", jPath, "-max-failure-rate", "0", "-p95-budget", "5s",
		"-trace-file", trace, "-unique-report")...))
	if code != exitOK {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	d, _ := readReport(t, dir)
	if d.Verdict != "PASSED" || d.Sessions != 6 || d.TotalVisits != 12 || d.FailedVisits != 0 || d.RescuedPercent != 100 {
		t.Fatalf("verdict=%s sessions=%d visits=%d failed=%d rescued=%.1f", d.Verdict, d.Sessions, d.TotalVisits, d.FailedVisits, d.RescuedPercent)
	}
	if !strings.Contains(out, "All lemmings accounted for.") || !strings.Contains(out, "PASSED") {
		t.Fatalf("missing verdict:\n%s", out)
	}
	lines, err := os.ReadFile(trace)
	if err != nil || bytes.Count(lines, []byte("\n")) != 12 {
		t.Fatalf("trace should have 12 visits: err=%v lines=%d", err, bytes.Count(lines, []byte("\n")))
	}
}

func TestE2E_ReviewModeTriageAndCompare(t *testing.T) {
	bin := e2eBinary(t)
	site := e2eSite(t)

	// 1. Baseline healthy run.
	baseDir := t.TempDir()
	goodJ := writeJourney(t, `{"name":"good","steps":[{"name":"home","path":"/","expect":{"status":[200]}}]}`)
	code, out := exitCode(t, exec.Command(bin, e2eArgs(t, site.URL, baseDir, "2s", "-journey", goodJ)...))
	if code != exitOK {
		t.Fatalf("baseline exit %d:\n%s", code, out)
	}
	_, baseJSON := readReport(t, baseDir)

	// 2. Failing run with -review mode: admin signs in, triages artifact as "bug", clicks Finalize.
	badDir := t.TempDir()
	badJ := writeJourney(t, `{"name":"broken","steps":[{"name":"home","path":"/"},{"name":"boom","path":"/boom","expect":{"status":[200]}}]}`)
	cmd := exec.Command(bin, e2eArgs(t, site.URL, badDir, "2s",
		"-journey", badJ, "-max-failure-rate", "0.1", "-review")...)
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
	foundLink := make(chan []string, 1)
	reviewReady := make(chan struct{}, 1)
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			logMu.Lock()
			logBuf.WriteString(line + "\n")
			logMu.Unlock()
			if m := linkRe.FindStringSubmatch(line); m != nil {
				select {
				case foundLink <- m:
				default:
				}
			}
			if strings.Contains(line, "REVIEW MODE") {
				select {
				case reviewReady <- struct{}{}:
				default:
				}
			}
		}
	}()

	var baseURL, token string
	select {
	case m := <-foundLink:
		baseURL, token = m[1], m[2]
	case <-time.After(15 * time.Second):
		t.Fatalf("dashboard link never printed:\n%s", logBuf.String())
	}
	select {
	case <-reviewReady:
	case <-time.After(15 * time.Second):
		t.Fatalf("review mode never activated:\n%s", logBuf.String())
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	resp, err := client.PostForm(baseURL+"/auth", url.Values{"token": {token}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	resp, err = client.Get(baseURL + "/api/artifacts")
	if err != nil {
		t.Fatal(err)
	}
	var listResp struct {
		Artifacts []forensics.Summary `json:"artifacts"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listResp)
	_ = resp.Body.Close()
	if len(listResp.Artifacts) == 0 {
		t.Fatalf("expected captured error artifacts in portal during -review mode")
	}

	// Admin triages the first artifact as "bug".
	artID := listResp.Artifacts[0].ID
	revResp, err := client.Post(baseURL+"/api/artifacts/"+artID+"/review", "application/json",
		strings.NewReader(`{"status":"bug","note":"e2e-verified-hopper-bug"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = revResp.Body.Close()
	if revResp.StatusCode != http.StatusOK {
		t.Fatalf("review status = %d, want 200", revResp.StatusCode)
	}

	// Admin clicks Finalize & Write Report.
	finResp, err := client.Post(baseURL+"/api/finalize", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = finResp.Body.Close()

	err = cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != exitGateFailed {
		logMu.Lock()
		defer logMu.Unlock()
		t.Fatalf("expected exit 2 after finalize, got %v\n%s", err, logBuf.String())
	}

	candData, candJSON := readReport(t, badDir)
	var triaged bool
	for _, a := range candData.Forensics.Artifacts {
		if a.ID == artID && a.ReviewStatus == forensics.ReviewBug && a.ReviewNote == "e2e-verified-hopper-bug" {
			triaged = true
			break
		}
	}
	if !triaged {
		t.Fatalf("admin triage note for %s missing from final report: %+v", artID, candData.Forensics)
	}
	rawJSON, _ := os.ReadFile(candJSON)
	if strings.Contains(string(rawJSON), "e2e-secret-val") {
		t.Fatal("raw session cookie value leaked into final JSON report")
	}

	// 3. `lemmings compare` must detect the regression and exit 2.
	cmpCode, cmpOut := exitCode(t, exec.Command(bin, "compare", "-max-failure-delta", "0.05", "-tty=false", baseJSON, candJSON))
	if cmpCode != exitGateFailed || !strings.Contains(cmpOut, "REGRESSED") {
		t.Fatalf("compare exit=%d want 2:\n%s", cmpCode, cmpOut)
	}
}

func TestE2E_DashboardAndInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Interrupt not supported on Windows")
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
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if m := linkRe.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case found <- m:
				default:
				}
			}
		}
	}()

	var baseURL, token string
	select {
	case m := <-found:
		baseURL, token = m[1], m[2]
	case <-time.After(15 * time.Second):
		t.Fatal("dashboard link never printed")
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	resp, err := client.PostForm(baseURL+"/auth", url.Values{"token": {token}})
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(page), `id="scene"`) || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("dashboard HTML or CSP missing (status %d)", resp.StatusCode)
	}

	// Verify SSE delivers a live frame.
	sseResp, err := client.Get(baseURL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	gotFrame := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(sseResp.Body)
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
			t.Fatal("SSE closed before frame")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no SSE frame received")
	}
	_ = sseResp.Body.Close()

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != exitInterrupted {
		t.Fatalf("expected exit %d after SIGINT, got %v", exitInterrupted, err)
	}
	d, _ := readReport(t, dir)
	if d.TotalVisits == 0 {
		t.Fatalf("expected interrupted run to still deliver visits, got %+v", d)
	}
}

func TestE2E_ConcertWaitingRoomAndAssetsTier(t *testing.T) {
	bin := e2eBinary(t)

	var mu sync.Mutex
	ticketSeq := 0
	admittedTickets := make(map[string]bool)

	serveConcertPage := func(w http.ResponseWriter, r *http.Request, title, body string) {
		if c, err := r.Cookie("concert_admit"); err == nil && strings.HasPrefix(c.Value, "pass-") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<!doctype html><html><head><title>%s</title><link rel="stylesheet" href="/assets/app.css"></head><body><img src="/assets/logo.svg" alt="logo"><nav><a href="/">Home</a><a href="/about">About</a></nav><main>%s</main></body></html>`, title, body)
			return
		}

		// If the visitor has an admitted room_ticket, mint concert_admit and serve origin page.
		if tc, err := r.Cookie("room_ticket"); err == nil && tc.Value != "" {
			mu.Lock()
			isAdmitted := admittedTickets[tc.Value]
			mu.Unlock()
			if isAdmitted {
				http.SetCookie(w, &http.Cookie{Name: "concert_admit", Value: "pass-" + tc.Value, Path: "/", HttpOnly: true})
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprintf(w, `<!doctype html><html><head><title>%s</title><link rel="stylesheet" href="/assets/app.css"></head><body><img src="/assets/logo.svg" alt="logo"><nav><a href="/">Home</a><a href="/about">About</a></nav><main>%s</main></body></html>`, title, body)
				return
			}
		}

		// Issue a room_ticket and render Concert waiting.html.
		mu.Lock()
		ticketSeq++
		ticket := fmt.Sprintf("tk-%d", ticketSeq)
		mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "room_ticket", Value: ticket, Path: "/", HttpOnly: true})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Retry-After", "1")
		fmt.Fprint(w, `<!doctype html><html><head><title>Waiting Room</title><script src="/_concert/waiting.js" defer></script></head><body class="queue-page"><h1>Please wait</h1><p>Position: <b id="position">2</b></p></body></html>`)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		serveConcertPage(w, r, "Concert Origin", "Admitted home content")
	})
	mux.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		serveConcertPage(w, r, "Concert About", "Admitted about content")
	})
	mux.HandleFunc("/queue/status", func(w http.ResponseWriter, r *http.Request) {
		tc, err := r.Cookie("room_ticket")
		if err != nil || tc.Value == "" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ready":false,"waiting":false,"cookies_required":true}`)
			return
		}
		mu.Lock()
		admittedTickets[tc.Value] = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ready":true,"waiting":false,"position":0}`)
	})
	mux.HandleFunc("/assets/app.css", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("concert_admit")
		if err != nil || !strings.HasPrefix(c.Value, "pass-") {
			http.Error(w, "concert pass required", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		fmt.Fprint(w, `body{color:#f7f1ff;background:#1d1727}`)
	})
	mux.HandleFunc("/assets/logo.svg", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("concert_admit")
		if err != nil || !strings.HasPrefix(c.Value, "pass-") {
			http.Error(w, "concert pass required", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		fmt.Fprint(w, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><circle cx="8" cy="8" r="7" fill="#ff7edb"/></svg>`)
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<urlset><url><loc>http://%[1]s/</loc></url><url><loc>http://%[1]s/about</loc></url></urlset>`, r.Host)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Confirm the mock Concert asset gate rejects unauthenticated requests with 403.
	unauthResp, err := http.Get(srv.URL + "/assets/app.css")
	if err != nil {
		t.Fatal(err)
	}
	_ = unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthenticated /assets/app.css status = %d, want 403", unauthResp.StatusCode)
	}

	dir := t.TempDir()
	args := []string{
		"-hit", srv.URL + "/",
		"-terrain", "2", "-pack", "2", "-limit", "4",
		"-until", "10s", "-ramp", "100ms",
		"-think-min", "10ms", "-think-max", "30ms",
		"-max-pages", "2",
		"-assets", "-assets-per-page", "8",
		"-tty=false", "-capture=false",
		"-save-to", dir,
		"-dashboard-port", fmt.Sprint(freePort(t)),
	}
	code, out := exitCode(t, exec.Command(bin, args...))
	if code != exitOK {
		t.Fatalf("Concert E2E exit %d, want 0:\n%s", code, out)
	}

	d, _ := readReport(t, dir)
	if d.Room.Held <= 0 || d.Room.Admitted <= 0 || d.Room.TotalPolls <= 0 {
		t.Fatalf("expected Concert waiting room admission telemetry, got %+v\n%s", d.Room, out)
	}
	if d.Assets.Requests <= 0 || d.Assets.Denied403 != 0 || d.Assets.Failed != 0 {
		t.Fatalf("expected admitted asset fetches with zero 403s, got %+v\n%s", d.Assets, out)
	}
	if d.Concert.AdmitPasses <= 0 || d.Concert.RoomTickets <= 0 {
		t.Fatalf("expected Concert passes and room tickets recorded, got %+v\n%s", d.Concert, out)
	}
	if !strings.Contains(out, "assets:") || !strings.Contains(out, "concert:") {
		t.Errorf("expected CLI final summary to render assets and concert sections:\n%s", out)
	}
}

func TestE2E_NonLoopbackCDPRejectedFailClosed(t *testing.T) {
	bin := e2eBinary(t)
	site := e2eSite(t)

	code, out := exitCode(t, exec.Command(bin,
		"-hit", site.URL+"/",
		"-cdp-url", "http://198.51.100.10:9222",
		"-dry-run",
		"-tty=false",
	))
	if code != exitError {
		t.Fatalf("non-loopback -cdp-url without -allow-remote-cdp exited %d, want %d:\n%s", code, exitError, out)
	}
	if !strings.Contains(out, "-allow-remote-cdp") {
		t.Fatalf("expected fail-closed error mentioning -allow-remote-cdp:\n%s", out)
	}

	// Also verify -assets-per-page 0 is rejected when -assets is enabled.
	code, out = exitCode(t, exec.Command(bin,
		"-hit", site.URL+"/",
		"-assets", "-assets-per-page", "0",
		"-dry-run", "-capture=false", "-tty=false",
	))
	if code != exitError || !strings.Contains(out, "-assets-per-page") {
		t.Fatalf("-assets with -assets-per-page 0 exited %d, want %d:\n%s", code, exitError, out)
	}
}
