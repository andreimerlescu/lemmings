// Package delivery implements report delivery adapters for Local filesystems
// (confined with os.Root), AWS S3, and hardened SMTP email.
package delivery

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/andreimerlescu/lemmings/v2/internal/report"
	"github.com/andreimerlescu/lemmings/v2/internal/secrets"
	"github.com/andreimerlescu/lemmings/v2/internal/swarm"
)

// DeliveredFunc receives notifications when a file or object is delivered.
type DeliveredFunc func(label, location string)

// ParseTarget constructs a report.Target from a URI string:
//
//	s3://bucket/prefix      -> S3Target
//	mailto:addr?subject=... -> MailTarget
//	file:///path            -> LocalTarget
//	./path or /path or .    -> LocalTarget
func ParseTarget(uri string, onDelivered DeliveredFunc) (report.Target, error) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return nil, fmt.Errorf("target URI must not be empty")
	}
	switch {
	case strings.HasPrefix(uri, "s3://"):
		return NewS3Target(uri, onDelivered)
	case strings.HasPrefix(uri, "mailto:"):
		return NewMailTarget(uri)
	case strings.HasPrefix(uri, "file://"):
		return NewLocalTarget(strings.TrimPrefix(uri, "file://"), onDelivered), nil
	default:
		return NewLocalTarget(uri, onDelivered), nil
	}
}

// SaveTargets merges -save-to flag targets with the comma-separated LEMMINGS_SAVE_TO
// environment variable, removing duplicates while preserving first-seen order.
func SaveTargets(flag []string, env string) []string {
	all := append([]string(nil), flag...)
	for _, s := range strings.Split(env, ",") {
		all = append(all, s)
	}
	seen := make(map[string]bool)
	var out []string
	for _, s := range all {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// SMTPConfig holds SMTP parameters. Pass is stored as secrets.Secret (S6).
type SMTPConfig struct {
	Host       string
	Port       int
	User       string
	Pass       secrets.Secret
	From       string
	RequireTLS bool
}

// SMTPConfigFromEnv loads SMTP defaults from LEMMINGS_SMTP_* environment variables.
func SMTPConfigFromEnv() SMTPConfig {
	host := strings.TrimSpace(os.Getenv("LEMMINGS_SMTP_HOST"))
	if host == "" {
		host = "localhost"
	}
	port := 587
	if portStr := os.Getenv("LEMMINGS_SMTP_PORT"); portStr != "" {
		if p := parseInt(portStr); p > 0 {
			port = p
		}
	}
	from := strings.TrimSpace(os.Getenv("LEMMINGS_SMTP_FROM"))
	if from == "" {
		from = "lemmings@localhost"
	}
	var pass secrets.Secret
	if p, ok, err := secrets.ResolveOptional("", "LEMMINGS_SMTP_PASS", false, nil); err == nil && ok {
		pass = p
	}
	return SMTPConfig{
		Host:       host,
		Port:       port,
		User:       strings.TrimSpace(os.Getenv("LEMMINGS_SMTP_USER")),
		Pass:       pass,
		From:       from,
		RequireTLS: !isLoopbackHost(host),
	}
}

// ApplyFlagSMTPOverrides applies non-zero CLI overrides from swarm.Config onto mt.
func ApplyFlagSMTPOverrides(mt *MailTarget, cfg swarm.Config) {
	if cfg.SMTPHost != "" {
		mt.smtpCfg.Host = cfg.SMTPHost
		mt.smtpCfg.RequireTLS = !isLoopbackHost(cfg.SMTPHost)
	}
	if cfg.SMTPPort != 0 {
		mt.smtpCfg.Port = cfg.SMTPPort
	}
	if cfg.SMTPUser != "" {
		mt.smtpCfg.User = cfg.SMTPUser
	}
	if !cfg.SMTPPass.IsZero() {
		mt.smtpCfg.Pass = cfg.SMTPPass
	}
	if cfg.SMTPFrom != "" {
		mt.smtpCfg.From = cfg.SMTPFrom
	}
	if cfg.SMTPRequireTLS {
		mt.smtpCfg.RequireTLS = true
	}
}

func parseInt(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}
