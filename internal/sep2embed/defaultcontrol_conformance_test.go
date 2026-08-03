package sep2embed

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// This file pins the wire form of the SHIPPED DefaultDERControl: the control
// a device applies whenever no DERControl is active.
//
// It is the third link in a chain, and none of the three links can stand
// alone. sep2config's TestDefaultPolicy_CommandsNothing asserts the compiled-in
// policy sets no mode field; cmd/bridge's TestSEP2EmbedConfigMapsFields
// asserts that policy reaches sep2embed.Config verbatim rather than being
// replaced at the mapping layer; this file asserts what those bytes look like
// once served. A struct-level assertion at either end would not catch a
// serializer that materialized an element for a nil pointer, which is exactly
// the class of defect that matters when the element in question commands a
// grid disconnect.

// emptyDefaultControl mirrors sep2config.DefaultPolicy's shipped
// DefaultControl: a present but EMPTY DERControlBase.
//
// It is restated here rather than imported because sep2config is a
// policy-only package this one deliberately does not depend on (see
// DERProgramSeed's doc comment for that boundary). The restatement is safe
// because the two are tied together by the mapping test in cmd/bridge, which
// fails if the real policy stops matching this shape.
func emptyDefaultControl() sep2.DefaultDERControl {
	return sep2.DefaultDERControl{DERControlBase: &sep2.DERControlBase{}}
}

// TestServedDefaultDERControlCommandsNothing asserts the served bytes of the
// shipped fallback carry no commanded mode at all.
//
// The two named elements are singled out because they are the ones with
// physical consequences. opModConnect is documented as connecting or
// disconnecting the DER from the grid, with the annotation invoking galvanic
// isolation (sep.xsd:3758), so any value at all is a commanded switch
// operation rather than a statement of preference. Under the 2023 edition's
// per-mode fallback evaluation it would be asserted continuously whenever no
// control is active, not applied once at expiry.
//
// Absence is asserted, not falsity, and the distinction is the whole point: a
// served opModConnect of false is not "no command", it is a commanded
// DISCONNECT of every DER the bridge serves.
func TestServedDefaultDERControlCommandsNothing(t *testing.T) {
	t.Parallel()

	baseURL, devices, _, _ := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DefaultControl = emptyDefaultControl()
	}, "DDERCEMPTY1")
	d := devices[0]

	ddercHref := "/edev/" + d.edevID + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID + "/dderc"
	status, body := getSEP2(t, d, baseURL+ddercHref)
	if status != http.StatusOK {
		t.Fatalf("GET DefaultDERControl status = %d, want 200\nbody=%s", status, ddercHref)
	}

	space, local := rootElement(t, body)
	if space != "urn:ieee:std:2030.5:ns" || local != "DefaultDERControl" {
		t.Fatalf("root element = {%s}%s, want {urn:ieee:std:2030.5:ns}DefaultDERControl\nbody=%s", space, local, body)
	}

	// DERControlBase is minOccurs=1 on DefaultDERControl (sep.xsd:3270), so
	// the container must be PRESENT even though it commands nothing. Absent
	// would be a schema violation; present and empty is the conformant way to
	// say "no mode".
	if !bytes.Contains(body, []byte("<DERControlBase>")) {
		t.Errorf("served DefaultDERControl carries no DERControlBase; it is minOccurs=1 (sep.xsd:3270) and its absence makes the document non-conformant\nbody=%s", body)
	}

	// No commanded mode of any kind. Asserted on the raw bytes rather than
	// through a decode, because a decode into a struct with pointer fields
	// reports nil for an element that was never there AND for one carrying an
	// empty value, and only one of those is acceptable.
	for _, forbidden := range []struct {
		element string
		why     string
	}{
		{"opModConnect", "any value commands a grid connect or disconnect (galvanic isolation, sep.xsd:3758); a fallback must command nothing"},
		{"opModEnergize", "a fallback must not energize or de-energize a device"},
		{"opModTargetW", "a fixed-power target in the fallback disables the device's autonomous curtailment (1547-2018 clause 5.3)"},
		{"opModTargetVar", "a fixed-var target in the fallback disables the device's autonomous volt-var (1547-2018 clause 5.3)"},
		{"setGradW", "sep.xsd:3306 says it SHALL update DERSettings, an installer-owned persistent write"},
		{"setESDelay", "sep.xsd:3271 says the setES family SHALL update DERSettings, an installer-owned persistent write"},
	} {
		if bytes.Contains(body, []byte("<"+forbidden.element+">")) {
			t.Errorf("served DefaultDERControl carries <%s>: %s\nbody=%s", forbidden.element, forbidden.why, body)
		}
	}

	// The full served document, recorded on success as well as failure, so
	// the bytes a device actually receives for the shipped fallback are
	// visible in the test log rather than inferred from a list of absences.
	t.Logf("served DefaultDERControl bytes: %s", body)
}

// TestServedDefaultDERControlPreservesOperatorValue is the other half, and it
// is what keeps the test above from being satisfiable by a seeding path that
// simply drops every mode field.
//
// An operator who configures opModConnect must get it, verbatim, on the wire.
// createDERProgram stamps only Href and MRID onto the configured control, and
// this asserts that nothing else is rewritten on the way out.
func TestServedDefaultDERControlPreservesOperatorValue(t *testing.T) {
	t.Parallel()

	// Explicitly FALSE, not true. A false value is the one a fill-or-override
	// bug would silently swallow, because it is indistinguishable from the
	// zero value of the underlying bool once the pointer is lost.
	configured := false
	baseURL, devices, _, _ := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DefaultControl = sep2.DefaultDERControl{
			DERControlBase: &sep2.DERControlBase{OpModConnect: &configured},
		}
	}, "DDERCOPERATOR1")
	d := devices[0]

	ddercHref := "/edev/" + d.edevID + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID + "/dderc"
	status, body := getSEP2(t, d, baseURL+ddercHref)
	if status != http.StatusOK {
		t.Fatalf("GET DefaultDERControl status = %d, want 200\nbody=%s", status, body)
	}

	const want = `<opModConnect>false</opModConnect>`
	if !bytes.Contains(body, []byte(want)) {
		t.Errorf("served DefaultDERControl does not carry %s; an operator-configured false must survive to the wire rather than being dropped as though unset\nbody=%s",
			want, body)
	}
}
