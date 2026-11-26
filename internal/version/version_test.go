package version

import "testing"

func TestStringAndRaw(t *testing.T) {
	orig := Build
	t.Cleanup(func() { Build = orig })

	Build = ""
	if got := String(); got != "v"+Default {
		t.Errorf("String() = %q, want %q", got, "v"+Default)
	}
	if got := Raw(); got != Default {
		t.Errorf("Raw() = %q, want %q", got, Default)
	}

	Build = "2.1.0-rc1"
	if got := String(); got != "v2.1.0-rc1" {
		t.Errorf("String() = %q, want v2.1.0-rc1", got)
	}
	if got := Raw(); got != "2.1.0-rc1" {
		t.Errorf("Raw() = %q, want 2.1.0-rc1", got)
	}

	Build = "  v3.0.0 \n"
	if got := String(); got != "v3.0.0" {
		t.Errorf("String() = %q, want v3.0.0", got)
	}
	if got := Raw(); got != "3.0.0" {
		t.Errorf("Raw() = %q, want 3.0.0", got)
	}
}
