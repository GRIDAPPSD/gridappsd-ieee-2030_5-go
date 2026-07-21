package buildinfo

import "testing"

// TestVersionDefaultNonEmpty asserts the compiled-in default is a
// non-empty, recognizable dev sentinel rather than the zero value.
// The zero value ("") would print as a blank -version response,
// which is indistinguishable from a broken build; the dev sentinel
// gives an operator a value that unambiguously means "no ldflags
// stamp was applied".
func TestVersionDefaultNonEmpty(t *testing.T) {
	t.Parallel()

	if Version == "" {
		t.Fatal("Version: got empty string, want a non-empty dev sentinel")
	}
	const want = "0.0.0-dev"
	if Version != want {
		t.Errorf("Version: got %q, want %q (compiled-in default; this test runs with no -ldflags stamp)", Version, want)
	}
}
