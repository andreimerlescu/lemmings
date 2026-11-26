package secrets

import "regexp"

var scrubPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// Authorization / Cookie / API-key headers appearing in error dumps or traces.
	{
		re:   regexp.MustCompile(`(?im)^(\s*(?:authorization|proxy-authorization|cookie|set-cookie|x-api-key|x-auth-token|x-amz-security-token|x-csrf-token)\s*:\s*).+$`),
		repl: `${1}` + Placeholder,
	},
	// Bearer / Basic tokens inline.
	{
		re:   regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9._~+/=-]{8,}`),
		repl: `${1} ` + Placeholder,
	},
	// JWTs (three base64url segments starting with eyJ).
	{
		re:   regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
		repl: Placeholder,
	},
	// Cloud / SaaS credential prefixes.
	{
		re:   regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
		repl: Placeholder,
	},
	{
		re:   regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b`),
		repl: Placeholder,
	},
	{
		re:   regexp.MustCompile(`\b(?:glpat-[A-Za-z0-9_-]{20,}|AIza[0-9A-Za-z_-]{20,})\b`),
		repl: Placeholder,
	},
	{
		re:   regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{20,}|xox[baprs]-[A-Za-z0-9-]{16,}|ops_[A-Za-z0-9._-]{20,})\b`),
		repl: Placeholder,
	},
	// Private key PEM headers/blocks.
	{
		re:   regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
		repl: Placeholder,
	},
	// URLs containing embedded userinfo credentials.
	{
		re:   regexp.MustCompile(`(https?://)([^/\s:@]+):([^/\s@]+)@`),
		repl: `${1}` + Placeholder + `@`,
	},
	// Key=value and JSON "key":"value" forms for common secret names.
	{
		re:   regexp.MustCompile(`(?i)("?(?:password|passwd|secret|api[_-]?key|access[_-]?token|refresh[_-]?token|auth[_-]?token|session[_-]?id|client[_-]?secret|private[_-]?key|aws[_-]?secret[_-]?access[_-]?key|token|jwt|[a-z0-9_-]+[_-]pass)"?\s*[:=]\s*"?)([^\s"',;&}\]]{4,})("?)`),
		repl: `${1}` + Placeholder + `${3}`,
	},
}

// ScrubText performs best-effort regex scrubbing of known credential shapes,
// auth headers, JWTs, private keys, credentialed URLs, and common secret
// key-value assignments from untrusted text (such as captured error page bodies
// or browser console output). Because it is pattern-based, it reduces leaks but
// cannot guarantee catching arbitrary or custom secret formats.
func ScrubText(s string) string {
	if s == "" {
		return ""
	}
	out := s
	for _, p := range scrubPatterns {
		out = p.re.ReplaceAllString(out, p.repl)
	}
	return out
}
