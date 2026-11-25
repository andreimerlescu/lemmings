package delivery

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/report"
	"github.com/andreimerlescu/lemmings/v2/internal/secrets"
	"github.com/andreimerlescu/lemmings/v2/internal/swarm"
)

func TestParseTargetAndSaveTargets(t *testing.T) {
	if tgt, err := ParseTarget(".", nil); err != nil {
		t.Fatal(err)
	} else if _, ok := tgt.(*LocalTarget); !ok {
		t.Errorf("expected LocalTarget, got %T", tgt)
	}
	if tgt, err := ParseTarget("s3://my-bucket/reports", nil); err != nil {
		t.Fatal(err)
	} else if _, ok := tgt.(*S3Target); !ok {
		t.Errorf("expected S3Target, got %T", tgt)
	}
	if tgt, err := ParseTarget("mailto:ops@example.com?subject=Test", nil); err != nil {
		t.Fatal(err)
	} else if _, ok := tgt.(*MailTarget); !ok {
		t.Errorf("expected MailTarget, got %T", tgt)
	}
	if _, err := ParseTarget("   ", nil); err == nil {
		t.Error("empty URI must fail")
	}

	merged := SaveTargets([]string{".", "./out"}, "./out, s3://b/p")
	if len(merged) != 3 {
		t.Errorf("expected 3 deduplicated targets, got %v", merged)
	}
}

func TestLocalTarget_ConfinedRootAndArtifacts(t *testing.T) {
	dir := t.TempDir()
	lt := NewLocalTarget(dir, nil)

	rep := report.Rendered{
		Filename: "lemmings.2026.11.25.example.com",
		Domain:   "example.com",
		Markdown: "# report",
		HTML:     "<html>report</html>",
		JSON:     []byte(`{"ok":true}`),
		Artifacts: []report.ArtifactAsset{
			{Filename: "abc123.jpg", MIMEType: "image/jpeg", Data: []byte{0xff, 0xd8, 0xff, 0xd9}},
		},
	}
	if err := lt.Deliver(context.Background(), rep); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	dest := filepath.Join(dir, "lemmings", "example.com")
	for _, rel := range []string{
		"lemmings.2026.11.25.example.com.md",
		"lemmings.2026.11.25.example.com.html",
		"lemmings.2026.11.25.example.com.json",
		filepath.Join("lemmings.2026.11.25.example.com.artifacts", "abc123.jpg"),
	} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("missing delivered file %s: %v", rel, err)
		}
	}

	// SEC-06: path traversal filename MUST be rejected.
	bad := rep
	bad.Filename = "../../escaped"
	if err := lt.Deliver(context.Background(), bad); err == nil {
		t.Error("SEC-06 violation: LocalTarget allowed path traversal filename")
	}
}

func TestMailTarget_RejectsCRLFHeaderInjection(t *testing.T) {
	// SEC-02: CR/LF in To, Cc, or From must fail closed.
	if _, err := NewMailTarget("mailto:ops@example.com%0d%0aBcc:evil@attacker.test"); err == nil {
		t.Error("SEC-02: CRLF in mailto recipient must fail")
	}

	mt := &MailTarget{
		to:      []string{"ops@example.com"},
		subject: "Report\r\nBcc: evil@attacker.test",
		smtpCfg: SMTPConfig{From: "bad@example.com\r\nBcc: evil@attacker.test"},
	}
	if _, err := mt.buildMessage("rep", "md", "<p>hi</p>"); err == nil {
		t.Error("SEC-02: CRLF in From must fail buildMessage")
	}

	// Valid From with CRLF in Subject is sanitized/Q-encoded without injecting raw header lines.
	mt.smtpCfg.From = "lemmings@example.com"
	msg, err := mt.buildMessage("rep", "md", "<p>hi</p>")
	if err != nil {
		t.Fatalf("valid buildMessage failed: %v", err)
	}
	if strings.Contains(string(msg), "\r\nBcc:") {
		t.Fatalf("SEC-02: injected Bcc header appeared in MIME message:\n%s", msg)
	}
}

func TestMailTarget_RequireTLSFailsClosedOnPlaintextServer(t *testing.T) {
	// Spin up a minimal mock SMTP server advertising NO STARTTLS.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		fmt.Fprint(conn, "220 mock.local ESMTP\r\n")
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				fmt.Fprint(conn, "250-mock.local\r\n250 SIZE 1024000\r\n")
			case strings.HasPrefix(line, "QUIT"):
				fmt.Fprint(conn, "221 bye\r\n")
				return
			default:
				fmt.Fprint(conn, "250 OK\r\n")
			}
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	mt := &MailTarget{
		to:      []string{"ops@example.com"},
		subject: "TLS Test",
		smtpCfg: SMTPConfig{
			Host:       "127.0.0.1",
			Port:       port,
			From:       "lemmings@example.com",
			RequireTLS: true, // SEC-03: must refuse plaintext
		},
	}

	err = mt.Deliver(context.Background(), report.Rendered{
		Filename:     "test",
		Markdown:     "# test",
		HTMLForEmail: "<html>ok</html>",
	})
	if err == nil {
		t.Fatal("SEC-03 violation: MailTarget delivered plaintext despite RequireTLS=true")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "tls") {
		t.Errorf("expected TLS error message, got: %v", err)
	}
}

func TestApplyFlagSMTPOverrides_UsesSecretType(t *testing.T) {
	sec := secrets.New("override-smtp-cred-777")
	mt := &MailTarget{smtpCfg: SMTPConfig{Host: "localhost", Port: 587, From: "a@b.com"}}
	ApplyFlagSMTPOverrides(mt, swarm.Config{
		SMTPHost:       "smtp.example.com",
		SMTPPort:       465,
		SMTPUser:       "mailer",
		SMTPPass:       sec,
		SMTPFrom:       "ops@example.com",
		SMTPRequireTLS: true,
	})
	if mt.smtpCfg.Host != "smtp.example.com" || mt.smtpCfg.Port != 465 || !mt.smtpCfg.RequireTLS {
		t.Errorf("overrides not applied: %+v", mt.smtpCfg)
	}
	if mt.smtpCfg.Pass.Reveal() != "override-smtp-cred-777" {
		t.Error("secret override lost")
	}
}

func TestMailTarget_FailedSTARTTLSNeverDowngrades(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var sawPlainMailFrom atomic.Bool
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		fmt.Fprint(conn, "220 mock.local ESMTP\r\n")
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "EHLO"):
				fmt.Fprint(conn, "250-mock.local\r\n250 STARTTLS\r\n")
			case strings.HasPrefix(line, "STARTTLS"):
				fmt.Fprint(conn, "220 Ready to start TLS\r\n")
				// Write non-TLS garbage to force handshake failure.
				_, _ = conn.Write([]byte("NOT_A_TLS_SERVER_HELLO\r\n"))
				return
			case strings.HasPrefix(line, "MAIL FROM"):
				sawPlainMailFrom.Store(true)
			}
		}
	}()

	mt := &MailTarget{
		to:      []string{"ops@example.com"},
		subject: "No Downgrade",
		smtpCfg: SMTPConfig{
			Host:       "127.0.0.1",
			Port:       ln.Addr().(*net.TCPAddr).Port,
			From:       "lemmings@example.com",
			RequireTLS: false, // Even when RequireTLS=false, failed STARTTLS must NEVER downgrade
		},
	}

	if err := mt.Deliver(context.Background(), report.Rendered{Filename: "r", Markdown: "m", HTMLForEmail: "<p>h</p>"}); err == nil {
		t.Fatal("expected error after corrupted STARTTLS handshake")
	}
	if sawPlainMailFrom.Load() {
		t.Fatal("SEC-03 violation: client downgraded to plaintext MAIL FROM after STARTTLS failure")
	}
}

func TestMailTarget_DeliverHonoursContextTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-done // stall without sending SMTP banner
	}()

	mt := &MailTarget{
		to:      []string{"ops@example.com"},
		subject: "Timeout Test",
		smtpCfg: SMTPConfig{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, From: "lemmings@example.com"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := mt.Deliver(ctx, report.Rendered{Filename: "r", Markdown: "m", HTMLForEmail: "<p>h</p>"}); err == nil {
		t.Fatal("expected timeout error against stalled SMTP server")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Deliver ignored context deadline, took %s", time.Since(start))
	}
}

func TestS3Target_ParsingKeysAndMissingRegionHint(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "nonexistent-aws-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "nonexistent-aws-creds"))

	if _, err := NewS3Target("s3://", nil); err == nil {
		t.Error("expected error for empty bucket")
	}
	st, err := NewS3Target("s3://ops-bucket/prod/reports/", nil)
	if err != nil {
		t.Fatalf("NewS3Target: %v", err)
	}
	if got := st.objectKey("run.artifacts/abc.jpg"); got != "prod/reports/run.artifacts/abc.jpg" {
		t.Errorf("objectKey = %q, want prod/reports/run.artifacts/abc.jpg", got)
	}
	if err := st.Deliver(context.Background(), report.Rendered{Filename: "run", Markdown: "# ok"}); err == nil || !strings.Contains(err.Error(), "AWS_REGION") {
		t.Errorf("expected actionable AWS_REGION hint, got: %v", err)
	}
}

func TestLocalTarget_AtomicWritesAndNoStrayTempFiles(t *testing.T) {
	dir := t.TempDir()
	lt := NewLocalTarget(dir, nil)

	first := report.Rendered{
		Filename: "lemmings.2026.11.25.example.com",
		Domain:   "example.com",
		Markdown: "# v1",
		HTML:     "<html>v1</html>",
		JSON:     []byte(`{"rev":1}`),
		Artifacts: []report.ArtifactAsset{
			{Filename: "shot1.jpg", MIMEType: "image/jpeg", Data: []byte("img-v1")},
		},
	}
	if err := lt.Deliver(context.Background(), first); err != nil {
		t.Fatalf("first Deliver: %v", err)
	}

	// Second delivery atomically replaces existing files and artifacts.
	second := first
	second.Markdown = "# v2 atomic replacement"
	second.HTML = "<html>v2</html>"
	second.JSON = []byte(`{"rev":2}`)
	second.Artifacts = []report.ArtifactAsset{
		{Filename: "shot1.jpg", MIMEType: "image/jpeg", Data: []byte("img-v2-updated")},
	}
	if err := lt.Deliver(context.Background(), second); err != nil {
		t.Fatalf("second Deliver: %v", err)
	}

	dest := filepath.Join(dir, "lemmings", "example.com")
	gotMD, err := os.ReadFile(filepath.Join(dest, second.Filename+".md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotMD) != second.Markdown {
		t.Errorf("markdown = %q, want %q", gotMD, second.Markdown)
	}
	gotArt, err := os.ReadFile(filepath.Join(dest, second.Filename+".artifacts", "shot1.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotArt) != "img-v2-updated" {
		t.Errorf("artifact = %q, want img-v2-updated", gotArt)
	}

	// Confirm zero stray .tmp-* files exist in report dir or artifacts dir.
	for _, checkDir := range []string{dest, filepath.Join(dest, second.Filename+".artifacts")} {
		entries, err := os.ReadDir(checkDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".tmp-") {
				t.Errorf("stray temp file left in %s: %s", checkDir, e.Name())
			}
		}
	}

	// Reject ".", "..", empty, slashes, and control characters in LocalTarget.
	for _, bad := range []string{"", ".", "..", "../escape", "a/b", `a\b`, "bad\x00name", "bad\nname"} {
		if err := validateSafeComponent(bad); err == nil {
			t.Errorf("validateSafeComponent(%q) = nil, want error", bad)
		}
		r := first
		r.Filename = bad
		if err := lt.Deliver(context.Background(), r); err == nil {
			t.Errorf("LocalTarget accepted unsafe filename %q", bad)
		}
	}
}

func TestS3Target_RejectsUnsafeComponents(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-1")
	st, err := NewS3Target("s3://ops-bucket/reports", nil)
	if err != nil {
		t.Fatalf("NewS3Target: %v", err)
	}

	unsafe := []string{"", ".", "..", "../escape", "sub/file", `sub\file`, "nul\x00byte", "ctrl\nchar", "del\x7fchar"}
	for _, bad := range unsafe {
		if err := st.Deliver(context.Background(), report.Rendered{Filename: bad, Domain: "example.com", Markdown: "# x"}); err == nil || !strings.Contains(err.Error(), "invalid filename") {
			t.Errorf("S3Target filename %q: err = %v, want invalid filename", bad, err)
		}
		if bad != "" {
			if err := st.Deliver(context.Background(), report.Rendered{Filename: "valid-run", Domain: bad, Markdown: "# x"}); err == nil || !strings.Contains(err.Error(), "invalid domain") {
				t.Errorf("S3Target domain %q: err = %v, want invalid domain", bad, err)
			}
		}
		if err := st.Deliver(context.Background(), report.Rendered{
			Filename:  "valid-run",
			Domain:    "example.com",
			Markdown:  "# x",
			Artifacts: []report.ArtifactAsset{{Filename: bad, MIMEType: "image/jpeg", Data: []byte("x")}},
		}); err == nil || !strings.Contains(err.Error(), "invalid artifact name") {
			t.Errorf("S3Target artifact %q: err = %v, want invalid artifact name", bad, err)
		}
	}
}
