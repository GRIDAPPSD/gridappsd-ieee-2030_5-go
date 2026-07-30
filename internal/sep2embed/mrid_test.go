package sep2embed

import (
	"strings"
	"testing"
)

// isMRIDLegal reports whether s is a legal IEEE 2030.5 mRIDType value:
// an even-length run of at most 32 hexadecimal digits. This mirrors the
// two checks a schema-driven client applies, and both matter: the EPRI
// reference client's parse_hex walks the string two characters at a time
// and fails on a non-hex digit or an odd count, and its XS_HEX_BINARY
// length bound rejects anything longer than 16 octets.
func isMRIDLegal(s string) bool {
	if s == "" || len(s) > mridHexChars || len(s)%2 != 0 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

func TestDeriveResourceMRIDIsWireLegal(t *testing.T) {
	t.Parallel()

	const lfdi = "AAAA00000000000000000000000000000000AAAA"
	for _, kind := range []string{"fsa/1", "derp/1", "dderc", "derc/active"} {
		got := deriveResourceMRID(lfdi, kind)
		if len(got) != mridHexChars {
			t.Errorf("deriveResourceMRID(%q) = %q, length %d, want exactly %d hex characters",
				kind, got, len(got), mridHexChars)
		}
		if !isMRIDLegal(got) {
			t.Errorf("deriveResourceMRID(%q) = %q, which no schema-driven client will parse", kind, got)
		}
	}
}

// TestDeriveResourceMRIDRejectsTheOldComposedShape is a guard, not a
// tautology: it asserts that the shape this bridge used to emit
// (lfdi + "-" + kind) really is illegal, so the helper's reason for
// existing stays documented in an executable form. If someone later
// "simplifies" the helper back to string concatenation, this fails.
func TestDeriveResourceMRIDRejectsTheOldComposedShape(t *testing.T) {
	t.Parallel()

	const lfdi = "AAAA00000000000000000000000000000000AAAA"
	old := lfdi + "-active"
	if isMRIDLegal(old) {
		t.Fatalf("test premise broken: %q was expected to be an illegal mRID", old)
	}
	if got := deriveResourceMRID(lfdi, "derc/active"); !isMRIDLegal(got) {
		t.Errorf("deriveResourceMRID produced an illegal mRID %q", got)
	}
}

// TestDeriveResourceMRIDIsStable pins determinism across calls. A client
// keys scheduled events by mRID (EPRI's schedule.c hashes event blocks on
// it), so a value that changed between two GETs of the same resource
// would present as a brand-new event on every poll.
func TestDeriveResourceMRIDIsStable(t *testing.T) {
	t.Parallel()

	const lfdi = "BBBB00000000000000000000000000000000BBBB"
	first := deriveResourceMRID(lfdi, "derc/active")
	for i := 0; i < 5; i++ {
		if got := deriveResourceMRID(lfdi, "derc/active"); got != first {
			t.Fatalf("call %d returned %q, want the stable %q", i, got, first)
		}
	}
}

// TestDeriveResourceMRIDIsDistinctPerDeviceAndKind is the collision
// assertion. Two devices sharing an mRID collide in the client's own
// mRID-keyed event hash, so one device's control would displace the
// other's: the cross-identity confusion [[data-invariants]] names
// directly. Two resources on the SAME device sharing one is the same
// failure one level down.
func TestDeriveResourceMRIDIsDistinctPerDeviceAndKind(t *testing.T) {
	t.Parallel()

	lfdis := []string{
		"AAAA00000000000000000000000000000000AAAA",
		"BBBB00000000000000000000000000000000BBBB",
		"CCCC00000000000000000000000000000000CCCC",
	}
	kinds := []string{"fsa/1", "derp/1", "dderc", "derc/active"}

	seen := make(map[string]string)
	for _, lfdi := range lfdis {
		for _, kind := range kinds {
			got := deriveResourceMRID(lfdi, kind)
			key := lfdi + " " + kind
			if prev, dup := seen[got]; dup {
				t.Errorf("mRID %q produced for both %q and %q", got, prev, key)
			}
			seen[got] = key
		}
	}
}

// TestDeriveResourceMRIDDependsOnTheWholeLFDI guards against a derivation
// that only reads a prefix. The bridge's canonical LFDIs are 40 hex
// characters and real fleets can share long prefixes, so two devices
// differing only in their final characters must still get distinct mRIDs.
func TestDeriveResourceMRIDDependsOnTheWholeLFDI(t *testing.T) {
	t.Parallel()

	a := deriveResourceMRID("AAAA0000000000000000000000000000000000A1", "derc/active")
	b := deriveResourceMRID("AAAA0000000000000000000000000000000000A2", "derc/active")
	if a == b {
		t.Errorf("two LFDIs differing only in the last character produced the same mRID %q", a)
	}
}
