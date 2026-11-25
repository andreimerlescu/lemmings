// Package version reports the compiled Lemmings release version.
// Build pipelines may override Build via -ldflags "-X .../internal/version.Build=v2.0.0".
package version

import "strings"

// Default is the semantic release version when Build is not overridden at link time.
const Default = "2.0.0"

// Build can be overridden via -ldflags at build time. It must never hold secrets.
var Build = ""

// String returns the canonical v-prefixed version string (e.g. "v2.0.0").
func String() string {
	v := strings.TrimSpace(Build)
	if v == "" {
		v = Default
	}
	if !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

// Raw returns the semantic version without a leading "v" (e.g. "2.0.0").
func Raw() string {
	return strings.TrimPrefix(String(), "v")
}
