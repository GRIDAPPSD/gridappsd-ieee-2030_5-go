package sep2embed

import (
	"bytes"
	"encoding/hex"
	"encoding/xml"
	"net/http"
	"testing"
)

// The tests in this file lock the wire form of the seeded default
// DERProgram, and of the FunctionSetAssignments that makes it reachable, as
// the embedded server actually serves them. They assert on the SERVED BYTES
// rather than on a Go struct, for the same reason the MirrorUsagePoint
// conformance tests in mirror_conformance_test.go do.
//
// The reason is specific and recent, not a general preference. A round trip
// through this project's own marshaller cannot see the class of defect that
// matters here: core shipped a wire regression in which replyTo and
// responseRequired were emitted as ELEMENTS where sep.xsd declares them
// ATTRIBUTES (IEEECORE-103). Marshal-then-unmarshal is symmetric, so it
// round-tripped perfectly while every conformant client rejected the
// document. Only bytes catch that.
//
// Schema authority is sep.xsd (IEEE 2030.5-2018, Model Build 20180301, and
// the identical copy vendored with core). The sequence pinned below for
// DERProgram is the concatenation of its base types:
//
//	Resource         (sep.xsd:5393): href attribute
//	IdentifiedObject (sep.xsd:5324): mRID (min 1), description, version
//	DERProgram       (sep.xsd:4094): ActiveDERControlListLink,
//	                                 DefaultDERControlLink,
//	                                 DERControlListLink,
//	                                 DERCurveListLink,
//	                                 primacy (min 1)
//
// and for FunctionSetAssignments the relevant facts are that mRID is
// mRIDType (sep.xsd:5919, an extension of HexBinary128, so exactly 32 hex
// characters) and description is String32 (sep.xsd:6319, xs:maxLength 32).

// elementText returns the text content of the first element with the given
// local name in the served bytes.
//
// It walks the token stream rather than unmarshalling into a struct on
// purpose: a struct field would happily accept a value carried as an
// attribute instead of an element, or a value at the wrong nesting depth,
// which is the defect class these tests exist to catch. Reporting "element
// not found" for a value that was emitted as an attribute is exactly the
// failure that should be visible.
func elementText(t *testing.T, body []byte, local string) string {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("element %q not found in served bytes\nbody=%s", local, body)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != local {
			continue
		}
		var text string
		if err := dec.DecodeElement(&text, &se); err != nil {
			t.Fatalf("decode element %q: %v\nbody=%s", local, err, body)
		}
		return text
	}
}

// assertNoAttribute fails when any element in the served bytes carries an
// attribute with the given name. This is the direct guard against the
// IEEECORE-103 defect class in the other direction: a value sep.xsd declares
// as an element must not be emitted as an attribute.
func assertNoAttribute(t *testing.T, body []byte, attr string) {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if err != nil {
			return
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		for _, a := range se.Attr {
			if a.Name.Local == attr {
				t.Fatalf("element <%s> carries %q as an ATTRIBUTE; sep.xsd declares it an element\nbody=%s",
					se.Name.Local, attr, body)
			}
		}
	}
}

// TestGETDERProgramListServesSeededProgramBeforeAnyControl is the wire-level
// statement of what seeding the default DERProgram buys.
//
// Before this change the served DERProgramList was empty until a control
// delta arrived, so a client that walked the tree once at startup saw
// nothing, and the operator's configured DefaultDERControl (which hangs off
// DERProgram) was unreachable. This asserts the served bytes now carry a
// program on a device that has received no control at all.
func TestGETDERProgramListServesSeededProgramBeforeAnyControl(t *testing.T) {
	t.Parallel()

	baseURL, devices := newMUPTestServer(t, "DERPROGSEED1")
	d := devices[0]

	status, body := getSEP2(t, d, baseURL+"/edev/"+d.edevID+"/fsa/"+controlFSAID+"/derp")
	if status != http.StatusOK {
		t.Fatalf("GET DERProgramList status = %d, want 200\nbody=%s", status, body)
	}

	space, local := rootElement(t, body)
	if space != "urn:ieee:std:2030.5:ns" || local != "DERProgramList" {
		t.Fatalf("root element = {%s}%s, want {urn:ieee:std:2030.5:ns}DERProgramList\nbody=%s", space, local, body)
	}

	// The List counts (sep.xsd:5360) must report the one seeded program. A
	// client is entitled to skip the child walk on all="0", so an
	// understated count is as bad as an absent program.
	if !bytes.Contains(body, []byte(`all="1"`)) {
		t.Errorf("DERProgramList does not carry all=\"1\"; a client may skip an all=\"0\" list entirely\nbody=%s", body)
	}
	if !bytes.Contains(body, []byte(`results="1"`)) {
		t.Errorf("DERProgramList does not carry results=\"1\"\nbody=%s", body)
	}
	if !bytes.Contains(body, []byte("<DERProgram")) {
		t.Fatalf("DERProgramList carries no DERProgram child before any control delta; the seeded program is the whole point of this path\nbody=%s", body)
	}
}

// TestServedDERProgramFollowsSchemaSequenceAndTypes pins the served
// DERProgram element by element against sep.xsd: the child order of the
// schema sequence, mRID's HexBinary128 type, description's String32 bound,
// and primacy being present, an element, and the configured value.
func TestServedDERProgramFollowsSchemaSequenceAndTypes(t *testing.T) {
	t.Parallel()

	baseURL, devices := newMUPTestServer(t, "DERPROGWIRE1")
	d := devices[0]

	status, body := getSEP2(t, d, baseURL+"/edev/"+d.edevID+"/fsa/"+controlFSAID+"/derp")
	if status != http.StatusOK {
		t.Fatalf("GET DERProgramList status = %d, want 200\nbody=%s", status, body)
	}

	// Schema sequence. IdentifiedObject's mRID and description come first,
	// then DERProgram's own links, then primacy last. A document that
	// carries every one of these in the wrong order is well-formed XML and
	// still rejected by a validating client, which is why order is pinned
	// rather than mere presence.
	assertOrder(t, body,
		"<mRID>",
		"<description>",
		"<DefaultDERControlLink",
		"<DERControlListLink",
		"<primacy>",
	)

	// mRID is mRIDType, an extension of HexBinary128 (sep.xsd:5919): exactly
	// 16 bytes, so exactly 32 hexadecimal characters. This is asserted on
	// the served text, not on the Go field, because the wire value is what a
	// client parses as hexBinary and aborts the document over.
	gotMRID := elementText(t, body, "mRID")
	if len(gotMRID) != 32 {
		t.Errorf("served DERProgram mRID = %q: length %d, want 32 (mRIDType is HexBinary128)", gotMRID, len(gotMRID))
	}
	if _, err := hex.DecodeString(gotMRID); err != nil {
		t.Errorf("served DERProgram mRID = %q: not valid hexadecimal (%v)", gotMRID, err)
	}

	// description is String32 (sep.xsd:6319). An over-length value is not
	// truncated by the serializer; it reaches the client, which fails the
	// whole document and loses every sibling field including the links.
	gotDescription := elementText(t, body, "description")
	if len(gotDescription) > 32 {
		t.Errorf("served DERProgram description = %q: %d characters, want at most 32 (String32)", gotDescription, len(gotDescription))
	}
	if gotDescription != testProgramSeed.Description {
		t.Errorf("served DERProgram description = %q, want %q (the configured value)", gotDescription, testProgramSeed.Description)
	}

	// primacy is minOccurs=1 on DERProgram and is a child ELEMENT, not an
	// attribute. Both halves matter: an absent primacy is a schema
	// violation, and one emitted as an attribute is the IEEECORE-103 defect
	// class that a round trip through our own marshaller cannot see.
	gotPrimacy := elementText(t, body, "primacy")
	if gotPrimacy != "1" {
		t.Errorf("served DERProgram primacy = %q, want %q (the configured PrimacyContractedServiceProvider)", gotPrimacy, "1")
	}
	assertNoAttribute(t, body, "primacy")

	// The value is not merely present, it is the configured one. Asserting
	// against the policy rather than a literal is what makes this a test of
	// the config path instead of a restatement of a constant.
	if gotPrimacy != string(rune('0'+testProgramSeed.Primacy)) {
		t.Errorf("served DERProgram primacy = %q, want the configured DefaultProgram.Primacy %d", gotPrimacy, testProgramSeed.Primacy)
	}
}

// TestServedFSAAdvertisesDERProgramListAndIsSchemaValid pins the link a
// client walks to reach the program above, and the two sep.xsd types the
// bridge previously violated on this exact resource: an over-length
// description and a non-hex mRID, either of which made a conformant client
// reject the whole document.
func TestServedFSAAdvertisesDERProgramListAndIsSchemaValid(t *testing.T) {
	t.Parallel()

	baseURL, devices := newMUPTestServer(t, "FSAWIRE1")
	d := devices[0]

	status, body := getSEP2(t, d, baseURL+"/edev/"+d.edevID+"/fsa")
	if status != http.StatusOK {
		t.Fatalf("GET FunctionSetAssignmentsList status = %d, want 200\nbody=%s", status, body)
	}

	space, local := rootElement(t, body)
	if space != "urn:ieee:std:2030.5:ns" || local != "FunctionSetAssignmentsList" {
		t.Fatalf("root element = {%s}%s, want {urn:ieee:std:2030.5:ns}FunctionSetAssignmentsList\nbody=%s", space, local, body)
	}

	// Schema sequence, which for FunctionSetAssignments is the opposite way
	// round from DERProgram and is worth pinning precisely because of that.
	// FunctionSetAssignments extends FunctionSetAssignmentsBase
	// (sep.xsd:237), whose sequence holds the links, and XML Schema
	// complexContent extension appends the DERIVED type's sequence after
	// the base's. So the links come first and mRID/description
	// (sep.xsd:265) come after: the reverse of DERProgram, which extends
	// SubscribableIdentifiedObject and therefore leads with mRID.
	assertOrder(t, body,
		"<DERProgramListLink",
		"<mRID>",
		"<description>",
	)

	gotMRID := elementText(t, body, "mRID")
	if len(gotMRID) != 32 {
		t.Errorf("served FSA mRID = %q: length %d, want 32 (mRIDType is HexBinary128)", gotMRID, len(gotMRID))
	}
	if _, err := hex.DecodeString(gotMRID); err != nil {
		t.Errorf("served FSA mRID = %q: not valid hexadecimal (%v)", gotMRID, err)
	}

	gotDescription := elementText(t, body, "description")
	if len(gotDescription) > 32 {
		t.Errorf("served FSA description = %q: %d characters, want at most 32 (String32); this exact overrun made the reference client reject the document", gotDescription, len(gotDescription))
	}

	// The advertised DERProgramList href must be the path the control path
	// writes under, which is what makes the seeded program reachable.
	wantHref := derProgramListHref(d.edevID, controlFSAID)
	if !bytes.Contains(body, []byte(`href="`+wantHref+`"`)) {
		t.Errorf("served FSA does not advertise DERProgramListLink href=%q\nbody=%s", wantHref, body)
	}

	// The link must also carry its item count. sep.xsd:5385: "This
	// attribute SHALL be present if the href is a local or relative URI",
	// and this href is relative. The Go field is `all,attr,omitempty`, so a
	// zero emits NO attribute rather than all="0"; asserting the exact
	// attribute text is what distinguishes those two, and a struct-level
	// check on All would not.
	if !bytes.Contains(body, []byte(`href="`+wantHref+`" all="1"`)) {
		t.Errorf("served FSA DERProgramListLink does not carry all=\"1\"; sep.xsd requires all on a relative href, and a client may skip the GET on an absent or zero count\nbody=%s", body)
	}
}

// TestServedDefaultDERControlIsReachableFromSeededProgram walks the link
// chain end to end over the wire, which is the only assertion that covers
// what a client actually does:
//
//	EndDevice -> FunctionSetAssignments -> DERProgram -> DefaultDERControl
//
// It is the wire-level counterpart of
// TestDefaultDERControlIsReachableBeforeAnyDeltaApplied: that test asserts
// the store holds the singleton, this one asserts a client can get to it,
// on a device that has received no control delta.
func TestServedDefaultDERControlIsReachableFromSeededProgram(t *testing.T) {
	t.Parallel()

	baseURL, devices := newMUPTestServer(t, "DDERCWALK1")
	d := devices[0]

	status, body := getSEP2(t, d, baseURL+"/edev/"+d.edevID+"/fsa/"+controlFSAID+"/derp")
	if status != http.StatusOK {
		t.Fatalf("GET DERProgramList status = %d, want 200\nbody=%s", status, body)
	}

	wantDDERCHref := "/edev/" + d.edevID + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID + "/dderc"
	if !bytes.Contains(body, []byte(`href="`+wantDDERCHref+`"`)) {
		t.Fatalf("served DERProgram does not advertise DefaultDERControlLink href=%q\nbody=%s", wantDDERCHref, body)
	}

	// Follow it. An advertised link that 404s is the same failure to a
	// client as an absent one.
	ddercStatus, ddercBody := getSEP2(t, d, baseURL+wantDDERCHref)
	if ddercStatus != http.StatusOK {
		t.Fatalf("GET DefaultDERControl status = %d, want 200; the advertised link does not resolve\nbody=%s", ddercStatus, ddercBody)
	}

	space, local := rootElement(t, ddercBody)
	if space != "urn:ieee:std:2030.5:ns" || local != "DefaultDERControl" {
		t.Fatalf("root element = {%s}%s, want {urn:ieee:std:2030.5:ns}DefaultDERControl\nbody=%s", space, local, ddercBody)
	}

	// The served default control carries the operator's configured value,
	// not a zero one. opModConnect true is what testDefaultControlSnapshot
	// configures, and serving false here would be an inverted fail-safe: it
	// would tell every DER to disconnect.
	if got := elementText(t, ddercBody, "opModConnect"); got != "true" {
		t.Errorf("served DefaultDERControl opModConnect = %q, want \"true\" (the configured value)\nbody=%s", got, ddercBody)
	}
}
