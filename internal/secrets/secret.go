// Package secrets holds credential values so they cannot be printed,
// logged, or marshaled by accident, and provides scrubbing helpers for
// untrusted error bodies and browser console output.
package secrets

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Placeholder is what every formatting, serialization, and logging path prints
// in place of a Secret's raw value.
const Placeholder = "[REDACTED]"

// Secret is a credential value object. Its zero value is empty and reports IsZero.
// Secret is a value type; copies share no mutable state. Every standard
// formatting, JSON, text, and slog path prints Placeholder. Call Reveal at the
// single point where the raw credential is consumed.
type Secret struct {
	v string
}

// New wraps raw in a Secret. Prefer FromFile, FromEnv, or FromReader at entry points.
func New(raw string) Secret { return Secret{v: raw} }

// Reveal returns the raw credential value. Call only at the single sink where it is required.
func (s Secret) Reveal() string { return s.v }

// IsZero reports whether the secret is unset.
func (s Secret) IsZero() bool { return s.v == "" }

// String returns Placeholder.
func (Secret) String() string { return Placeholder }

// GoString returns a redacted Go-syntax representation.
func (Secret) GoString() string { return "secrets.Secret{" + Placeholder + "}" }

// Format catches every fmt verb (%v, %+v, %#v, %s, %q, %x, ...) so raw values never escape.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, Placeholder) }

// MarshalText encodes Placeholder.
func (Secret) MarshalText() ([]byte, error) { return []byte(Placeholder), nil }

// MarshalJSON encodes Placeholder as a JSON string.
func (Secret) MarshalJSON() ([]byte, error) { return json.Marshal(Placeholder) }

// LogValue ensures log/slog always emits Placeholder.
func (Secret) LogValue() slog.Value { return slog.StringValue(Placeholder) }

var (
	_ fmt.Stringer           = Secret{}
	_ fmt.GoStringer         = Secret{}
	_ fmt.Formatter          = Secret{}
	_ json.Marshaler         = Secret{}
	_ encoding.TextMarshaler = Secret{}
	_ slog.LogValuer         = Secret{}
)

// Sentinel errors returned by secret loaders. None ever embed secret values.
var (
	ErrNotSet      = errors.New("secret not set")
	ErrEmpty       = errors.New("secret is empty")
	ErrPlaceholder = errors.New("secret looks like a placeholder")
	ErrTooLarge    = errors.New("secret source too large")
	ErrPermissions = errors.New("secret file permissions too open (require 0600 or stricter; run chmod 600)")
	ErrNotRegular  = errors.New("secret source is not a regular file")
)

const maxSecretBytes = 64 << 10

// FromFile reads a credential from path. It opens first and stats the open handle
// (avoiding stat/open TOCTOU), requires a regular file, enforces mode 0600 or
// stricter on Unix-like systems, bounds the read, trims surrounding whitespace,
// and rejects empty or placeholder values.
func FromFile(path string) (Secret, error) {
	if path == "" {
		return Secret{}, ErrNotSet
	}
	f, err := os.Open(path)
	if err != nil {
		return Secret{}, fmt.Errorf("open secret file %q: %w", path, err)
	}
	defer f.Close() // read-only handle

	fi, err := f.Stat()
	if err != nil {
		return Secret{}, fmt.Errorf("stat secret file %q: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return Secret{}, fmt.Errorf("secret file %q: %w", path, ErrNotRegular)
	}
	if fi.Mode().Perm()&0o077 != 0 && !permissionsNotEnforced() {
		return Secret{}, fmt.Errorf("secret file %q has mode %04o: %w", path, fi.Mode().Perm(), ErrPermissions)
	}
	return FromReader(f, path)
}

// FromReader reads a bounded credential from r (for example stdin or an open file).
func FromReader(r io.Reader, source string) (Secret, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxSecretBytes+1))
	if err != nil {
		return Secret{}, fmt.Errorf("read secret from %s: %w", source, err)
	}
	if len(b) > maxSecretBytes {
		return Secret{}, fmt.Errorf("secret from %s: %w", source, ErrTooLarge)
	}
	return validate(strings.TrimSpace(string(b)), source)
}

// FromEnv reads a credential from environment variable name.
func FromEnv(name string) (Secret, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return Secret{}, fmt.Errorf("environment variable %s: %w", name, ErrNotSet)
	}
	return validate(strings.TrimSpace(raw), "$"+name)
}

// ResolveOptional loads an optional credential using explicit precedence:
// 1. filePath (from CLI flag like -smtp-pass-file)
// 2. stdin (when useStdin is true)
// 3. <envName>_FILE environment variable
// 4. <envName> environment variable
//
// If none of the sources is configured, it returns (Secret{}, false, nil).
// If a source is configured but invalid (empty, world-readable, placeholder, unreadable),
// it fails closed and returns an error naming the source, never the value.
func ResolveOptional(filePath, envName string, useStdin bool, stdin io.Reader) (Secret, bool, error) {
	if filePath != "" {
		s, err := FromFile(filePath)
		return s, err == nil, err
	}
	if useStdin {
		if stdin == nil {
			return Secret{}, false, errors.New("stdin reader is nil")
		}
		s, err := FromReader(stdin, "stdin")
		return s, err == nil, err
	}
	if envName != "" {
		if p := os.Getenv(envName + "_FILE"); p != "" {
			s, err := FromFile(p)
			return s, err == nil, err
		}
		if raw, ok := os.LookupEnv(envName); ok && raw != "" {
			s, err := validate(strings.TrimSpace(raw), "$"+envName)
			return s, err == nil, err
		}
	}
	return Secret{}, false, nil
}

// NewEphemeralKey generates a 32-byte cryptographically random key for per-run
// HMAC fingerprinting and session signing.
func NewEphemeralKey() ([]byte, error) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("generate ephemeral key: %w", err)
	}
	return k, nil
}

// Fingerprint returns a short, non-reversible 12-hex-character HMAC-SHA256 tag
// of raw under key. Useful for tracking whether a cookie or token changed across
// requests without logging the value or an unsalted hash.
func Fingerprint(raw string, key []byte) string {
	if raw == "" {
		return ""
	}
	if len(key) > 0 {
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(raw))
		return hex.EncodeToString(mac.Sum(nil))[:12]
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:12]
}

func validate(raw, src string) (Secret, error) {
	if raw == "" {
		return Secret{}, fmt.Errorf("secret from %s: %w", src, ErrEmpty)
	}
	if looksLikePlaceholder(raw) {
		return Secret{}, fmt.Errorf("secret from %s: %w", src, ErrPlaceholder)
	}
	return Secret{v: raw}, nil
}

func looksLikePlaceholder(s string) bool {
	switch strings.ToLower(s) {
	case "changeme", "change-me", "change_me", "replace-me", "replace_me",
		"placeholder", "password", "your-api-key", "your_api_key",
		"your-password", "your_password", "xxx", "todo", "tbd", "none", "null",
		"<redacted>", "[redacted]":
		return true
	}
	return false
}
