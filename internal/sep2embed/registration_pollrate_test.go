package sep2embed

import (
	"context"
	"encoding/xml"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// seedOneDevice seeds a single-device fleet with the given poll-rate
// resolver and returns the stored Registration marshalled to the bytes a
// client would receive from GET /edev/{id}/rg.
//
// Asserting on marshalled bytes rather than on the struct field is the point
// of these tests: pollRate is an xsd:attribute (sep.xsd:190), and a Go tag
// that lost its ",attr" would still round-trip through encoding/xml while
// emitting a child element that a schema-validating client rejects outright.
// The stored uint32 looks identical in both cases.
func seedOneDevice(t *testing.T, lfdi string, resolvePollRate func(string) (uint32, bool)) string {
	t.Helper()

	reg := registry.New()
	if err := reg.Add(registry.Entry{MRID: "mrid-rate", Name: "Rate", LFDI: lfdi}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	if err := seedStores(ctx, stores, reg, seedPolicy{
		// An obvious dummy fixture PIN, never a shipped default.
		resolvePIN:      func(string) (uint32, bool) { return 123455, true },
		resolvePollRate: resolvePollRate,
	}); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	stored, err := stores.Registrations.Get(ctx, urlIndexFor(t, stores, "mrid-rate"))
	if err != nil {
		t.Fatalf("Registrations.Get: %v", err)
	}

	data, err := xml.Marshal(&stored)
	if err != nil {
		t.Fatalf("marshal Registration: %v", err)
	}
	return string(data)
}

// TestSeededRegistrationServesPollRateAsAttribute asserts the configured
// rate reaches the wire in ATTRIBUTE position on the Registration element,
// with the configured value, and that the required sequence elements keep
// their sep.xsd order around it.
func TestSeededRegistrationServesPollRateAsAttribute(t *testing.T) {
	t.Parallel()

	served := seedOneDevice(t, "AAAA00000000000000000000000000000000AAAA",
		func(string) (uint32, bool) { return 900, true })

	if !strings.Contains(served, `pollRate="900"`) {
		t.Errorf("served bytes lack the pollRate attribute; got:\n%s", served)
	}
	if strings.Contains(served, "<pollRate>") {
		t.Errorf("pollRate served as a child element, but sep.xsd:190 declares it an attribute; got:\n%s", served)
	}

	// The attribute must sit on the Registration start tag, not leak into
	// a child. Everything before the first '>' is the root start tag.
	rootEnd := strings.Index(served, ">")
	if rootEnd == -1 {
		t.Fatalf("no root start tag in served bytes:\n%s", served)
	}
	if !strings.Contains(served[:rootEnd], `pollRate="900"`) {
		t.Errorf("pollRate is not an attribute of the Registration root element; root tag = %q", served[:rootEnd])
	}

	// Element order is unchanged by the attribute: dateTimeRegistered then
	// pIN, both still present.
	dt := strings.Index(served, "<dateTimeRegistered>")
	pin := strings.Index(served, "<pIN>")
	if dt == -1 || pin == -1 {
		t.Fatalf("required sequence elements missing; got:\n%s", served)
	}
	if dt > pin {
		t.Errorf("dateTimeRegistered appears after pIN, violating sep.xsd sequence order; got:\n%s", served)
	}
}

// TestSeededRegistrationOmitsPollRateWhenUnresolved is the no-op guard. A
// nil resolver, or one with no opinion for this device, must leave the
// attribute ABSENT so the client applies sep.xsd's own 900-second default.
// Emitting pollRate="0" instead would advertise a continuous poll, which is
// the failure this pointer/ok plumbing exists to prevent.
func TestSeededRegistrationOmitsPollRateWhenUnresolved(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		resolve func(string) (uint32, bool)
	}{
		{name: "nil resolver", resolve: nil},
		{name: "resolver declines", resolve: func(string) (uint32, bool) { return 0, false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			served := seedOneDevice(t, "BBBB00000000000000000000000000000000BBBB", tc.resolve)

			if strings.Contains(served, "pollRate") {
				t.Errorf("pollRate present with no configured rate; want the attribute omitted so the client applies the schema default. got:\n%s", served)
			}
			// The Registration itself is still well formed and complete.
			for _, elem := range []string{"<dateTimeRegistered>", "<pIN>"} {
				if !strings.Contains(served, elem) {
					t.Errorf("required element %s missing; got:\n%s", elem, served)
				}
			}
		})
	}
}

// TestSeededRegistrationPollRateResolverIsKeyedOnLFDI asserts the resolver
// receives the device's canonical LFDI, not its URL index. The URL index is
// an addressing artifact with no meaning in an operator's config, so a
// per-device rate map keyed on LFDI would silently miss every device if
// seeding ever passed the index instead.
func TestSeededRegistrationPollRateResolverIsKeyedOnLFDI(t *testing.T) {
	t.Parallel()

	const lfdi = "CCCC00000000000000000000000000000000CCCC"
	var got string
	served := seedOneDevice(t, lfdi, func(k string) (uint32, bool) {
		got = k
		return 60, true
	})

	if got != lfdi {
		t.Errorf("poll-rate resolver received key %q, want the canonical LFDI %q", got, lfdi)
	}
	if !strings.Contains(served, `pollRate="60"`) {
		t.Errorf("per-device rate not served; got:\n%s", served)
	}
}
