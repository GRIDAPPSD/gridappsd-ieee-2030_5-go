package sep2embed

import (
	"fmt"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/assembly"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// urlIndexFor returns the store id, and therefore the {id} URL segment, that
// seeding assigned to the device with the given CIM mRID.
//
// Since IEEECORE-URLINDEX that id is an opaque server-chosen index rather
// than the device's LFDI, so a test must ASK for it rather than construct it
// from device identity. Constructing it would reintroduce exactly the
// LFDI-in-URL coupling this change removed, and would let the tests keep
// passing against a server that had quietly reverted.
//
// It fails the test if the mRID has no assignment, which means seeding never
// ran or never saw that device.
func urlIndexFor(t *testing.T, stores *assembly.Stores, mrid string) string {
	t.Helper()
	id, ok := stores.EndDeviceIndexes.IndexFor(mrid)
	if !ok {
		t.Fatalf("no URL index assigned for mRID %q: was the device seeded?", mrid)
	}
	return id
}

// embedURLIndex is urlIndexFor against an already-constructed Embed, whose
// stores are not otherwise reachable from a test.
func embedURLIndex(t *testing.T, e *Embed, mrid string) string {
	t.Helper()
	return urlIndexFor(t, e.stores, mrid)
}

// fixtureRegistryFor builds a registry holding one valid entry per mRID, with
// synthetic but well-formed 40-hex LFDIs. Used by tests that care about
// index assignment rather than about certificate-derived identity.
func fixtureRegistryFor(t *testing.T, mrids []string) *registry.Registry {
	t.Helper()
	reg := registry.New()
	entries := make([]registry.Entry, 0, len(mrids))
	for i, m := range mrids {
		// A distinct, canonical-shaped LFDI per entry: 40 uppercase hex
		// characters, which is what registry validation requires.
		lfdi := fmt.Sprintf("%040X", i+1)
		entries = append(entries, registry.Entry{MRID: m, Name: m, LFDI: lfdi})
	}
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	return reg
}
