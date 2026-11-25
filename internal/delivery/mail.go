package delivery

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/report"
)

const smtpTimeout = 30 * time.Second

// MailTarget delivers the HTML report inline (SMTP-size-budgeted) and Markdown
// as an attachment via hardened SMTP (TLS 1.2+, validated headers, secrets.Secret).
type MailTarget struct {
	to      []string
	cc      []string
	subject string
	smtpCfg SMTPConfig
}

// NewMailTarget parses an RFC 6068 mailto: URI.
func NewMailTarget(uri string) (*MailTarget, error) {
	parsed, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("mail target: parse URI %q: %w", uri, err)
	}
	if parsed.Opaque == "" && parsed.Path == "" {
		return nil, fmt.Errorf("mail target: no recipient address in %q", uri)
	}
	rawTo := parsed.Opaque
	if rawTo == "" {
		rawTo = parsed.Path
	}

	toAddrs, err := parseAddressList(rawTo, "to")
	if err != nil {
		return nil, err
	}
	if len(toAddrs) == 0 {
		return nil, fmt.Errorf("mail target: no valid recipient addresses in %q", uri)
	}

	q := parsed.Query()
	ccAddrs, err := parseAddressList(q.Get("cc"), "cc")
	if err != nil {
		return nil, err
	}

	subject := q.Get("subject")
	if subject == "" {
		subject = fmt.Sprintf("Lemmings Report — %s", time.Now().Format("2006-01-02"))
	}

	return &MailTarget{
		to:      toAddrs,
		cc:      ccAddrs,
		subject: subject,
		smtpCfg: SMTPConfigFromEnv(),
	}, nil
}

// SetSMTPConfig replaces SMTP settings (primarily for testing and explicit wiring).
func (t *MailTarget) SetSMTPConfig(cfg SMTPConfig) { t.smtpCfg = cfg }

// SMTPConfig returns the target's SMTP configuration (Pass remains redacted on format/log).
func (t *MailTarget) SMTPConfig() SMTPConfig { return t.smtpCfg }

// Name returns the human-readable recipient list.
func (t *MailTarget) Name() string {
	return fmt.Sprintf("mailto:%s", strings.Join(t.to, ","))
}

// Deliver builds the MIME message and sends it over SMTPS (465) or STARTTLS.
func (t *MailTarget) Deliver(ctx context.Context, r report.Rendered) error {
	htmlBody := r.HTMLForEmail
	if htmlBody == "" {
		htmlBody = r.HTML
	}
	msg, err := t.buildMessage(r.Filename, r.Markdown, htmlBody)
	if err != nil {
		return fmt.Errorf("mail target: build message: %w", err)
	}

	addr := net.JoinHostPort(t.smtpCfg.Host, strconv.Itoa(t.smtpCfg.Port))
	switch t.smtpCfg.Port {
	case 465:
		return t.sendTLS(ctx, addr, msg)
	default:
		return t.sendSTARTTLS(ctx, addr, msg)
	}
}

func (t *MailTarget) buildMessage(filename, md, html string) ([]byte, error) {
	// Validate From/To/Cc against CR/LF header injection (SEC-02).
	fromAddr, err := validateHeaderAddress(t.smtpCfg.From, "from")
	if err != nil {
		return nil, err
	}
	for _, a := range t.to {
		if _, err := validateHeaderAddress(a, "to"); err != nil {
			return nil, err
		}
	}
	for _, a := range t.cc {
		if _, err := validateHeaderAddress(a, "cc"); err != nil {
			return nil, err
		}
	}

	safeFilename := sanitizeAttachmentFilename(filename)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s\r\n", fromAddr)
	fmt.Fprintf(&buf, "To: %s\r\n", strings.Join(t.to, ", "))
	if len(t.cc) > 0 {
		fmt.Fprintf(&buf, "Cc: %s\r\n", strings.Join(t.cc, ", "))
	}
	fmt.Fprintf(&buf, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", stripCRLF(t.subject)))
	fmt.Fprintf(&buf, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&buf, "MIME-Version: 1.0\r\n")

	mw := multipart.NewWriter(&buf)
	fmt.Fprintf(&buf, "Content-Type: multipart/mixed; boundary=%s\r\n\r\n", mw.Boundary())

	// Part 1: inline HTML report.
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

	// Part 2: Markdown attachment with properly formatted MIME disposition (SEC-02).
	mdHeader := make(textproto.MIMEHeader)
	mdHeader.Set("Content-Type", "text/plain; charset=utf-8")
	mdHeader.Set("Content-Transfer-Encoding", "quoted-printable")
	mdHeader.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": safeFilename + ".md",
	}))

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

func dialSMTP(ctx context.Context, addr string) (net.Conn, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(smtpTimeout)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	_ = conn.SetDeadline(deadline)
	return conn, nil
}

func (t *MailTarget) sendTLS(ctx context.Context, addr string, msg []byte) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("split host/port: %w", err)
	}
	raw, err := dialSMTP(ctx, addr)
	if err != nil {
		return err
	}
	conn := tls.Client(raw, &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return fmt.Errorf("tls handshake %s: %w", addr, err)
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp new client: %w", err)
	}
	defer client.Close()
	return t.sendViaSMTPConn(client, host, msg)
}

func (t *MailTarget) sendSTARTTLS(ctx context.Context, addr string, msg []byte) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("split host/port: %w", err)
	}
	conn, err := dialSMTP(ctx, addr)
	if err != nil {
		return err
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp new client: %w", err)
	}
	defer client.Close()

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	} else if t.smtpCfg.RequireTLS {
		// Fail closed against MITM STARTTLS downgrade on non-loopback hosts (SEC-03).
		return errors.New("smtp server does not advertise STARTTLS and -smtp-require-tls is enabled")
	}
	return t.sendViaSMTPConn(client, host, msg)
}

func (t *MailTarget) sendViaSMTPConn(client *smtp.Client, host string, msg []byte) error {
	defer client.Quit()

	if t.smtpCfg.User != "" && !t.smtpCfg.Pass.IsZero() {
		// Single point of secret consumption (S6).
		auth := smtp.PlainAuth("", t.smtpCfg.User, t.smtpCfg.Pass.Reveal(), host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	if err := client.Mail(t.smtpCfg.From); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	recipients := make([]string, 0, len(t.to)+len(t.cc))
	recipients = append(recipients, t.to...)
	recipients = append(recipients, t.cc...)
	for _, addr := range recipients {
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

func parseAddressList(raw, field string) ([]string, error) {
	var out []string
	for _, addr := range strings.Split(raw, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		valid, err := validateHeaderAddress(addr, field)
		if err != nil {
			return nil, err
		}
		out = append(out, valid)
	}
	return out, nil
}

func validateHeaderAddress(raw, field string) (string, error) {
	if strings.ContainsAny(raw, "\r\n") {
		return "", fmt.Errorf("mail target: %s address contains line break", field)
	}
	parsed, err := mail.ParseAddress(raw)
	if err != nil {
		return "", fmt.Errorf("mail target: invalid %s address %q: %w", field, raw, err)
	}
	return parsed.Address, nil
}

func sanitizeAttachmentFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" {
		return "lemmings-report"
	}
	return out
}

func stripCRLF(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}
