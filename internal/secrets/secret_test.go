package secrets

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSecret_RedactsInAllSinks(t *testing.T) {
	const sentinel = "SENTINEL_TEST_VALUE_9x8y7z6w"
	s := New(sentinel)

	if s.IsZero() {
		t.Fatal("expected non-zero secret")
	}
	if s.Reveal() != sentinel {
		t.Fatal("Reveal did not return original value")
	}

	// Formatting verbs.
	for _, verb := range []string{"%s", "%v", "%+v", "%#v", "%q"} {
		out := fmt.Sprintf(verb, s)
		if strings.Contains(out, sentinel) {
			t.Errorf("fmt.Sprintf(%q, s) leaked sentinel: %q", verb, out)
		}
	}

	// JSON marshaling (struct and bare).
	b, err := json.Marshal(struct {
		Pass Secret `json:"pass"`
	}{Pass: s})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(b), sentinel) {
		t.Errorf("json.Marshal leaked sentinel: %s", b)
	}

	// Text marshaling.
	tb, err := s.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	if strings.Contains(string(tb), sentinel) {
		t.Errorf("MarshalText leaked sentinel: %s", tb)
	}

	// slog JSON handler.
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	logger.Info("auth check", "secret", s, "struct", struct{ S Secret }{S: s})
	if strings.Contains(logBuf.String(), sentinel) {
		t.Errorf("slog leaked sentinel: %s", logBuf.String())
	}
}

func TestSecret_RejectsEmptyAndPlaceholders(t *testing.T) {
	bad := []string{
		"", "   ", "changeme", "CHANGE_ME", "password", "your-password",
		"placeholder", "todo", "xxx", "REPLACE_ME",
	}
	for _, v := range bad {
		if _, err := FromReader(strings.NewReader(v), "test"); err == nil {
			t.Errorf("FromReader(%q) should have failed closed", v)
		}
	}
}

func TestFromFile_PermissionsAndLoad(t *testing.T) {
	dir := t.TempDir()
	goodPath := filepath.Join(dir, "smtp.pass")
	if err := os.WriteFile(goodPath, []byte("super-real-smtp-cred-42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sec, err := FromFile(goodPath)
	if err != nil {
		t.Fatalf("FromFile(0600) failed: %v", err)
	}
	if sec.Reveal() != "super-real-smtp-cred-42" {
		t.Errorf("unexpected secret value: %q", sec.Reveal())
	}

	if runtime.GOOS != "windows" {
		badPath := filepath.Join(dir, "world.pass")
		if err := os.WriteFile(badPath, []byte("super-real-smtp-cred-42\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := FromFile(badPath); err == nil {
			t.Error("FromFile(0644) must reject world/group-readable secret file")
		}
	}
}

func TestResolveOptional_Precedence(t *testing.T) {
	dir := t.TempDir()
	filePass := filepath.Join(dir, "pass.txt")
	if err := os.WriteFile(filePass, []byte("from-flag-file-99\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TEST_SECRET_ENV", "from-env-77")
	t.Setenv("TEST_SECRET_ENV_FILE", "")

	// File takes precedence over env.
	s, ok, err := ResolveOptional(filePass, "TEST_SECRET_ENV", false, nil)
	if err != nil || !ok || s.Reveal() != "from-flag-file-99" {
		t.Fatalf("file precedence failed: ok=%v err=%v", ok, err)
	}

	// Env fallback when no file.
	s, ok, err = ResolveOptional("", "TEST_SECRET_ENV", false, nil)
	if err != nil || !ok || s.Reveal() != "from-env-77" {
		t.Fatalf("env resolution failed: ok=%v err=%v", ok, err)
	}

	// Stdin works when requested.
	s, ok, err = ResolveOptional("", "UNSET_ENV_KEY_XYZ", true, strings.NewReader("from-stdin-88\n"))
	if err != nil || !ok || s.Reveal() != "from-stdin-88" {
		t.Fatalf("stdin resolution failed: ok=%v err=%v", ok, err)
	}

	// Nothing configured returns zero Secret, ok=false, err=nil.
	s, ok, err = ResolveOptional("", "UNSET_ENV_KEY_XYZ", false, nil)
	if err != nil || ok || !s.IsZero() {
		t.Fatalf("empty resolution failed: ok=%v err=%v zero=%v", ok, err, s.IsZero())
	}
}

func TestScrubText_RedactsSensitiveTokens(t *testing.T) {
	fakeJWT := "eyJ" + "hbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." + "eyJ" + "zdWIiOiIxMjM0NTY3ODkwIn0.abc123def456"
	fakeAWS := "AKIA" + strings.Repeat("A", 16)
	fakeAWSSecret := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	fakeGH := "ghp_" + strings.Repeat("1", 36)
	fakeGL := "glpat-" + strings.Repeat("a", 22)
	fakeGoogle := "AIza" + strings.Repeat("B", 35)
	fakePass := "synthetic_db_val_123"
	fakeCookie := "synthetic_cookie_999"
	fakeURLPass := "synthetic_url_val"
	fakeOpaqueToken := "opaque_tok_987654"
	fakeOpaqueJWT := "opaque_jwt_value_42"

	cases := []struct {
		name   string
		input  string
		secret string
	}{
		{"bearer", "Authorization: Bearer " + fakeJWT, "eyJ"},
		{"aws_key", "error connecting with " + fakeAWS + " to bucket", fakeAWS},
		{"github_pat", "token=" + fakeGH, fakeGH},
		{"gitlab_pat", "gitlab error using " + fakeGL + " on clone", fakeGL},
		{"google_api_key", "maps init failed key " + fakeGoogle + " denied", fakeGoogle},
		{"kv_password", `{"password":"` + fakePass + `"}`, fakePass},
		{"kv_token", `{"token":"` + fakeOpaqueToken + `"}`, fakeOpaqueToken},
		{"kv_jwt", `jwt=` + fakeOpaqueJWT + `&next=/home`, fakeOpaqueJWT},
		{"kv_aws_secret", `{"aws_secret_access_key":"` + fakeAWSSecret + `"}`, fakeAWSSecret},
		{"kv_db_pass", `db_pass=` + fakePass + ` host=db.internal`, fakePass},
		{"kv_smtp_pass", `{"smtp_pass":"` + fakePass + `"}`, fakePass},
		{"cookie_hdr", "Cookie: session=" + fakeCookie + "; theme=dark", fakeCookie},
		{"url_creds", "https://" + "alice:" + fakeURLPass + "@internal.example.com/api", fakeURLPass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScrubText(tc.input)
			if strings.Contains(got, tc.secret) {
				t.Errorf("ScrubText(%q) failed to redact %q -> %q", tc.input, tc.secret, got)
			}
			if !strings.Contains(got, "REDACTED") {
				t.Errorf("ScrubText(%q) expected REDACTED marker -> %q", tc.input, got)
			}
		})
	}

	// Ensure benign pass-related fields are not falsely redacted.
	benign := `{"plan_pass_rate":"99.5%","bypass":"true","tests_passed":"1000"}`
	if got := ScrubText(benign); got != benign {
		t.Errorf("ScrubText falsely modified benign input:\n got: %s\nwant: %s", got, benign)
	}
}

func TestFingerprint_HMAC(t *testing.T) {
	k1, err := NewEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	k2, err := NewEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	fp1 := Fingerprint("session=xyz", k1)
	fp1Again := Fingerprint("session=xyz", k1)
	fp2 := Fingerprint("session=xyz", k2)

	if fp1 == "" || fp1 != fp1Again {
		t.Errorf("expected deterministic fingerprint for same key, got %q vs %q", fp1, fp1Again)
	}
	if fp1 == fp2 {
		t.Error("expected distinct fingerprints across distinct ephemeral keys")
	}
	if strings.Contains(fp1, "xyz") {
		t.Error("fingerprint must not contain raw value")
	}
}

func FuzzScrubText(f *testing.F) {
	f.Add("Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJ1IjoxfQ.sig")
	f.Add("password=hunter2&api_key=sk-1234567890abcdef123456")
	f.Add("plain benign error message with no secrets")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		_ = ScrubText(s)
	})
}
