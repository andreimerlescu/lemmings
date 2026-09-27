package main

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// smtpStub is a line-oriented SMTP server for delivery tests. It records
// every command it receives. When starttls is set it advertises STARTTLS,
// accepts the command, then fails the handshake by closing the connection.
type smtpStub struct {
	mu       sync.Mutex
	commands []string
	data     bool
}

// serve accepts one connection on l and speaks just enough SMTP.
func (s *smtpStub) serve(t *testing.T, l net.Listener, starttls bool) {
	t.Helper()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		write := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }
		write("220 stub ESMTP")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			s.mu.Lock()
			s.commands = append(s.commands, line)
			s.mu.Unlock()
			switch cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0]); cmd {
			case "EHLO":
				if starttls {
					write("250-stub")
					write("250 STARTTLS")
				} else {
					write("250 stub")
				}
			case "STARTTLS":
				write("220 go ahead")
				return // the handshake fails: the client sees EOF
			case "DATA":
				write("354 go ahead")
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
				}
				s.mu.Lock()
				s.data = true
				s.mu.Unlock()
				write("250 queued")
			case "QUIT":
				write("221 bye")
				return
			default:
				write("250 ok")
			}
		}
	}()
}

// saw reports whether a command with prefix was received.
func (s *smtpStub) saw(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.commands {
		if strings.HasPrefix(strings.ToUpper(c), prefix) {
			return true
		}
	}
	return false
}

// stubTarget returns a MailTarget pointed at l.
func stubTarget(l net.Listener) *MailTarget {
	return &MailTarget{
		to: []string{"ops@example.com"}, subject: "Report",
		smtpCfg: SMTPConfig{Host: "127.0.0.1", Port: l.Addr().(*net.TCPAddr).Port, From: "lemmings@localhost"},
	}
}

// TestMailTarget_FailedSTARTTLSIsNeverDowngraded verifies that when a
// server offers STARTTLS but the handshake fails, delivery fails instead
// of resending the report in plaintext.
func TestMailTarget_FailedSTARTTLSIsNeverDowngraded(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	stub := &smtpStub{}
	stub.serve(t, l, true)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := stubTarget(l).Deliver(ctx, report("f", "md", "html")); err == nil {
		t.Fatal("a failed STARTTLS handshake must fail delivery")
	}
	if !stub.saw("STARTTLS") || stub.saw("MAIL FROM") || stub.data {
		t.Fatalf("the report was sent without TLS: %v", stub.commands)
	}
}

// TestMailTarget_PlainWhenServerOffersNoTLS verifies delivery to a server
// that does not advertise STARTTLS, such as a local relay.
func TestMailTarget_PlainWhenServerOffersNoTLS(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	stub := &smtpStub{}
	stub.serve(t, l, false)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := stubTarget(l).Deliver(ctx, report("f", "md", "html")); err != nil {
		t.Fatalf("delivery to a plain relay failed: %v", err)
	}
	if stub.saw("STARTTLS") || !stub.data {
		t.Fatalf("unexpected conversation: %v", stub.commands)
	}
}

// TestMailTarget_DeliverHonoursContext verifies a stalled server cannot
// hang delivery past the context deadline.
func TestMailTarget_DeliverHonoursContext(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(2 * time.Second) // never greets
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := stubTarget(l).Deliver(ctx, report("f", "md", "html")); err == nil {
		t.Fatal("expected a timeout")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("delivery ignored its deadline: took %s", time.Since(start))
	}
}

// TestMailTarget_SubjectIsEncodedAndCannotInjectHeaders verifies RFC 2047
// encoding for non-ASCII subjects and that line breaks cannot add headers.
func TestMailTarget_SubjectIsEncodedAndCannotInjectHeaders(t *testing.T) {
	mt := &MailTarget{to: []string{"ops@example.com"}, subject: "Lemmings Report — 2026", smtpCfg: SMTPConfig{From: "l@localhost"}}
	msg, err := mt.buildMessage("f", "md", "html")
	if err != nil {
		t.Fatal(err)
	}
	head := string(msg)[:strings.Index(string(msg), "\r\n\r\n")]
	if strings.Contains(head, "—") || !strings.Contains(head, "Subject: =?utf-8?q?") {
		t.Fatalf("non-ASCII subject not encoded:\n%s", head)
	}

	mt.subject = "hello\r\nBcc: attacker@example.com"
	msg, _ = mt.buildMessage("f", "md", "html")
	if strings.Contains(string(msg), "\r\nBcc:") {
		t.Fatal("subject injected a header")
	}
}

// TestLocalTarget_WritesJSON verifies the JSON report is written beside
// the markdown and HTML.
func TestLocalTarget_WritesJSON(t *testing.T) {
	dir := t.TempDir()
	lt := &LocalTarget{basePath: dir}
	r := report("lemmings.2026.09.27.example.com", "md", "html")
	r.JSON = []byte(`{"ok":true}`)
	if err := lt.Deliver(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "lemmings", "example.com", r.Filename+".json"))
	if err != nil || string(b) != `{"ok":true}` {
		t.Fatalf("json report: %q, %v", b, err)
	}
}

// TestS3Target_MissingRegionIsExplained verifies the S3 target explains a
// missing region instead of surfacing a raw SDK error.
func TestS3Target_MissingRegionIsExplained(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_PROFILE", "")
	target, err := ParseTarget("s3://bucket/prefix")
	if err != nil {
		t.Fatal(err)
	}
	err = target.Deliver(context.Background(), report("f", "md", "html"))
	if err == nil || !strings.Contains(err.Error(), "AWS_REGION") {
		t.Fatalf("expected a region hint, got %v", err)
	}
}

// TestReportJSON_NeverContainsDeliverySecrets verifies that credentials
// and delivery destinations stay out of the JSON report.
func TestReportJSON_NeverContainsDeliverySecrets(t *testing.T) {
	cfg := testConfig()
	cfg.Hit = "https://user:hunter2@example.com/?token=abc"
	cfg.SMTPPass, cfg.SMTPUser = "smtp-secret", "smtp-user"
	cfg.SaveTo = []string{"mailto:ceo@example.com", "s3://private-bucket"}
	r := NewReporter(cfg)
	r.RecordVisit(&Visit{URL: "https://example.com/", StatusCode: 200, Duration: time.Millisecond})
	out, err := renderReport(r.Result(), "f")
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{string(out.JSON), out.HTML, out.Markdown} {
		for _, secret := range []string{"hunter2", "token=abc", "smtp-secret", "smtp-user", "ceo@example.com", "private-bucket"} {
			if strings.Contains(format, secret) {
				t.Fatalf("report leaks %q", secret)
			}
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.JSON, &decoded); err != nil {
		t.Fatalf("JSON report does not parse: %v", err)
	}
}

// TestReport_VerdictsAndRescue verifies the gate verdicts and the rescued
// share of lemmings.
func TestReport_VerdictsAndRescue(t *testing.T) {
	ll := func(failed int64) LifeLog { return LifeLog{Streamed: true, TotalVisits: 4, FailedVisits: failed} }
	build := func(cfg SwarmConfig) ReportData {
		r := NewReporter(cfg)
		for i := 0; i < 9; i++ {
			r.RecordVisit(&Visit{URL: "https://e.com/", StatusCode: 200, Duration: 10 * time.Millisecond, Failed: false})
		}
		r.RecordVisit(&Visit{URL: "https://e.com/x", StatusCode: 500, Duration: 900 * time.Millisecond, Failed: true})
		r.Ingest(ll(0))
		r.Ingest(ll(0))
		r.Ingest(ll(0))
		r.Ingest(ll(1))
		return r.Result()
	}
	cfg := testConfig()
	if d := build(cfg); d.Verdict != "NO GATES" || d.GatesFailed() || d.RescuedPercent != 75 || d.FailureRate != 0.1 {
		t.Fatalf("no gates: verdict=%s rescued=%.1f rate=%.2f", d.Verdict, d.RescuedPercent, d.FailureRate)
	}
	cfg.FailureGate, cfg.MaxFailureRate = true, 0.2
	if d := build(cfg); d.Verdict != "PASSED" {
		t.Fatalf("10%% failures under a 20%% gate should pass: %+v", d.Gates)
	}
	cfg.MaxFailureRate = 0.05
	if d := build(cfg); d.Verdict != "FAILED" || !d.GatesFailed() {
		t.Fatalf("10%% failures over a 5%% gate should fail: %+v", d.Gates)
	}
	cfg.FailureGate, cfg.P95Budget = false, 100*time.Millisecond
	if d := build(cfg); d.Verdict != "FAILED" || d.Gates[0].Name != "p95 latency" {
		t.Fatalf("a 900ms p95 over a 100ms budget should fail: %+v", d.Gates)
	}
	if line := rescueLine(build(testConfig())); line != "Oh no! Many lemmings hit trouble." {
		t.Fatalf("rescue line %q", line)
	}
}

// TestFormatInt_Exact verifies thousands separators, including negatives
// (v0.0.2 rendered -1000 as "-,1000").
func TestFormatInt_Exact(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0", 7: "7", 999: "999", 1000: "1,000", -1000: "-1,000", -999: "-999",
		1234567: "1,234,567", -1234567: "-1,234,567", math.MinInt64: "-9,223,372,036,854,775,808",
	} {
		if got := formatInt(n); got != want {
			t.Errorf("formatInt(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestSaveTargets_MergesFlagAndEnvironment verifies -save-to and
// LEMMINGS_SAVE_TO combine additively without duplicates.
func TestSaveTargets_MergesFlagAndEnvironment(t *testing.T) {
	got := saveTargets([]string{".", " s3://b/p "}, "s3://b/p, mailto:ops@example.com,,.")
	want := []string{".", "s3://b/p", "mailto:ops@example.com"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("saveTargets = %v, want %v", got, want)
	}
}

// TestEstimateWallClock accounts for the concurrency limit forcing waves.
func TestEstimateWallClock(t *testing.T) {
	cfg := SwarmConfig{Terrain: 10, Pack: 10, Limit: 30, Until: 10 * time.Second, Ramp: time.Minute}
	if got := estimateWallClock(cfg); got != time.Minute+40*time.Second {
		t.Fatalf("100 lemmings at 30 at a time = 4 waves: got %s", got)
	}
	cfg.Limit = -1
	if got := estimateWallClock(cfg); got != time.Minute+10*time.Second {
		t.Fatalf("unlimited: got %s", got)
	}
}

// TestUI_ColourOnlyForInteractiveTerminals verifies NO_COLOR and -tty=false.
func TestUI_ColourOnlyForInteractiveTerminals(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if newUI(SwarmConfig{Color: true, TTY: true}).color {
		t.Fatal("NO_COLOR must disable colour")
	}
	os.Unsetenv("NO_COLOR")
	if newUI(SwarmConfig{Color: true, TTY: false}).color {
		t.Fatal("-tty=false must disable colour")
	}
	u := ui{color: true, truecolor: true}
	if s := u.paint(tonePink, "hi"); visibleLen(s) != 2 || s == "hi" {
		t.Fatalf("paint %q", s)
	}
	if ui := (ui{}); ui.paint(tonePink, "hi") != "hi" || ui.wordmark() != "L E M M I N G S" {
		t.Fatal("colourless output must be plain")
	}
}
