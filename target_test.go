package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── ParseTarget ───────────────────────────────────────────────────────────────

// TestParseTarget_LocalPath verifies that a bare filesystem path produces
// a LocalTarget.
func TestParseTarget_LocalPath(t *testing.T) {
	cases := []string{".", "/tmp/reports", "./output", "relative/path"}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			target, err := ParseTarget(tc)
			if err != nil {
				t.Fatalf("ParseTarget(%q) error: %v", tc, err)
			}
			if _, ok := target.(*LocalTarget); !ok {
				t.Errorf("expected *LocalTarget for %q, got %T", tc, target)
			}
		})
	}
}

// TestParseTarget_FileScheme verifies that a file:// URI produces a LocalTarget.
func TestParseTarget_FileScheme(t *testing.T) {
	target, err := ParseTarget("file:///tmp/reports")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := target.(*LocalTarget); !ok {
		t.Errorf("expected *LocalTarget, got %T", target)
	}
}

// TestParseTarget_S3Scheme verifies that an s3:// URI produces an S3Target.
func TestParseTarget_S3Scheme(t *testing.T) {
	target, err := ParseTarget("s3://my-bucket/lemmings/reports")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := target.(*S3Target); !ok {
		t.Errorf("expected *S3Target, got %T", target)
	}
}

// TestParseTarget_MailtoScheme verifies that a mailto: URI produces a MailTarget.
func TestParseTarget_MailtoScheme(t *testing.T) {
	target, err := ParseTarget("mailto:ops@example.com?subject=Lemmings%20Report")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := target.(*MailTarget); !ok {
		t.Errorf("expected *MailTarget, got %T", target)
	}
}

// TestParseTarget_EmptyURI verifies that an empty string returns an error.
func TestParseTarget_EmptyURI(t *testing.T) {
	_, err := ParseTarget("")
	if err == nil {
		t.Error("expected error for empty URI")
	}
}

// TestParseTarget_WhitespaceOnly verifies that a whitespace-only string
// returns an error.
func TestParseTarget_WhitespaceOnly(t *testing.T) {
	_, err := ParseTarget("   ")
	if err == nil {
		t.Error("expected error for whitespace-only URI")
	}
}

// ── LocalTarget ───────────────────────────────────────────────────────────────

// TestLocalTarget_Name verifies the human-readable name format.
func TestLocalTarget_Name(t *testing.T) {
	lt := &LocalTarget{basePath: "/tmp/reports"}
	if !strings.Contains(lt.Name(), "/tmp/reports") {
		t.Errorf("Name should contain base path, got %q", lt.Name())
	}
}

// TestLocalTarget_Deliver_CreatesFiles verifies that Deliver writes both
// .md and .html files to the expected directory.
func TestLocalTarget_Deliver_CreatesFiles(t *testing.T) {
	dir := t.TempDir()
	lt := &LocalTarget{basePath: dir}

	err := lt.Deliver(
		context.Background(),
		"lemmings.2026.04.14.example.com",
		"# markdown content",
		"<html><body>html content</body></html>",
	)
	if err != nil {
		t.Fatalf("Deliver error: %v", err)
	}

	expectedDir := filepath.Join(dir, "lemmings", "example.com")
	mdPath := filepath.Join(expectedDir, "lemmings.2026.04.14.example.com.md")
	htmlPath := filepath.Join(expectedDir, "lemmings.2026.04.14.example.com.html")

	if _, err := os.Stat(mdPath); os.IsNotExist(err) {
		t.Errorf("expected md file at %s", mdPath)
	}
	if _, err := os.Stat(htmlPath); os.IsNotExist(err) {
		t.Errorf("expected html file at %s", htmlPath)
	}
}

// TestLocalTarget_Deliver_FileContents verifies that the written files
// contain exactly the content passed to Deliver.
func TestLocalTarget_Deliver_FileContents(t *testing.T) {
	dir := t.TempDir()
	lt := &LocalTarget{basePath: dir}

	md := "# test markdown"
	html := "<html><body>test html</body></html>"

	if err := lt.Deliver(context.Background(), "lemmings.2026.04.14.localhost-8080", md, html); err != nil {
		t.Fatalf("Deliver error: %v", err)
	}

	destDir := filepath.Join(dir, "lemmings", "localhost-8080")
	mdBytes, err := os.ReadFile(filepath.Join(destDir, "lemmings.2026.04.14.localhost-8080.md"))
	if err != nil {
		t.Fatalf("read md file: %v", err)
	}
	if string(mdBytes) != md {
		t.Errorf("md content mismatch: expected %q, got %q", md, string(mdBytes))
	}

	htmlBytes, err := os.ReadFile(filepath.Join(destDir, "lemmings.2026.04.14.localhost-8080.html"))
	if err != nil {
		t.Fatalf("read html file: %v", err)
	}
	if string(htmlBytes) != html {
		t.Errorf("html content mismatch: expected %q, got %q", html, string(htmlBytes))
	}
}

// TestLocalTarget_Deliver_CreatesDirectoryTree verifies that Deliver
// creates the full directory tree if it does not already exist.
func TestLocalTarget_Deliver_CreatesDirectoryTree(t *testing.T) {
	base := t.TempDir()
	nested := filepath.Join(base, "deep", "nested", "path")
	lt := &LocalTarget{basePath: nested}

	err := lt.Deliver(context.Background(), "lemmings.2026.04.14.example.com", "md", "html")
	if err != nil {
		t.Fatalf("Deliver should create missing directories: %v", err)
	}
}

// TestLocalTarget_Deliver_OverwritesExisting verifies that Deliver
// overwrites existing files without error.
func TestLocalTarget_Deliver_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	lt := &LocalTarget{basePath: dir}

	filename := "lemmings.2026.04.14.example.com"
	if err := lt.Deliver(context.Background(), filename, "first", "first html"); err != nil {
		t.Fatalf("first Deliver error: %v", err)
	}
	if err := lt.Deliver(context.Background(), filename, "second", "second html"); err != nil {
		t.Fatalf("second Deliver error: %v", err)
	}

	destDir := filepath.Join(dir, "lemmings", "example.com")
	mdBytes, _ := os.ReadFile(filepath.Join(destDir, filename+".md"))
	if string(mdBytes) != "second" {
		t.Errorf("expected second write to overwrite first, got %q", string(mdBytes))
	}
}

// TestLocalTarget_ResolveDir_ExtractsDomain verifies that resolveDir
// correctly extracts the domain component from the filename.
func TestLocalTarget_ResolveDir_ExtractsDomain(t *testing.T) {
	cases := []struct {
		filename string
		domain   string
	}{
		{"lemmings.2026.04.14.example.com", "example.com"},
		{"lemmings.2026.04.14.localhost-8080", "localhost-8080"},
		{"lemmings.2026.04.14.my.long.domain.com", "my.long.domain.com"},
	}

	for _, tc := range cases {
		t.Run(tc.filename, func(t *testing.T) {
			lt := &LocalTarget{basePath: "/base"}
			dir, err := lt.resolveDir(tc.filename)
			if err != nil {
				t.Fatalf("resolveDir error: %v", err)
			}
			if !strings.HasSuffix(dir, tc.domain) {
				t.Errorf("expected dir to end with %q, got %q", tc.domain, dir)
			}
		})
	}
}

// ── S3Target ──────────────────────────────────────────────────────────────────

// TestNewS3Target_ParsesBucketAndPrefix verifies correct parsing of the
// bucket and prefix components from an s3:// URI.
func TestNewS3Target_ParsesBucketAndPrefix(t *testing.T) {
	cases := []struct {
		uri    string
		bucket string
		prefix string
	}{
		{"s3://my-bucket", "my-bucket", ""},
		{"s3://my-bucket/", "my-bucket", ""},
		{"s3://my-bucket/lemmings", "my-bucket", "lemmings"},
		{"s3://my-bucket/lemmings/reports", "my-bucket", "lemmings/reports"},
		{"s3://my-bucket/path/with/trailing/", "my-bucket", "path/with/trailing"},
	}

	for _, tc := range cases {
		t.Run(tc.uri, func(t *testing.T) {
			target, err := newS3Target(tc.uri)
			if err != nil {
				t.Fatalf("newS3Target error: %v", err)
			}
			if target.bucket != tc.bucket {
				t.Errorf("expected bucket %q, got %q", tc.bucket, target.bucket)
			}
			if target.prefix != tc.prefix {
				t.Errorf("expected prefix %q, got %q", tc.prefix, target.prefix)
			}
		})
	}
}

// TestNewS3Target_EmptyBucket verifies that an s3:// URI without a bucket
// name returns an error.
func TestNewS3Target_EmptyBucket(t *testing.T) {
	_, err := newS3Target("s3://")
	if err == nil {
		t.Error("expected error for empty bucket name")
	}
}

// TestS3Target_Name_WithPrefix verifies the Name format when a prefix
// is configured.
func TestS3Target_Name_WithPrefix(t *testing.T) {
	target := &S3Target{bucket: "my-bucket", prefix: "lemmings/reports"}
	name := target.Name()
	if !strings.Contains(name, "my-bucket") {
		t.Errorf("Name should contain bucket, got %q", name)
	}
	if !strings.Contains(name, "lemmings/reports") {
		t.Errorf("Name should contain prefix, got %q", name)
	}
}

// TestS3Target_Name_WithoutPrefix verifies the Name format when no prefix
// is configured.
func TestS3Target_Name_WithoutPrefix(t *testing.T) {
	target := &S3Target{bucket: "my-bucket", prefix: ""}
	name := target.Name()
	if !strings.Contains(name, "my-bucket") {
		t.Errorf("Name should contain bucket, got %q", name)
	}
}

// TestS3Target_ObjectKey_WithPrefix verifies that the object key includes
// the prefix and a separator.
func TestS3Target_ObjectKey_WithPrefix(t *testing.T) {
	target := &S3Target{bucket: "b", prefix: "lemmings"}
	key := target.objectKey("report.md")
	if key != "lemmings/report.md" {
		t.Errorf("expected 'lemmings/report.md', got %q", key)
	}
}

// TestS3Target_ObjectKey_WithoutPrefix verifies that the object key is
// just the filename when no prefix is configured.
func TestS3Target_ObjectKey_WithoutPrefix(t *testing.T) {
	target := &S3Target{bucket: "b", prefix: ""}
	key := target.objectKey("report.md")
	if key != "report.md" {
		t.Errorf("expected 'report.md', got %q", key)
	}
}

// TestS3Target_RegionFromEnv verifies that the AWS_DEFAULT_REGION
// environment variable is read at construction time.
func TestS3Target_RegionFromEnv(t *testing.T) {
	t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")
	target, err := newS3Target("s3://my-bucket")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target.region != "eu-west-1" {
		t.Errorf("expected region eu-west-1, got %q", target.region)
	}
}

// TestS3Target_RegionFallback verifies that AWS_REGION is used as a
// fallback when AWS_DEFAULT_REGION is not set.
func TestS3Target_RegionFallback(t *testing.T) {
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_REGION", "ap-southeast-1")
	target, err := newS3Target("s3://my-bucket")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target.region != "ap-southeast-1" {
		t.Errorf("expected region ap-southeast-1, got %q", target.region)
	}
}

// ── MailTarget ────────────────────────────────────────────────────────────────

// TestNewMailTarget_ParsesSingleRecipient verifies that a simple mailto:
// URI with one recipient is parsed correctly.
func TestNewMailTarget_ParsesSingleRecipient(t *testing.T) {
	target, err := newMailTarget("mailto:ops@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(target.to) != 1 {
		t.Fatalf("expected 1 to address, got %d", len(target.to))
	}
	if target.to[0] != "ops@example.com" {
		t.Errorf("expected ops@example.com, got %q", target.to[0])
	}
}

// TestNewMailTarget_ParsesSubject verifies that the subject query param
// is correctly URL-decoded and stored.
func TestNewMailTarget_ParsesSubject(t *testing.T) {
	target, err := newMailTarget("mailto:ops@example.com?subject=Lemmings%20Test%20Results%202026.04.14")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target.subject != "Lemmings Test Results 2026.04.14" {
		t.Errorf("expected decoded subject, got %q", target.subject)
	}
}

// TestNewMailTarget_DefaultSubject verifies that a missing subject query
// param produces a sensible default.
func TestNewMailTarget_DefaultSubject(t *testing.T) {
	target, err := newMailTarget("mailto:ops@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target.subject == "" {
		t.Error("subject should have a default value when not specified")
	}
	if !strings.Contains(target.subject, "Lemmings") {
		t.Errorf("default subject should contain 'Lemmings', got %q", target.subject)
	}
}

// TestNewMailTarget_ParsesCC verifies that the cc query param is parsed
// into the cc address list.
func TestNewMailTarget_ParsesCC(t *testing.T) {
	target, err := newMailTarget("mailto:ops@example.com?cc=team@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(target.cc) != 1 {
		t.Fatalf("expected 1 cc address, got %d", len(target.cc))
	}
	if target.cc[0] != "team@example.com" {
		t.Errorf("expected team@example.com, got %q", target.cc[0])
	}
}

// TestNewMailTarget_NoRecipient verifies that a mailto: URI with no
// address returns an error.
func TestNewMailTarget_NoRecipient(t *testing.T) {
	_, err := newMailTarget("mailto:")
	if err == nil {
		t.Error("expected error for mailto: with no recipient")
	}
}

// TestNewMailTarget_InvalidAddress verifies that a malformed email address
// returns an error.
func TestNewMailTarget_InvalidAddress(t *testing.T) {
	_, err := newMailTarget("mailto:not-an-email")
	if err == nil {
		t.Error("expected error for invalid email address")
	}
}

// TestMailTarget_Name verifies the Name format includes the to address.
func TestMailTarget_Name(t *testing.T) {
	target := &MailTarget{to: []string{"ops@example.com"}}
	name := target.Name()
	if !strings.Contains(name, "ops@example.com") {
		t.Errorf("Name should contain recipient, got %q", name)
	}
}

// TestMailTarget_BuildMessage_IsValidMIME verifies that buildMessage
// produces a non-empty byte slice containing MIME headers.
func TestMailTarget_BuildMessage_IsValidMIME(t *testing.T) {
	target := &MailTarget{
		to:      []string{"ops@example.com"},
		subject: "Test Report",
		smtpCfg: SMTPConfig{From: "lemmings@localhost"},
	}

	msg, err := target.buildMessage(
		"lemmings.2026.04.14.example.com",
		"# markdown",
		"<html><body>html</body></html>",
	)
	if err != nil {
		t.Fatalf("buildMessage error: %v", err)
	}
	if len(msg) == 0 {
		t.Error("buildMessage returned empty bytes")
	}

	content := string(msg)
	if !strings.Contains(content, "MIME-Version: 1.0") {
		t.Error("message should contain MIME-Version header")
	}
	if !strings.Contains(content, "Content-Type: multipart/mixed") {
		t.Error("message should be multipart/mixed")
	}
	if !strings.Contains(content, "ops@example.com") {
		t.Error("message should contain recipient address")
	}
	if !strings.Contains(content, "Test Report") {
		t.Error("message should contain subject")
	}
}

// TestMailTarget_BuildMessage_ContainsBothParts verifies that the MIME
// message contains both an HTML part and a plain text attachment.
func TestMailTarget_BuildMessage_ContainsBothParts(t *testing.T) {
	target := &MailTarget{
		to:      []string{"ops@example.com"},
		subject: "Test",
		smtpCfg: SMTPConfig{From: "lemmings@localhost"},
	}

	msg, err := target.buildMessage("filename", "markdown content", "<html>html</html>")
	if err != nil {
		t.Fatalf("buildMessage error: %v", err)
	}

	content := string(msg)
	if !strings.Contains(content, "text/html") {
		t.Error("message should contain text/html part")
	}
	if !strings.Contains(content, "text/plain") {
		t.Error("message should contain text/plain attachment")
	}
	if !strings.Contains(content, "filename.md") {
		t.Error("message should reference markdown filename as attachment")
	}
}

// TestMailTarget_BuildMessage_CCHeader verifies that the Cc header is
// included when cc addresses are configured.
func TestMailTarget_BuildMessage_CCHeader(t *testing.T) {
	target := &MailTarget{
		to:      []string{"ops@example.com"},
		cc:      []string{"team@example.com"},
		subject: "Test",
		smtpCfg: SMTPConfig{From: "lemmings@localhost"},
	}

	msg, err := target.buildMessage("filename", "md", "html")
	if err != nil {
		t.Fatalf("buildMessage error: %v", err)
	}

	content := string(msg)
	if !strings.Contains(content, "Cc: team@example.com") {
		t.Errorf("expected Cc header, got content starting with: %s",
			content[:min(200, len(content))])
	}
}

// TestMailTarget_BuildMessage_NoCCHeader verifies that the Cc header is
// omitted when no cc addresses are configured.
func TestMailTarget_BuildMessage_NoCCHeader(t *testing.T) {
	target := &MailTarget{
		to:      []string{"ops@example.com"},
		cc:      nil,
		subject: "Test",
		smtpCfg: SMTPConfig{From: "lemmings@localhost"},
	}

	msg, err := target.buildMessage("filename", "md", "html")
	if err != nil {
		t.Fatalf("buildMessage error: %v", err)
	}

	if strings.Contains(string(msg), "\r\nCc:") {
		t.Error("Cc header should not be present when no cc addresses configured")
	}
}

// TestMailTarget_Deliver_SMTPFailure verifies that Deliver returns a
// non-nil error when the SMTP server is unreachable.
func TestMailTarget_Deliver_SMTPFailure(t *testing.T) {
	target := &MailTarget{
		to:      []string{"ops@example.com"},
		subject: "Test",
		smtpCfg: SMTPConfig{
			Host: "localhost",
			Port: 1, // port 1 is reserved — will refuse connections
			From: "lemmings@localhost",
		},
	}

	err := target.Deliver(context.Background(), "filename", "md", "html")
	if err == nil {
		t.Error("expected error when SMTP server is unreachable")
	}
}

// TestMailTarget_Deliver_LocalSMTP verifies end-to-end email delivery
// against a minimal local SMTP server. Skipped if no SMTP port is
// available — this test is informational in CI, not a gate.
func TestMailTarget_Deliver_LocalSMTP(t *testing.T) {
	// Start a minimal SMTP server that accepts and discards messages
	listener, port, err := startTestSMTPServer(t)
	if err != nil {
		t.Skipf("could not start test SMTP server: %v", err)
	}
	defer listener.Close()

	received := make(chan []byte, 1)
	go acceptOneSMTPMessage(listener, received)

	target := &MailTarget{
		to:      []string{"ops@example.com"},
		subject: "Lemmings Test",
		smtpCfg: SMTPConfig{
			Host: "localhost",
			Port: port,
			From: "lemmings@localhost",
		},
	}

	err = target.Deliver(context.Background(),
		"lemmings.2026.04.14.example.com",
		"# markdown report",
		"<html><body>html report</body></html>",
	)
	if err != nil {
		t.Fatalf("Deliver error: %v", err)
	}

	select {
	case msg := <-received:
		content := string(msg)
		if !strings.Contains(content, "Lemmings Test") {
			t.Error("received message should contain subject")
		}
		if !strings.Contains(content, "markdown report") || !strings.Contains(content, "html report") {
			t.Error("received message should contain report content")
		}
	case <-time.After(3 * time.Second):
		t.Error("SMTP server did not receive message within 3 seconds")
	}
}

// ── smtpConfigFromEnv ─────────────────────────────────────────────────────────

// TestSMTPConfigFromEnv_Defaults verifies that unset environment variables
// produce sensible default values.
func TestSMTPConfigFromEnv_Defaults(t *testing.T) {
	t.Setenv("LEMMINGS_SMTP_HOST", "")
	t.Setenv("LEMMINGS_SMTP_PORT", "")
	t.Setenv("LEMMINGS_SMTP_USER", "")
	t.Setenv("LEMMINGS_SMTP_PASS", "")
	t.Setenv("LEMMINGS_SMTP_FROM", "")

	cfg := smtpConfigFromEnv()
	if cfg.Host != "localhost" {
		t.Errorf("expected default host 'localhost', got %q", cfg.Host)
	}
	if cfg.Port != 587 {
		t.Errorf("expected default port 587, got %d", cfg.Port)
	}
	if cfg.From != "lemmings@localhost" {
		t.Errorf("expected default from 'lemmings@localhost', got %q", cfg.From)
	}
}

// TestSMTPConfigFromEnv_ReadsEnvVars verifies that environment variables
// are read correctly when set.
func TestSMTPConfigFromEnv_ReadsEnvVars(t *testing.T) {
	t.Setenv("LEMMINGS_SMTP_HOST", "mail.example.com")
	t.Setenv("LEMMINGS_SMTP_PORT", "465")
	t.Setenv("LEMMINGS_SMTP_USER", "sender@example.com")
	t.Setenv("LEMMINGS_SMTP_PASS", "secret")
	t.Setenv("LEMMINGS_SMTP_FROM", "noreply@example.com")

	cfg := smtpConfigFromEnv()
	if cfg.Host != "mail.example.com" {
		t.Errorf("expected host 'mail.example.com', got %q", cfg.Host)
	}
	if cfg.Port != 465 {
		t.Errorf("expected port 465, got %d", cfg.Port)
	}
	if cfg.User != "sender@example.com" {
		t.Errorf("expected user 'sender@example.com', got %q", cfg.User)
	}
	if cfg.Pass != "secret" {
		t.Errorf("expected pass 'secret', got %q", cfg.Pass)
	}
	if cfg.From != "noreply@example.com" {
		t.Errorf("expected from 'noreply@example.com', got %q", cfg.From)
	}
}

// TestSMTPConfigFromEnv_InvalidPort verifies that a non-numeric port
// value in the environment variable falls back to the default.
func TestSMTPConfigFromEnv_InvalidPort(t *testing.T) {
	t.Setenv("LEMMINGS_SMTP_PORT", "notanumber")
	cfg := smtpConfigFromEnv()
	if cfg.Port != 587 {
		t.Errorf("invalid port should fall back to 587, got %d", cfg.Port)
	}
}

// ── parseInt ──────────────────────────────────────────────────────────────────

// TestParseInt_Valid verifies correct parsing of numeric strings.
func TestParseInt_Valid(t *testing.T) {
	cases := []struct {
		input    string
		expected int
	}{
		{"587", 587},
		{"465", 465},
		{"25", 25},
		{"0", 0},
	}
	for _, tc := range cases {
		got := parseInt(tc.input)
		if got != tc.expected {
			t.Errorf("parseInt(%q) = %d, want %d", tc.input, got, tc.expected)
		}
	}
}

// TestParseInt_Invalid verifies that non-numeric input returns 0.
func TestParseInt_Invalid(t *testing.T) {
	cases := []string{"", "abc", "587abc", "!@#"}
	for _, tc := range cases {
		got := parseInt(tc)
		if got != 0 {
			t.Errorf("parseInt(%q) should return 0 for invalid input, got %d", tc, got)
		}
	}
}

// ── Reporter.AddTarget / Write fan-out ────────────────────────────────────────

// TestReporter_AddTarget_AppendsTarget verifies that AddTarget appends
// the target to the reporter's internal list.
func TestReporter_AddTarget_AppendsTarget(t *testing.T) {
	r := NewReporter(testConfig())
	dir := t.TempDir()
	r.AddTarget(&LocalTarget{basePath: dir})
	if len(r.targets) != 1 {
		t.Errorf("expected 1 target, got %d", len(r.targets))
	}
}

// TestReporter_Write_DelivershToAllTargets verifies that Write calls
// Deliver on every registered target.
func TestReporter_Write_DeliversToAllTargets(t *testing.T) {
	r := NewReporter(testConfig())

	var mu sync.Mutex
	var deliveries []string

	for i := 0; i < 3; i++ {
		i := i
		r.AddTarget(&mockTarget{
			name: fmt.Sprintf("mock-%d", i),
			deliverFn: func(ctx context.Context, filename, md, html string) error {
				mu.Lock()
				deliveries = append(deliveries, fmt.Sprintf("mock-%d", i))
				mu.Unlock()
				return nil
			},
		})
	}

	if err := r.Write(context.Background()); err != nil {
		t.Fatalf("Write error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != 3 {
		t.Errorf("expected 3 deliveries, got %d: %v", len(deliveries), deliveries)
	}
}

// TestReporter_Write_ContinuesOnPartialFailure verifies that when one
// target fails, the others still receive the report.
func TestReporter_Write_ContinuesOnPartialFailure(t *testing.T) {
	r := NewReporter(testConfig())

	var successCount int
	var mu sync.Mutex

	r.AddTarget(&mockTarget{
		name:      "failing",
		deliverFn: func(_ context.Context, _, _, _ string) error {
			return fmt.Errorf("delivery failed")
		},
	})
	r.AddTarget(&mockTarget{
		name: "succeeding",
		deliverFn: func(_ context.Context, _, _, _ string) error {
			mu.Lock()
			successCount++
			mu.Unlock()
			return nil
		},
	})

	err := r.Write(context.Background())
	if err == nil {
		t.Error("expected error when at least one target fails")
	}

	mu.Lock()
	defer mu.Unlock()
	if successCount != 1 {
		t.Errorf("succeeding target should still receive report, got %d successes", successCount)
	}
}

// TestReporter_Write_NoTargets_WritesToStdout verifies that when no targets
// are registered, Write falls back to printing the markdown to stdout
// without error.
func TestReporter_Write_NoTargets_WritesToStdout(t *testing.T) {
	r := NewReporter(testConfig())
	// No targets registered
	err := r.Write(context.Background())
	if err != nil {
		t.Fatalf("Write with no targets should not error, got: %v", err)
	}
}

// TestReporter_Write_ContextCancellation verifies that cancelling the
// context causes in-progress deliveries to abort where possible.
func TestReporter_Write_ContextCancellation(t *testing.T) {
	r := NewReporter(testConfig())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	r.AddTarget(&mockTarget{
		name: "slow",
		deliverFn: func(ctx context.Context, _, _, _ string) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
				return nil
			}
		},
	})

	// Should return promptly due to cancelled context
	done := make(chan error, 1)
	go func() { done <- r.Write(ctx) }()

	select {
	case <-done:
		// correct — returned promptly
	case <-time.After(2 * time.Second):
		t.Fatal("Write did not return promptly after context cancellation")
	}
}

// TestReporter_BuildFilename_Format verifies the filename format includes
// the lemmings prefix, date, and domain.
func TestReporter_BuildFilename_Format(t *testing.T) {
	cfg := testConfig()
	cfg.Hit = "https://example.com/"
	r := NewReporter(cfg)

	filename := r.buildFilename()
	if !strings.HasPrefix(filename, "lemmings.") {
		t.Errorf("filename should start with 'lemmings.', got %q", filename)
	}
	if !strings.Contains(filename, "example.com") {
		t.Errorf("filename should contain domain, got %q", filename)
	}
}

// ── applyFlagSMTPOverrides ────────────────────────────────────────────────────

// TestApplyFlagSMTPOverrides_OverridesNonZero verifies that non-empty flag
// values replace the corresponding SMTP config fields.
func TestApplyFlagSMTPOverrides_OverridesNonZero(t *testing.T) {
	mt := &MailTarget{
		smtpCfg: SMTPConfig{
			Host: "env-host",
			Port: 587,
			From: "env@example.com",
		},
	}

	cfg := testConfig()
	cfg.SMTPHost = "flag-host"
	cfg.SMTPPort = 465
	cfg.SMTPFrom = "flag@example.com"

	applyFlagSMTPOverrides(mt, cfg)

	if mt.smtpCfg.Host != "flag-host" {
		t.Errorf("expected flag-host, got %q", mt.smtpCfg.Host)
	}
	if mt.smtpCfg.Port != 465 {
		t.Errorf("expected 465, got %d", mt.smtpCfg.Port)
	}
	if mt.smtpCfg.From != "flag@example.com" {
		t.Errorf("expected flag@example.com, got %q", mt.smtpCfg.From)
	}
}

// TestApplyFlagSMTPOverrides_DoesNotOverrideZero verifies that zero/empty
// flag values do not overwrite existing SMTP config values — env vars
// should remain active when flags are not explicitly set.
func TestApplyFlagSMTPOverrides_DoesNotOverrideZero(t *testing.T) {
	mt := &MailTarget{
		smtpCfg: SMTPConfig{
			Host: "env-host",
			Port: 587,
		},
	}

	cfg := testConfig()
	cfg.SMTPHost = "" // not set
	cfg.SMTPPort = 0  // not set

	applyFlagSMTPOverrides(mt, cfg)

	if mt.smtpCfg.Host != "env-host" {
		t.Errorf("env host should not be overridden by empty flag, got %q", mt.smtpCfg.Host)
	}
	if mt.smtpCfg.Port != 587 {
		t.Errorf("env port should not be overridden by zero flag, got %d", mt.smtpCfg.Port)
	}
}

// ── Benchmark ─────────────────────────────────────────────────────────────────

// BenchmarkLocalTarget_Deliver measures the time to write a realistic
// report to a temporary directory — establishes the baseline I/O cost
// of local delivery.
func BenchmarkLocalTarget_Deliver(b *testing.B) {
	dir := b.TempDir()
	lt := &LocalTarget{basePath: dir}
	md := strings.Repeat("# Report\n\nContent line.\n", 100)
	html := strings.Repeat("<p>Content</p>", 500)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		filename := fmt.Sprintf("lemmings.2026.04.14.bench-%d.com", i)
		lt.Deliver(context.Background(), filename, md, html)
	}
}

// BenchmarkMailTarget_BuildMessage measures MIME message construction
// time for a realistic report — this runs synchronously on the delivery
// goroutine and must complete quickly.
func BenchmarkMailTarget_BuildMessage(b *testing.B) {
	target := &MailTarget{
		to:      []string{"ops@example.com"},
		cc:      []string{"team@example.com"},
		subject: "Lemmings Benchmark Report",
		smtpCfg: SMTPConfig{From: "lemmings@localhost"},
	}
	md := strings.Repeat("# Report\n\nContent.\n", 100)
	html := strings.Repeat("<p>Content</p>", 500)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		target.buildMessage("lemmings.2026.04.14.example.com", md, html)
	}
}

// ── Internal test helpers ─────────────────────────────────────────────────────

// mockTarget is a ReportTarget implementation for unit tests that records
// deliveries and can be configured to return errors.
type mockTarget struct {
	name      string
	deliverFn func(ctx context.Context, filename, md, html string) error
}

func (m *mockTarget) Name() string { return m.name }
func (m *mockTarget) Deliver(ctx context.Context, filename, md, html string) error {
	if m.deliverFn != nil {
		return m.deliverFn(ctx, filename, md, html)
	}
	return nil
}

// startTestSMTPServer starts a minimal TCP listener that accepts one
// connection and records the transmitted bytes. Returns the listener,
// the port it bound to, and any error.
func startTestSMTPServer(t *testing.T) (net.Listener, int, error) {
	t.Helper()
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return nil, 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { l.Close() })
	return l, port, nil
}

// acceptOneSMTPMessage handles a single SMTP connection, reads the DATA
// payload, and sends it to the received channel. Implements the minimal
// SMTP server state machine required for net/smtp.
func acceptOneSMTPMessage(l net.Listener, received chan<- []byte) {
	conn, err := l.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	var buf []byte
	var inData bool
	tmp := make([]byte, 4096)

	// Send greeting
	conn.Write([]byte("220 localhost SMTP test server\r\n"))

	for {
		n, err := conn.Read(tmp)
		if err != nil {
			break
		}
		chunk := tmp[:n]
		line := strings.TrimSpace(string(chunk))

		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			conn.Write([]byte("250 OK\r\n"))
		case strings.HasPrefix(line, "MAIL FROM"):
			conn.Write([]byte("250 OK\r\n"))
		case strings.HasPrefix(line, "RCPT TO"):
			conn.Write([]byte("250 OK\r\n"))
		case strings.HasPrefix(line, "DATA"):
			inData = true
			conn.Write([]byte("354 Start input\r\n"))
		case inData && line == ".":
			conn.Write([]byte("250 OK\r\n"))
			received <- buf
			return
		case inData:
			buf = append(buf, chunk...)
		case strings.HasPrefix(line, "QUIT"):
			conn.Write([]byte("221 Bye\r\n"))
			return
		default:
			conn.Write([]byte("250 OK\r\n"))
		}
	}
}

// min returns the smaller of two ints. Used for safe string truncation
// in error messages.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
