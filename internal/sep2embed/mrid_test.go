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

// TestDeriveControlMRIDVariesPerGenerationAndStaysWireLegal covers the two
// properties a per-generation DERControl mRID must hold simultaneously
// (GAGO-094).
//
// Distinctness is why the generation is an input at all: mRID is the event's
// identity to a client scheduler, and a repeated mRID means a repeated
// command is silently ignored.
//
// Format stability is why the generation is HASHED rather than appended.
// Appending the number would grow the string as the counter grows, past the
// 16-octet mRIDType ceiling, and would introduce a separator that is not a
// hex digit. A generation of 0 and a generation near the uint64 ceiling must
// produce the same shape, so both ends of the range are exercised rather
// than a few small values.
func TestDeriveControlMRIDVariesPerGenerationAndStaysWireLegal(t *testing.T) {
	t.Parallel()

	const lfdi = "AAAA00000000000000000000000000000000AAAA"
	generations := []uint64{0, 1, 2, 3, 42, 1000, 4294967296, 18446744073709551615}

	seen := make(map[string]uint64, len(generations))
	for _, gen := range generations {
		got := deriveControlMRID(lfdi, dercMRIDKind, gen)

		if len(got) != mridHexChars {
			t.Errorf("deriveControlMRID(gen=%d) = %q, length %d, want exactly %d hex characters",
				gen, got, len(got), mridHexChars)
		}
		if !isMRIDLegal(got) {
			t.Errorf("deriveControlMRID(gen=%d) = %q, which is not a legal mRIDType", gen, got)
		}
		if got != strings.ToUpper(got) {
			t.Errorf("deriveControlMRID(gen=%d) = %q, want uppercase hex", gen, got)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("generations %d and %d both produced mRID %q; a repeated mRID is silently ignored by a client scheduler", prev, gen, got)
		}
		seen[got] = gen
	}
}

// TestDeriveControlMRIDIsDeterministic pins reproducibility, which is the
// reason a generation counter was chosen over a random value: a served-bytes
// test can only assert an expected mRID if the same inputs always yield the
// same output, and an operator can only correlate a log line to a stored
// control for the same reason.
func TestDeriveControlMRIDIsDeterministic(t *testing.T) {
	t.Parallel()

	const lfdi = "AAAA00000000000000000000000000000000AAAA"
	first := deriveControlMRID(lfdi, dercMRIDKind, 7)
	second := deriveControlMRID(lfdi, dercMRIDKind, 7)
	if first != second {
		t.Errorf("deriveControlMRID is not deterministic: %q then %q", first, second)
	}
}

// TestDeriveControlMRIDIsPerDevice guards the cross-device collision case.
// A client hashes event blocks by mRID, so two devices sharing an mRID at the
// same generation would have one device's control displace the other's in
// that hash.
func TestDeriveControlMRIDIsPerDevice(t *testing.T) {
	t.Parallel()

	a := deriveControlMRID("AAAA00000000000000000000000000000000AAAA", dercMRIDKind, 3)
	b := deriveControlMRID("BBBB00000000000000000000000000000000BBBB", dercMRIDKind, 3)
	if a == b {
		t.Errorf("two devices at generation 3 produced the same mRID %q", a)
	}
}

// TestDeriveControlMRIDDoesNotCollideWithTheOtherResources confirms a
// generation's control mRID never equals one of the device's stable resource
// mRIDs. The kind discriminators exist to keep a device's own resources
// distinct from each other; adding the generation suffix must not
// accidentally land on one of them.
func TestDeriveControlMRIDDoesNotCollideWithTheOtherResources(t *testing.T) {
	t.Parallel()

	const lfdi = "AAAA00000000000000000000000000000000AAAA"
	stable := map[string]string{
		fsaMRIDKind:   deriveResourceMRID(lfdi, fsaMRIDKind),
		derpMRIDKind:  deriveResourceMRID(lfdi, derpMRIDKind),
		ddercMRIDKind: deriveResourceMRID(lfdi, ddercMRIDKind),
		dercMRIDKind:  deriveResourceMRID(lfdi, dercMRIDKind),
	}

	for gen := uint64(0); gen < 8; gen++ {
		got := deriveControlMRID(lfdi, dercMRIDKind, gen)
		for kind, want := range stable {
			if got == want {
				t.Errorf("generation %d's control mRID %q collides with the stable mRID for kind %q", gen, got, kind)
			}
		}
	}
}
