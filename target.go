package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ReportTarget is the interface for anything that can receive a rendered
// lemmings report. Implementations receive the fully rendered markdown and
// HTML content and are responsible for delivering them to their destination.
//
// Usage:
//
//	target, err := ParseTarget("s3://my-bucket/lemmings")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	if err := target.Deliver(ctx, "lemmings.2026.04.14.example.com", md, html); err != nil {
//	    log.Printf("delivery failed: %v", err)
//	}
//
// All three built-in implementations — LocalTarget, S3Target, and
// MailTarget — are constructed via ParseTarget from a URI string.
type ReportTarget interface {
	// Deliver sends the rendered report to the target destination.
	// filename is the base name without extension — implementations
	// append .md and .html as appropriate.
	// Returns an error if delivery fails — partial delivery is not
	// considered success.
	Deliver(ctx context.Context, filename, md, html string) error

	// Name returns a human-readable description of this target for
	// use in log output and boot summary display.
	Name() string
}

// ParseTarget constructs the appropriate ReportTarget implementation from
// a URI string. The URI prefix determines which implementation is used:
//
//	s3://bucket/prefix      → S3Target
//	mailto:addr?subject=... → MailTarget
//	file:///path            → LocalTarget (explicit)
//	./path or /path or .    → LocalTarget (implicit)
//
// Returns an error if the URI is empty, malformed, or refers to an
// unsupported scheme.
//
// Warning: ParseTarget does not validate credentials or connectivity.
// A successfully constructed target may still fail at Deliver time if
// AWS credentials are absent or SMTP settings are misconfigured.
func ParseTarget(uri string) (ReportTarget, error) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return nil, fmt.Errorf("target URI must not be empty")
	}

	switch {
	case strings.HasPrefix(uri, "s3://"):
		return newS3Target(uri)
	case strings.HasPrefix(uri, "mailto:"):
		return newMailTarget(uri)
	case strings.HasPrefix(uri, "file://"):
		path := strings.TrimPrefix(uri, "file://")
		return &LocalTarget{basePath: path}, nil
	default:
		// Treat anything else as a local filesystem path
		return &LocalTarget{basePath: uri}, nil
	}
}

// ── LocalTarget ───────────────────────────────────────────────────────────────

// LocalTarget writes the lemmings report to a local filesystem path.
//
// The report is written to:
//
//	<basePath>/lemmings/<domain>/<filename>.md
//	<basePath>/lemmings/<domain>/<filename>.html
//
// The directory tree is created if it does not already exist.
//
// Warning: LocalTarget will overwrite existing files with the same name.
// Filenames include the date component so this only occurs if lemmings
// is run more than once on the same day against the same target.
type LocalTarget struct {
	basePath string
}

// Name returns the human-readable identifier for this target.
func (t *LocalTarget) Name() string {
	return fmt.Sprintf("local(%s)", t.basePath)
}

// Deliver writes the markdown and HTML report files to the local filesystem.
func (t *LocalTarget) Deliver(_ context.Context, filename, md, html string) error {
	dir, err := t.resolveDir(filename)
	if err != nil {
		return fmt.Errorf("local target resolve dir: %w", err)
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("local target mkdir %s: %w", dir, err)
	}

	mdPath := filepath.Join(dir, filename+".md")
	if err := os.WriteFile(mdPath, []byte(md), 0644); err != nil {
		return fmt.Errorf("local target write md: %w", err)
	}
	fmt.Printf("  report (md):   %s\n", mdPath)

	htmlPath := filepath.Join(dir, filename+".html")
	if err := os.WriteFile(htmlPath, []byte(html), 0644); err != nil {
		return fmt.Errorf("local target write html: %w", err)
	}
	fmt.Printf("  report (html): %s\n", htmlPath)

	return nil
}

// resolveDir computes the output directory from the base path and the
// domain embedded in the filename.
func (t *LocalTarget) resolveDir(filename string) (string, error) {
	// filename is "lemmings.YYYY.MM.DD.domain"
	// Skip "lemmings" + 3 date components = 4 dot-separated parts
	parts := strings.SplitN(filename, ".", 5)
	domain := "unknown"
	if len(parts) == 5 {
		domain = parts[4]
	}
	return filepath.Join(t.basePath, "lemmings", domain), nil
}

// ── S3Target ──────────────────────────────────────────────────────────────────

// S3Target uploads the lemmings report to an Amazon S3 bucket.
//
// The report is uploaded to:
//
//	s3://<bucket>/<prefix>/<filename>.md
//	s3://<bucket>/<prefix>/<filename>.html
//
// Credentials are read from the standard AWS credential chain in order:
//  1. Environment variables: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY,
//     AWS_SESSION_TOKEN
//  2. Shared credentials file: ~/.aws/credentials
//  3. IAM instance role (when running on EC2, ECS, or Lambda)
//
// The bucket region is read from the AWS_DEFAULT_REGION environment
// variable or the -s3-region flag. If neither is set, the SDK attempts
// to detect the region automatically.
//
// Warning: S3Target does not create the bucket if it does not exist.
// Ensure the bucket exists and the credentials have s3:PutObject
// permission before running lemmings with an S3 target.
type S3Target struct {
	bucket string
	prefix string
	region string
}

// newS3Target parses an s3:// URI and constructs an S3Target.
//
// URI format: s3://bucket/optional/prefix
// The bucket is the first path component. Everything after it is the prefix.
func newS3Target(uri string) (*S3Target, error) {
	// Strip s3:// and split into bucket + prefix
	path := strings.TrimPrefix(uri, "s3://")
	if path == "" {
		return nil, fmt.Errorf("s3 target: bucket name must not be empty in %q", uri)
	}

	parts := strings.SplitN(path, "/", 2)
	bucket := parts[0]
	prefix := ""
	if len(parts) == 2 {
		prefix = strings.TrimSuffix(parts[1], "/")
	}

	if bucket == "" {
		return nil, fmt.Errorf("s3 target: bucket name is empty in %q", uri)
	}

	region := os.Getenv("AWS_DEFAULT_REGION")
	if region == "" {
		region = os.Getenv("AWS_REGION")
	}

	return &S3Target{
		bucket: bucket,
		prefix: prefix,
		region: region,
	}, nil
}

// Name returns the human-readable identifier for this target.
func (t *S3Target) Name() string {
	if t.prefix != "" {
		return fmt.Sprintf("s3://%s/%s", t.bucket, t.prefix)
	}
	return fmt.Sprintf("s3://%s", t.bucket)
}

// Deliver uploads the markdown and HTML report files to S3.
//
// Both files are uploaded with the correct Content-Type headers so they
// render correctly when accessed directly via S3 URLs or CloudFront.
// Uploads run sequentially — if the markdown upload fails the HTML
// upload is not attempted.
func (t *S3Target) Deliver(ctx context.Context, filename, md, html string) error {
	cfg, err := t.loadAWSConfig(ctx)
	if err != nil {
		return fmt.Errorf("s3 target: load aws config: %w", err)
	}

	client := s3.NewFromConfig(cfg)

	// Upload markdown
	mdKey := t.objectKey(filename + ".md")
	if err := t.upload(ctx, client, mdKey, "text/markdown; charset=utf-8", []byte(md)); err != nil {
		return fmt.Errorf("s3 target: upload md: %w", err)
	}
	fmt.Printf("  report (md):   s3://%s/%s\n", t.bucket, mdKey)

	// Upload HTML
	htmlKey := t.objectKey(filename + ".html")
	if err := t.upload(ctx, client, htmlKey, "text/html; charset=utf-8", []byte(html)); err != nil {
		return fmt.Errorf("s3 target: upload html: %w", err)
	}
	fmt.Printf("  report (html): s3://%s/%s\n", t.bucket, htmlKey)

	return nil
}

// upload performs a single S3 PutObject call.
func (t *S3Target) upload(ctx context.Context, client *s3.Client, key, contentType string, body []byte) error {
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(t.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String(contentType),
	})
	return err
}

// objectKey builds the full S3 object key from the prefix and filename.
func (t *S3Target) objectKey(filename string) string {
	if t.prefix == "" {
		return filename
	}
	return t.prefix + "/" + filename
}

// loadAWSConfig loads the AWS configuration from the standard credential
// chain. Uses the target's region if set, otherwise relies on SDK detection.
func (t *S3Target) loadAWSConfig(ctx context.Context) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{}
	if t.region != "" {
		opts = append(opts, config.WithRegion(t.region))
	}
	return config.LoadDefaultConfig(ctx, opts...)
}

// ── MailTarget ────────────────────────────────────────────────────────────────

// MailTarget sends the lemmings report as a MIME email.
//
// The email contains two parts:
//   - text/html (inline): the complete self-contained HTML report, rendered
//     correctly in any modern email client
//   - text/plain (attachment): the markdown report for archival and
//     plain-text email clients
//
// SMTP configuration is read from environment variables:
//
//	LEMMINGS_SMTP_HOST  SMTP server hostname (default: localhost)
//	LEMMINGS_SMTP_PORT  SMTP server port (default: 587)
//	LEMMINGS_SMTP_USER  SMTP username (optional)
//	LEMMINGS_SMTP_PASS  SMTP password (optional)
//	LEMMINGS_SMTP_FROM  From address (default: lemmings@localhost)
//
// SMTP flags passed to lemmings override environment variables when both
// are present.
//
// TLS behaviour is auto-detected from the port:
//   - Port 465: implicit TLS (SMTPS)
//   - Port 587: STARTTLS (default)
//   - Port 25:  plain SMTP (no TLS)
//   - Any other port: STARTTLS attempted, falls back to plain
//
// Warning: MailTarget does not queue or retry. If the SMTP server is
// unavailable at report delivery time, the error is returned and logged
// but other targets (local, S3) are unaffected.
type MailTarget struct {
	to      []string
	cc      []string
	subject string
	smtpCfg SMTPConfig
}

// SMTPConfig holds the SMTP connection parameters for MailTarget.
//
// All fields are optional — zero values produce sensible defaults.
// Fields set via CLI flags take precedence over environment variables,
// which take precedence over built-in defaults.
type SMTPConfig struct {
	Host string
	Port int
	User string
	Pass string
	From string
}

// smtpConfigFromEnv reads SMTP configuration from environment variables.
// Returns a SMTPConfig with defaults for any unset variables.
func smtpConfigFromEnv() SMTPConfig {
	host := os.Getenv("LEMMINGS_SMTP_HOST")
	if host == "" {
		host = "localhost"
	}

	portStr := os.Getenv("LEMMINGS_SMTP_PORT")
	port := 587
	if portStr != "" {
		if p := parseInt(portStr); p > 0 {
			port = p
		}
	}

	from := os.Getenv("LEMMINGS_SMTP_FROM")
	if from == "" {
		from = "lemmings@localhost"
	}

	return SMTPConfig{
		Host: host,
		Port: port,
		User: os.Getenv("LEMMINGS_SMTP_USER"),
		Pass: os.Getenv("LEMMINGS_SMTP_PASS"),
		From: from,
	}
}

// newMailTarget parses a mailto: URI and constructs a MailTarget.
//
// URI format follows RFC 6068:
//
//	mailto:addr@example.com?subject=Lemmings%20Results&cc=other@example.com
//
// Multiple to addresses are separated by commas in the address part.
// The subject is URL-decoded. cc is optional.
//
// SMTP configuration is loaded from environment variables at construction
// time and can be overridden by fields on SMTPConfig after construction.
func newMailTarget(uri string) (*MailTarget, error) {
	// net/url can parse mailto: URIs
	parsed, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("mail target: parse URI %q: %w", uri, err)
	}

	if parsed.Opaque == "" && parsed.Path == "" {
		return nil, fmt.Errorf("mail target: no recipient address in %q", uri)
	}

	// The opaque part of mailto:addr?query is the address
	rawTo := parsed.Opaque
	if rawTo == "" {
		rawTo = parsed.Path
	}

	// Validate and split to addresses
	var toAddrs []string
	for _, addr := range strings.Split(rawTo, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		if _, err := mail.ParseAddress(addr); err != nil {
			return nil, fmt.Errorf("mail target: invalid to address %q: %w", addr, err)
		}
		toAddrs = append(toAddrs, addr)
	}
	if len(toAddrs) == 0 {
		return nil, fmt.Errorf("mail target: no valid recipient addresses in %q", uri)
	}

	q := parsed.Query()

	// Parse cc addresses
	var ccAddrs []string
	for _, addr := range strings.Split(q.Get("cc"), ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		if _, err := mail.ParseAddress(addr); err != nil {
			return nil, fmt.Errorf("mail target: invalid cc address %q: %w", addr, err)
		}
		ccAddrs = append(ccAddrs, addr)
	}

	subject := q.Get("subject")
	if subject == "" {
		subject = fmt.Sprintf("Lemmings Report — %s", time.Now().Format("2006-01-02"))
	}

	return &MailTarget{
		to:      toAddrs,
		cc:      ccAddrs,
		subject: subject,
		smtpCfg: smtpConfigFromEnv(),
	}, nil
}

// Name returns the human-readable identifier for this target.
func (t *MailTarget) Name() string {
	return fmt.Sprintf("mailto:%s", strings.Join(t.to, ","))
}

// Deliver sends the report as a multipart MIME email.
//
// The HTML body is sent inline so it renders in the email client.
// The markdown is attached as lemmings-report.md for archival.
//
// TLS mode is selected automatically from the configured port.
func (t *MailTarget) Deliver(_ context.Context, filename, md, html string) error {
	_, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	msg, err := t.buildMessage(filename, md, html)
	if err != nil {
		return fmt.Errorf("mail target: build message: %w", err)
	}

	addr := fmt.Sprintf("%s:%d", t.smtpCfg.Host, t.smtpCfg.Port)

	switch t.smtpCfg.Port {
	case 465:
		return t.sendTLS(addr, msg)
	case 25:
		return t.sendPlain(addr, msg)
	default:
		// 587 and all others: attempt STARTTLS, fall back to plain
		if err := t.sendSTARTTLS(addr, msg); err != nil {
			return t.sendPlain(addr, msg)
		}
		return nil
	}
}

// buildMessage constructs the raw MIME email bytes.
//
// The email is a multipart/mixed message containing:
//   - text/html part (inline): the full HTML report
//   - text/plain attachment: the markdown report
func (t *MailTarget) buildMessage(filename, md, html string) ([]byte, error) {
	var buf bytes.Buffer
	allRecipients := append(t.to, t.cc...)

	// Headers
	fmt.Fprintf(&buf, "From: %s\r\n", t.smtpCfg.From)
	fmt.Fprintf(&buf, "To: %s\r\n", strings.Join(t.to, ", "))
	if len(t.cc) > 0 {
		fmt.Fprintf(&buf, "Cc: %s\r\n", strings.Join(t.cc, ", "))
	}
	fmt.Fprintf(&buf, "Subject: %s\r\n", t.subject)
	fmt.Fprintf(&buf, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&buf, "MIME-Version: 1.0\r\n")
	_ = allRecipients

	// Multipart writer
	mw := multipart.NewWriter(&buf)
	fmt.Fprintf(&buf, "Content-Type: multipart/mixed; boundary=%s\r\n\r\n", mw.Boundary())

	// Part 1: HTML body (inline)
	htmlHeader := make(textproto.MIMEHeader)
	htmlHeader.Set("Content-Type", "text/html; charset=utf-8")
	htmlHeader.Set("Content-Transfer-Encoding", "quoted-printable")
	htmlHeader.Set("Content-Disposition", "inline")

	htmlPart, err := mw.CreatePart(htmlHeader)
	if err != nil {
		return nil, fmt.Errorf("create html part: %w", err)
	}
	qpWriter := quotedprintable.NewWriter(htmlPart)
	if _, err := qpWriter.Write([]byte(html)); err != nil {
		return nil, fmt.Errorf("write html part: %w", err)
	}
	if err := qpWriter.Close(); err != nil {
		return nil, fmt.Errorf("close html qp writer: %w", err)
	}

	// Part 2: Markdown attachment
	mdHeader := make(textproto.MIMEHeader)
	mdHeader.Set("Content-Type", "text/plain; charset=utf-8")
	mdHeader.Set("Content-Transfer-Encoding", "quoted-printable")
	mdHeader.Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s.md"`, filename))

	mdPart, err := mw.CreatePart(mdHeader)
	if err != nil {
		return nil, fmt.Errorf("create md part: %w", err)
	}
	mdWriter := quotedprintable.NewWriter(mdPart)
	if _, err := mdWriter.Write([]byte(md)); err != nil {
		return nil, fmt.Errorf("write md part: %w", err)
	}
	if err := mdWriter.Close(); err != nil {
		return nil, fmt.Errorf("close md qp writer: %w", err)
	}

	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close multipart writer: %w", err)
	}

	return buf.Bytes(), nil
}

// sendTLS sends the email using implicit TLS (port 465 / SMTPS).
func (t *MailTarget) sendTLS(addr string, msg []byte) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("split host/port: %w", err)
	}

	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host})
	if err != nil {
		return fmt.Errorf("tls dial %s: %w", addr, err)
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("smtp new client: %w", err)
	}
	return t.sendViaSMTPConn(client, msg)
}

// sendSTARTTLS sends the email using STARTTLS negotiation (port 587).
func (t *MailTarget) sendSTARTTLS(addr string, msg []byte) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("split host/port: %w", err)
	}

	client, err := smtp.Dial(addr)
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	defer client.Close()

	if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
		return fmt.Errorf("starttls: %w", err)
	}

	return t.sendViaSMTPConn(client, msg)
}

// sendPlain sends the email over an unencrypted SMTP connection (port 25).
//
// Warning: sendPlain transmits credentials in plaintext if SMTP auth is
// configured. Use only in trusted network environments.
func (t *MailTarget) sendPlain(addr string, msg []byte) error {
	_, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("split host/port: %w", err)
	}

	client, err := smtp.Dial(addr)
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	defer client.Close()

	return t.sendViaSMTPConn(client, msg)
}

// sendViaSMTPConn performs auth, sets envelope, and writes the message
// body to an already-connected smtp.Client. Used by all three TLS modes.
func (t *MailTarget) sendViaSMTPConn(client *smtp.Client, msg []byte) error {
	defer client.Quit()

	host, _, _ := net.SplitHostPort(
		fmt.Sprintf("%s:%d", t.smtpCfg.Host, t.smtpCfg.Port),
	)

	// Auth — only if credentials are configured
	if t.smtpCfg.User != "" && t.smtpCfg.Pass != "" {
		auth := smtp.PlainAuth("", t.smtpCfg.User, t.smtpCfg.Pass, host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	if err := client.Mail(t.smtpCfg.From); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}

	for _, addr := range append(t.to, t.cc...) {
		if err := client.Rcpt(addr); err != nil {
			return fmt.Errorf("smtp RCPT TO %s: %w", addr, err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp write body: %w", err)
	}
	return w.Close()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// parseInt parses a string to int, returning 0 on failure.
// Used for permissive port number parsing from environment variables.
func parseInt(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
