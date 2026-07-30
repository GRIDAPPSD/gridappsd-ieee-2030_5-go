package sep2embed

import (
	"context"
	"encoding/xml"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// discoveryTestEntries is the shared fixture for the GAGO-094 discovery
// seeding tests. Three entries with distinct canonical-shaped LFDIs, so
// each assertion below can prove a record is keyed off the entry's OWN
// identity rather than a shared or positional value (the exact
// cross-device index confusion data-invariants Rule 2 forbids).
func discoveryTestEntries() []registry.Entry {
	return []registry.Entry{
		{MRID: "mrid-inv-1", Name: "Inverter 1", LFDI: "AAAA00000000000000000000000000000000AAAA", Placeholder: true},
		{MRID: "mrid-bat-1", Name: "Battery 1", LFDI: "BBBB00000000000000000000000000000000BBBB", Placeholder: true},
		{MRID: "mrid-sol-1", Name: "Solar 1", LFDI: "CCCC00000000000000000000000000000000CCCC", Placeholder: true},
	}
}

// TestSeedStoresCreatesRegistrationPerDevice is the HIGH-1 assertion.
// Every seeded EndDevice must carry a RegistrationLink AND a matching
// Registration record in the store, because the EPRI reference client
// aborts with "EndDevice does not contain RegistrationLink." when the
// link is absent (oeg_client.c:600-601) and would 404 on the followed
// href if the record were missing.
//
// The assertions are on FIELD VALUES, not merely non-nil: per
// data-invariants Rule 1 a syntactically present but semantically wrong
// href or pIN is exactly the invisible-breakage class this must catch.
func TestSeedStoresCreatesRegistrationPerDevice(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	entries := discoveryTestEntries()
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	pin := uint32(111115)
	if err := seedStores(ctx, stores, reg, nil, &pin, nil); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	for _, e := range entries {
		dev, err := stores.EndDevices.Get(ctx, e.LFDI)
		if err != nil {
			t.Fatalf("EndDevices.Get(%q): %v", e.LFDI, err)
		}

		// The link the client checks with se_exists(e, RegistrationLink).
		if dev.RegistrationLink == nil {
			t.Fatalf("dev.RegistrationLink is nil for LFDI %q (EPRI oeg_client.c aborts registration on this)", e.LFDI)
		}
		wantHref := "/edev/" + e.LFDI + "/rg"
		if dev.RegistrationLink.Href != wantHref {
			t.Errorf("dev.RegistrationLink.Href = %q, want %q", dev.RegistrationLink.Href, wantHref)
		}

		// The record that href must actually resolve to. Keyed by the
		// device's own store id (its canonical LFDI), which is exactly
		// what core's HandleGetRegistration looks up (registration.go:75).
		rec, err := stores.Registrations.Get(ctx, e.LFDI)
		if err != nil {
			t.Fatalf("Registrations.Get(%q): %v (RegistrationLink would 404)", e.LFDI, err)
		}
		if rec.Href != wantHref {
			t.Errorf("registration.Href = %q, want %q", rec.Href, wantHref)
		}
		if rec.PIN != pin {
			t.Errorf("registration.PIN = %d, want %d", rec.PIN, pin)
		}
		if rec.DateTimeRegistered <= 0 {
			t.Errorf("registration.DateTimeRegistered = %d, want a positive Unix epoch second", rec.DateTimeRegistered)
		}
	}
}

// TestSeedStoresRegistrationXMLShape asserts the wire-level shape a
// third-party C client actually parses, not just the in-memory struct.
// pIN and dateTimeRegistered are REQUIRED (non-omitempty) elements on
// sep2.Registration, so both must appear in the emitted XML.
func TestSeedStoresRegistrationXMLShape(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	entries := discoveryTestEntries()
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	pin := uint32(111115)
	if err := seedStores(ctx, stores, reg, nil, &pin, nil); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	lfdi := entries[0].LFDI
	rec, err := stores.Registrations.Get(ctx, lfdi)
	if err != nil {
		t.Fatalf("Registrations.Get: %v", err)
	}

	out, err := xml.Marshal(&rec)
	if err != nil {
		t.Fatalf("xml.Marshal: %v", err)
	}
	got := string(out)

	// The 2030.5 namespace must be present: a client parsing against
	// sep.xsd rejects an unqualified element.
	if !strings.Contains(got, `xmlns="urn:ieee:std:2030.5:ns"`) {
		t.Errorf("marshalled Registration missing 2030.5 namespace:\n%s", got)
	}
	if !strings.Contains(got, "<pIN>111115</pIN>") {
		t.Errorf("marshalled Registration missing <pIN>111115</pIN>:\n%s", got)
	}
	if !strings.Contains(got, "<dateTimeRegistered>") {
		t.Errorf("marshalled Registration missing <dateTimeRegistered>:\n%s", got)
	}
	if !strings.Contains(got, `href="/edev/`+lfdi+`/rg"`) {
		t.Errorf("marshalled Registration href not the canonical /rg path:\n%s", got)
	}
}

// TestSeedStoresCreatesFSAPerDevice is the HIGH-2 assertion. A
// link-traversing client reaches a DERProgram only via
// EndDevice.FunctionSetAssignmentsListLink -> the FSA list -> the FSA's
// own DERProgramListLink (EPRI oeg_client.c:541-552's fsa()/fsa_list()).
// With the FSA store empty, GET /edev/{lfdi}/fsa returns all="0" and the
// walk dead-ends.
//
// The FSA id asserted here is controlFSAID, the SAME id the DOWN path
// writes DERControls under (control.go's controlFSAID). Seeding a
// different id would serve a discoverable-but-empty program and silently
// strand every control this bridge applies: that coupling is the
// invariant, so it is asserted explicitly rather than left implicit.
func TestSeedStoresCreatesFSAPerDevice(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	entries := discoveryTestEntries()
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	pin := uint32(111115)
	pollRate := uint32(300)
	if err := seedStores(ctx, stores, reg, nil, &pin, &pollRate); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	for _, e := range entries {
		dev, err := stores.EndDevices.Get(ctx, e.LFDI)
		if err != nil {
			t.Fatalf("EndDevices.Get(%q): %v", e.LFDI, err)
		}

		if dev.FunctionSetAssignmentsListLink == nil {
			t.Fatalf("dev.FunctionSetAssignmentsListLink is nil for LFDI %q; a client cannot begin the FSA walk", e.LFDI)
		}
		wantListHref := "/edev/" + e.LFDI + "/fsa"
		if dev.FunctionSetAssignmentsListLink.Href != wantListHref {
			t.Errorf("FunctionSetAssignmentsListLink.Href = %q, want %q",
				dev.FunctionSetAssignmentsListLink.Href, wantListHref)
		}
		// all="1" is what the client's list walk reads; all="0" is the
		// exact symptom Hale measured.
		if dev.FunctionSetAssignmentsListLink.All != 1 {
			t.Errorf("FunctionSetAssignmentsListLink.All = %d, want 1", dev.FunctionSetAssignmentsListLink.All)
		}

		// The FSA record itself, keyed (parent=device store id, id=controlFSAID).
		count, err := stores.FSAs.Count(ctx, e.LFDI)
		if err != nil {
			t.Fatalf("FSAs.Count(%q): %v", e.LFDI, err)
		}
		if count != 1 {
			t.Fatalf("FSAs.Count(%q) = %d, want 1 (GET /edev/{lfdi}/fsa would serve all=%d)", e.LFDI, count, count)
		}

		fsa, err := stores.FSAs.Get(ctx, e.LFDI, controlFSAID)
		if err != nil {
			t.Fatalf("FSAs.Get(%q, %q): %v", e.LFDI, controlFSAID, err)
		}
		wantFSAHref := "/edev/" + e.LFDI + "/fsa/" + controlFSAID
		if fsa.Href != wantFSAHref {
			t.Errorf("fsa.Href = %q, want %q", fsa.Href, wantFSAHref)
		}
		if fsa.DERProgramListLink == nil {
			t.Fatalf("fsa.DERProgramListLink is nil for LFDI %q; the DERProgram walk dead-ends here", e.LFDI)
		}
		wantDERPHref := derProgramListHref(e.LFDI, controlFSAID)
		if fsa.DERProgramListLink.Href != wantDERPHref {
			t.Errorf("fsa.DERProgramListLink.Href = %q, want %q", fsa.DERProgramListLink.Href, wantDERPHref)
		}
		// mRID must be per-device, not a shared constant: two devices
		// sharing an mRID is a distinct-identity violation. It must
		// ALSO be a wire-legal mRIDType value: hexBinary, at most 16
		// octets. An LFDI-composed mRID is neither (40+ characters, and
		// the separator and kind text are not hex digits), and a
		// schema-driven client rejects the whole document on it, so the
		// assertion is on the derived value, not on containment.
		if fsa.MRID == "" {
			t.Errorf("fsa.MRID is empty for LFDI %q", e.LFDI)
		}
		if !isMRIDLegal(fsa.MRID) {
			t.Errorf("fsa.MRID = %q is not a legal mRIDType (hexBinary, <= %d hex chars); a schema-driven client rejects the entire FSA document",
				fsa.MRID, mridHexChars)
		}
		if want := deriveResourceMRID(e.LFDI, fsaMRIDKind); fsa.MRID != want {
			t.Errorf("fsa.MRID = %q, want %q derived from the device's own LFDI", fsa.MRID, want)
		}
	}

	// Cross-device distinctness: no two devices may share an FSA mRID or
	// href. This is the sibling-array index-confusion failure mode
	// data-invariants calls out by name.
	seen := make(map[string]string)
	for _, e := range entries {
		fsa, err := stores.FSAs.Get(ctx, e.LFDI, controlFSAID)
		if err != nil {
			t.Fatalf("FSAs.Get(%q): %v", e.LFDI, err)
		}
		if prev, dup := seen[fsa.MRID]; dup {
			t.Errorf("fsa.MRID %q reused by %q and %q", fsa.MRID, prev, e.LFDI)
		}
		seen[fsa.MRID] = e.LFDI
	}
}

// TestSeedStoresFSAMatchesControlPathScope pins the coupling that makes
// the discovered program the SAME program the DOWN path writes into. If
// controlFSAID or controlDERProgramID ever changes without the seeding
// following, a client would discover an empty program while controls
// land somewhere it never looks.
func TestSeedStoresFSAMatchesControlPathScope(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	entries := discoveryTestEntries()
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	pin := uint32(111115)
	if err := seedStores(ctx, stores, reg, nil, &pin, nil); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	lfdi := entries[0].LFDI
	fsa, err := stores.FSAs.Get(ctx, lfdi, controlFSAID)
	if err != nil {
		t.Fatalf("FSAs.Get: %v", err)
	}

	// The DERProgramList the client will GET must be the list that
	// contains controlDERProgramID, which is where ApplyControlDelta's
	// ensureDERProgram creates the program.
	wantProgramHref := "/edev/" + lfdi + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID
	if got := fsa.DERProgramListLink.Href + "/" + controlDERProgramID; got != wantProgramHref {
		t.Errorf("discovered program href = %q, want the control path's own %q", got, wantProgramHref)
	}
}

// TestSeedStoresFSAXMLShape asserts the emitted FSA XML, since a C
// client parses bytes, not Go structs.
func TestSeedStoresFSAXMLShape(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	entries := discoveryTestEntries()
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	pin := uint32(111115)
	if err := seedStores(ctx, stores, reg, nil, &pin, nil); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	lfdi := entries[0].LFDI
	fsa, err := stores.FSAs.Get(ctx, lfdi, controlFSAID)
	if err != nil {
		t.Fatalf("FSAs.Get: %v", err)
	}

	out, err := xml.Marshal(&fsa)
	if err != nil {
		t.Fatalf("xml.Marshal: %v", err)
	}
	got := string(out)

	if !strings.Contains(got, `xmlns="urn:ieee:std:2030.5:ns"`) {
		t.Errorf("marshalled FSA missing 2030.5 namespace:\n%s", got)
	}
	if !strings.Contains(got, "<DERProgramListLink") {
		t.Errorf("marshalled FSA missing <DERProgramListLink>:\n%s", got)
	}
	if !strings.Contains(got, `href="`+derProgramListHref(lfdi, controlFSAID)+`"`) {
		t.Errorf("marshalled FSA DERProgramListLink href wrong:\n%s", got)
	}

	// The XSD sequence puts FunctionSetAssignmentsBase's link fields
	// BEFORE mRID. A C client that parses strictly by sequence order
	// fails on a transposition, so assert the order on the bytes.
	linkIdx := strings.Index(got, "<DERProgramListLink")
	mridIdx := strings.Index(got, "<mRID>")
	if linkIdx < 0 || mridIdx < 0 {
		t.Fatalf("expected both DERProgramListLink and mRID in:\n%s", got)
	}
	if linkIdx > mridIdx {
		t.Errorf("DERProgramListLink must precede mRID per the xsd:sequence; got:\n%s", got)
	}
}

// TestSeedStoresEndDeviceXMLCarriesBothLinks asserts that the served
// EndDevice, the single resource the EPRI client's end_device() callback
// inspects, carries both new links in XSD sequence order.
func TestSeedStoresEndDeviceXMLCarriesBothLinks(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	entries := discoveryTestEntries()
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	pin := uint32(111115)
	if err := seedStores(ctx, stores, reg, nil, &pin, nil); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	lfdi := entries[0].LFDI
	dev, err := stores.EndDevices.Get(ctx, lfdi)
	if err != nil {
		t.Fatalf("EndDevices.Get: %v", err)
	}

	out, err := xml.Marshal(&dev)
	if err != nil {
		t.Fatalf("xml.Marshal: %v", err)
	}
	got := string(out)

	if !strings.Contains(got, `<RegistrationLink href="/edev/`+lfdi+`/rg"`) {
		t.Errorf("EndDevice XML missing RegistrationLink:\n%s", got)
	}
	if !strings.Contains(got, `<FunctionSetAssignmentsListLink`) {
		t.Errorf("EndDevice XML missing FunctionSetAssignmentsListLink:\n%s", got)
	}

	// EndDevice's own xsd:sequence: FunctionSetAssignmentsListLink then
	// RegistrationLink (see core's enddevice.go field order).
	fsaIdx := strings.Index(got, "<FunctionSetAssignmentsListLink")
	regIdx := strings.Index(got, "<RegistrationLink")
	if fsaIdx > regIdx {
		t.Errorf("FunctionSetAssignmentsListLink must precede RegistrationLink per the xsd:sequence; got:\n%s", got)
	}
}

// TestSeedStoresNoPINSeedsNoRegistration pins the refusal path. When no
// PIN policy value is supplied, seedStores must NOT fabricate one: pIN is
// a required wire field whose value the client may check, and inventing a
// value is precisely the "silently filling a default that does not match
// the upstream scheme" failure data-invariants Rule 2 forbids. No
// Registration record and no RegistrationLink is the honest state.
func TestSeedStoresNoPINSeedsNoRegistration(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	entries := discoveryTestEntries()
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	if err := seedStores(ctx, stores, reg, nil, nil, nil); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	for _, e := range entries {
		dev, err := stores.EndDevices.Get(ctx, e.LFDI)
		if err != nil {
			t.Fatalf("EndDevices.Get(%q): %v", e.LFDI, err)
		}
		if dev.RegistrationLink != nil {
			t.Errorf("dev.RegistrationLink = %+v with no PIN policy; want nil rather than a link to an absent record",
				dev.RegistrationLink)
		}
		if _, err := stores.Registrations.Get(ctx, e.LFDI); err == nil {
			t.Errorf("Registrations.Get(%q) succeeded with no PIN policy; want no fabricated record", e.LFDI)
		}
	}
}

// TestSeedStoresFSADescriptionIsSet keeps the FSA record self-describing
// on the wire. Description is optional in the schema, so this asserts the
// value we choose rather than that some value exists.
//
// It also pins the LENGTH. IEEE 2030.5 types description as String32, and
// a schema-driven client enforces that bound on the simple value and
// rejects the ENTIRE resource when it is exceeded, not just that one
// field. Measured against the EPRI reference client, whose XS_STRING
// check is "reject when strlen(data) > n-1": 31 characters parse, 32 do
// not. So the effective ceiling is 31, and a description one character
// too long would silently make every FSA undiscoverable.
func TestSeedStoresFSADescriptionIsSet(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	entries := discoveryTestEntries()
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	pin := uint32(111115)
	if err := seedStores(ctx, stores, reg, nil, &pin, nil); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	fsa, err := stores.FSAs.Get(ctx, entries[0].LFDI, controlFSAID)
	if err != nil {
		t.Fatalf("FSAs.Get: %v", err)
	}
	if fsa.Description != fsaDescription {
		t.Errorf("fsa.Description = %q, want %q", fsa.Description, fsaDescription)
	}
	if n := len(fsaDescription); n > maxDescriptionChars {
		t.Errorf("fsaDescription is %d characters (%q); the wire ceiling is %d and an over-long value makes a client reject the whole FSA",
			n, fsaDescription, maxDescriptionChars)
	}
}
