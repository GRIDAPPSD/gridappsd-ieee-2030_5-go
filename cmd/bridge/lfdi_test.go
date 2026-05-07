package main

import "testing"

func TestPlaceholderLFDIDeterministic(t *testing.T) {
	a := placeholderLFDI("_C1C3E687-6FFD-C753-582B-632A27E28507")
	b := placeholderLFDI("_C1C3E687-6FFD-C753-582B-632A27E28507")
	if a != b {
		t.Fatalf("placeholderLFDI not deterministic: %q vs %q", a, b)
	}
}

func TestPlaceholderLFDIShape(t *testing.T) {
	got := placeholderLFDI("any-mrid")
	if len(got) != 40 {
		t.Errorf("LFDI length: got %d want 40", len(got))
	}
	for _, r := range got {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Errorf("LFDI %q has non-lowercase-hex rune %q", got, r)
		}
	}
}

func TestPlaceholderLFDIDistinct(t *testing.T) {
	a := placeholderLFDI("_AAA")
	b := placeholderLFDI("_BBB")
	if a == b {
		t.Errorf("distinct mRIDs collided to %q", a)
	}
}
