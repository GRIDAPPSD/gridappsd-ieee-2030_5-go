package sep2embed

import (
	"encoding/hex"
	"testing"
)

// assertValidMRID checks the one property sep.xsd's mRIDType actually
// requires: exactly 16 bytes of hexadecimal, i.e. 32 hex characters and
// nothing else. A conformant client parses this field as hexBinary and
// aborts the enclosing document when it is not, so "close enough" is a
// wire failure, not a cosmetic one.
func assertValidMRID(t *testing.T, field, got string) {
	t.Helper()
	if len(got) != 32 {
		t.Errorf("%s = %q: length %d, want 32 hex characters (mRIDType is HexBinary128)", field, got, len(got))
		return
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Errorf("%s = %q: not valid hexadecimal (%v); mRIDType is HexBinary128", field, got, err)
	}
}

func TestDeriveMRIDIsSchemaValidHexBinary128(t *testing.T) {
	t.Parallel()

	// Includes the empty LFDI deliberately: deriveMRID must produce a valid
	// mRID for every input rather than propagating a caller's bad value onto
	// the wire the way "<LFDI>-active" concatenation did.
	lfdis := []string{
		"",
		"4E8114FA4EC3B0E465FB3AEBFA79F6EA5CD1807A",
		"2FDB03105E674A65A6980CB53E5B8BBC38EAD91B",
	}
	kinds := []string{mridKindFSA, mridKindDERProgram, mridKindDERControl, mridKindDefaultDERControl}

	for _, lfdi := range lfdis {
		for _, kind := range kinds {
			assertValidMRID(t, "deriveMRID("+kind+", "+lfdi+")", deriveMRID(kind, lfdi))
		}
	}
}

func TestDeriveMRIDIsDeterministic(t *testing.T) {
	t.Parallel()

	const lfdi = "4E8114FA4EC3B0E465FB3AEBFA79F6EA5CD1807A"
	// Determinism is what lets a client that cached an mRID still recognise
	// the resource after a bridge restart. A random identifier would pass the
	// validity test above and still break that.
	first := deriveMRID(mridKindDERControl, lfdi)
	if second := deriveMRID(mridKindDERControl, lfdi); first != second {
		t.Errorf("deriveMRID is not deterministic: %q then %q", first, second)
	}
}

func TestDeriveMRIDSeparatesKindsAndDevices(t *testing.T) {
	t.Parallel()

	const lfdiA = "4E8114FA4EC3B0E465FB3AEBFA79F6EA5CD1807A"
	const lfdiB = "2FDB03105E674A65A6980CB53E5B8BBC38EAD91B"

	// Distinct kinds on ONE device must differ: this is the property the old
	// "-active"/"-dderc"/"-fsa" suffixes carried, and losing it would give a
	// device's DERControl and DefaultDERControl the same identity.
	seen := make(map[string]string)
	for _, kind := range []string{mridKindFSA, mridKindDERProgram, mridKindDERControl, mridKindDefaultDERControl} {
		got := deriveMRID(kind, lfdiA)
		if prev, dup := seen[got]; dup {
			t.Errorf("deriveMRID collision on one device: kinds %q and %q both yield %q", prev, kind, got)
		}
		seen[got] = kind
	}

	// The same kind on two devices must differ, or two devices would share
	// one control identity.
	if deriveMRID(mridKindDERControl, lfdiA) == deriveMRID(mridKindDERControl, lfdiB) {
		t.Error("deriveMRID collision across devices for the same kind")
	}
}
