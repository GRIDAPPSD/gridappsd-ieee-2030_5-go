package sep2embed

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/store"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
)

// Wire-level tests for supersession.
//
// Everything here asserts the BYTES the embedded server writes to a client,
// never a round trip through this project's own marshaller, for the reason
// spelled out at the top of control_interval_test.go:
// marshal-then-unmarshal is symmetric, so it agrees with itself
// in the correct and the incorrect encoding alike. Two suites in two repos
// stayed green while every served document was unparseable to a
// schema-following client.
//
// The defect these tests exist to prevent was observed live on 2026-08-03. A
// mid-run delta changing opModTargetW from 5000 W to 7500 W was served and
// fetched 22 times and the client never re-actuated, because the event mRID
// was a pure function of (kind, device LFDI) and so constant forever. Per
// 2018 clause 10.2.5.6 p.95 a client "SHALL detect duplicate Events by
// comparing the mRIDs of the Events", so the client was correct and the
// server was not. Nothing in a struct-level test would have failed: the
// stored control did carry 7500.

// derControlElements returns the raw bytes of each <DERControl> element in a
// served document, in document order.
//
// It slices the bytes rather than unmarshalling because these tests assert on
// the served encoding. The scan is exact on both ends: the opening tag is
// accepted only when the character after "<DERControl" is not a name
// character, which excludes "<DERControlBase>" and "<DERControlListLink>",
// and the closing "</DERControl>" carries its own ">" so it cannot match
// "</DERControlBase>".
func derControlElements(t *testing.T, body []byte) [][]byte {
	t.Helper()

	const open = "<DERControl"
	const closed = "</DERControl>"

	var out [][]byte
	rest := body
	for {
		i := bytes.Index(rest, []byte(open))
		if i < 0 {
			return out
		}
		after := rest[i+len(open):]
		if len(after) == 0 {
			return out
		}
		if c := after[0]; c != '>' && c != ' ' && c != '\t' && c != '\r' && c != '\n' && c != '/' {
			// A longer element name that merely starts with DERControl.
			rest = after
			continue
		}
		j := bytes.Index(rest[i:], []byte(closed))
		if j < 0 {
			t.Fatalf("unterminated <DERControl> element in served bytes\nbody=%s", body)
		}
		end := i + j + len(closed)
		out = append(out, rest[i:end])
		rest = rest[end:]
	}
}

// derControlElementWith returns the one served <DERControl> element
// containing needle, failing the test when the count is not exactly one.
//
// Tests name the control they mean by a byte sequence unique to it (its own
// commanded value, say) rather than by list position. Position is not a
// contract: the DERControlList is ordered by store key, and an assertion that
// silently tracked list order would keep passing if supersession started
// marking the wrong one of the two events.
func derControlElementWith(t *testing.T, body []byte, needle string) []byte {
	t.Helper()

	var found [][]byte
	for _, el := range derControlElements(t, body) {
		if bytes.Contains(el, []byte(needle)) {
			found = append(found, el)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d served DERControl elements containing %q, want exactly 1\nbody=%s", len(found), needle, body)
	}
	return found[0]
}

// derControlHrefAttr reads the href off a served <DERControl> element.
//
// href is an ATTRIBUTE on Resource (sep.xsd declares it
// `xs:attribute name="href" type="xs:anyURI"`), not a child element, so it is
// read as one. Using elementText for it would report "element not found" on a
// perfectly correct document.
func derControlHrefAttr(t *testing.T, element []byte) string {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(element))
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("no <DERControl> start element in %s", element)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "DERControl" {
			continue
		}
		for _, a := range se.Attr {
			if a.Name.Local == "href" {
				return a.Value
			}
		}
		t.Fatalf("served <DERControl> carries no href attribute: %s", element)
	}
}

// steppingClock returns a clock that yields base, then base+step, then
// base+2*step, and so on, so successive writes land on distinguishable
// instants without the test sleeping.
func steppingClock(base, step int64) func() time.Time {
	var reads int64
	return func() time.Time {
		at := base + reads*step
		reads++
		return time.Unix(at, 0).UTC()
	}
}

// The exact wire forms of the two EventStatus blocks under the 2018 semantics
// this server presents.
//
// They are written as whole-element literals rather than as separate value
// checks so that the EventStatus sequence (sep.xsd:5598: currentStatus,
// dateTime, potentiallySuperseded, potentiallySupersededTime, reason) is
// pinned along with the values. A document carrying the right values in the
// wrong order is well-formed XML and still rejected by a validating client.
//
// potentiallySuperseded is present and false on both. Under 2018 it flags
// PARTIAL supersession only (Annex B p.160: overlap in "SOME, BUT NOT ALL"
// controls), so a fully superseded event carries status 4 with the flag still
// false. Serving it true here would tell a client the older event is still
// partly in force. potentiallySupersededTime is absent because nothing set
// the flag; 2018 p.161 defines it as "the time that the potentiallySuperseded
// flag was set".
//
// The two instants are controlClockUnix (1767225600) and one 600-second step
// past it (1767226200), written out rather than computed for the reason given
// on controlClockUnix itself: an expectation derived from the code agrees with
// the code by construction and passes whatever the code emits.
const (
	wantActiveStatusAtFirst = `<EventStatus><currentStatus>1</currentStatus><dateTime>1767225600</dateTime>` +
		`<potentiallySuperseded>false</potentiallySuperseded></EventStatus>`
	wantActiveStatusAtSecond = `<EventStatus><currentStatus>1</currentStatus><dateTime>1767226200</dateTime>` +
		`<potentiallySuperseded>false</potentiallySuperseded></EventStatus>`
	wantSupersededStatusAtSecond = `<EventStatus><currentStatus>4</currentStatus><dateTime>1767226200</dateTime>` +
		`<potentiallySuperseded>false</potentiallySuperseded></EventStatus>`
)

// TestSupersedingControlIsANewEventWithANewMRID is the wire-level statement of
// the whole fix.
//
// A setpoint change is served as a SECOND DERControl inside the SAME
// DERProgram, with its own mRID, its own href and a later creationTime, while
// the first control survives byte for byte apart from its status. That shape
// is CTP BASIC-019's (CTP v1.2 printed p.119): two distinct events in one
// program, each with its own response cycle.
//
// The clause chain: 2018 rules q)2) p.91 and t)3) p.92 say a server "SHALL
// NOT edit the original Event but SHALL maintain all Events in their
// entirety"; 2018 clause 10.10.4.2 p.120 says each DERControl instance "SHALL
// be uniquely identified by an mRID"; 2018 clause 10.2.5.6 p.95 says clients
// detect duplicates by comparing mRIDs; and CSIP v2.0 section 4.4.1 lines 282
// to 283 names the remedy, "a new DERControl is issued to supersede or cancel
// the existing DERControl".
func TestSupersedingControlIsANewEventWithANewMRID(t *testing.T) {
	t.Parallel()

	const step = 600
	baseURL, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = steppingClock(controlClockUnix, step)
	}, "DERCSUP2")
	d := devices[0]

	applyTestControlDeltaValue(t, e, reg, "mrid-DERCSUP2", "opModTargetW", 5000)
	applyTestControlDeltaValue(t, e, reg, "mrid-DERCSUP2", "opModTargetW", 7500)

	dercHref := "/edev/" + d.edevID + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID + "/derc"
	status, body := getSEP2(t, d, baseURL+dercHref)
	if status != http.StatusOK {
		t.Fatalf("GET DERControlList status = %d, want 200\nbody=%s", status, body)
	}

	elements := derControlElements(t, body)
	if len(elements) != 2 {
		t.Fatalf("served DERControlList carries %d DERControl elements, want 2; a setpoint change is a NEW event, not an edit of the served one (2018 rules q)2) p.91 and t)3) p.92)\nbody=%s",
			len(elements), body)
	}

	// The two commanded values are BOTH on the wire, each in its own event.
	const wantOld = `<opModTargetW><multiplier>0</multiplier><value>5000</value></opModTargetW>`
	const wantNew = `<opModTargetW><multiplier>0</multiplier><value>7500</value></opModTargetW>`
	superseded := derControlElementWith(t, body, wantOld)
	superseding := derControlElementWith(t, body, wantNew)

	// Distinct mRIDs. This is the assertion the live defect would have
	// failed: one constant mRID per device made the client discard the
	// update as a duplicate of the event it already held.
	oldMRID := elementText(t, superseded, "mRID")
	newMRID := elementText(t, superseding, "mRID")
	if oldMRID == newMRID {
		t.Errorf("both served DERControls carry mRID %s; each instance SHALL be uniquely identified by an mRID (2018 clause 10.10.4.2 p.120), and a client SHALL treat a repeated mRID as a duplicate (10.2.5.6 p.95)", oldMRID)
	}
	assertValidMRID(t, "superseded control mRID", oldMRID)
	assertValidMRID(t, "superseding control mRID", newMRID)

	// Distinct hrefs, both of which resolve. An event maintained "in its
	// entirety" that cannot be fetched is not maintained.
	oldHref := derControlHrefAttr(t, superseded)
	newHref := derControlHrefAttr(t, superseding)
	if oldHref == newHref {
		t.Fatalf("both served DERControls are addressed at %s", oldHref)
	}
	for _, href := range []string{oldHref, newHref} {
		if s, b := getSEP2(t, d, baseURL+href); s != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200; a superseded event stays fetchable through its Effective Scheduled Period (2018 rule r) p.91)\nbody=%s", href, s, b)
		}
	}

	// EventStatus, byte for byte, on each event.
	//
	// The superseded event's dateTime is the SUPERSEDING event's Effective
	// Start Time, which 2018 Annex B p.160 names exactly: the server SHALL
	// mark the event Superseded "at the earliest Effective Start Time of the
	// overlapping event". It is not the instant the older event was created,
	// and it is not left at that instant either.
	if !bytes.Contains(superseded, []byte(wantSupersededStatusAtSecond)) {
		t.Errorf("superseded DERControl does not carry %s\nelement=%s", wantSupersededStatusAtSecond, superseded)
	}
	if !bytes.Contains(superseding, []byte(wantActiveStatusAtSecond)) {
		t.Errorf("superseding DERControl does not carry %s\nelement=%s", wantActiveStatusAtSecond, superseding)
	}

	// The superseded event is otherwise untouched: same commanded value,
	// same creationTime, same interval. Status is the ONLY modification 2018
	// rule c) p.90 permits on a served Event.
	if got, want := elementInt64(t, superseded, "creationTime"), int64(controlClockUnix); got != want {
		t.Errorf("superseded control creationTime = %d, want %d unchanged", got, want)
	}
	if got, want := elementInt64(t, superseded, "start"), int64(controlClockUnix); got != want {
		t.Errorf("superseded control interval start = %d, want %d unchanged", got, want)
	}
	if got, want := elementInt64(t, superseding, "creationTime"), int64(controlClockUnix+step); got != want {
		t.Errorf("superseding control creationTime = %d, want %d; the larger creationTime is what makes it newer (2018 rule f) p.90)", got, want)
	}
}

// TestDifferingControlModesDoNotSupersede is rule t)'s negative case, and it
// is the assertion that keeps the fix from over-reaching.
//
// 2018 rule t) p.91: "For DERControls, differing controls (e.g.,
// opModTargetVar, opModTargetW) within DERControl Events are independent and
// are allowed to overlap or nest without superseding." So a delta on
// opModTargetVar must NOT mark an active opModTargetW control superseded,
// even though the two events are in the same program, overlap in time, and
// the second is strictly newer. Every one of the conditions that DOES trigger
// supersession is present here except the control set, which is the point.
//
// CTP BASIC-024 through BASIC-026 (CTP v1.2 printed pp.130 to 138) test the
// client half: two overlapping INDEPENDENT DERControls, and the client
// executes BOTH with no status 7 expected.
//
// Note the 2018 edition contradicts itself here. Clause 10.10.4.2 p.120 says
// a DER client is managed by "a single DERControl Event, which SHALL supersede
// any previous Event", which taken literally would make this test wrong. Rule
// t) governs: it is the DER-specific carve-out written for exactly this case,
// 2023 REMOVED the p.120 sentence (2023 clause 10.10.4.2.1 p.129 has no such
// statement), and the CTP sides with rule t).
func TestDifferingControlModesDoNotSupersede(t *testing.T) {
	t.Parallel()

	const step = 600
	baseURL, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = steppingClock(controlClockUnix, step)
	}, "DERCSUP3")
	d := devices[0]

	applyTestControlDeltaValue(t, e, reg, "mrid-DERCSUP3", "opModTargetW", 5000)
	applyTestControlDeltaValue(t, e, reg, "mrid-DERCSUP3", "opModTargetVar", 250)

	status, body := getSEP2(t, d, baseURL+"/edev/"+d.edevID+"/fsa/"+controlFSAID+"/derp/"+controlDERProgramID+"/derc")
	if status != http.StatusOK {
		t.Fatalf("GET DERControlList status = %d, want 200\nbody=%s", status, body)
	}

	elements := derControlElements(t, body)
	if len(elements) != 2 {
		t.Fatalf("served DERControlList carries %d DERControl elements, want 2 (one per independent control mode)\nbody=%s", len(elements), body)
	}

	// The strongest single assertion: the status a server sets ONLY on
	// supersession appears nowhere in the document.
	if bytes.Contains(body, []byte("<currentStatus>4</currentStatus>")) {
		t.Errorf("a served DERControl carries currentStatus 4 (Superseded) after a delta on a DIFFERENT control mode; 2018 rule t) p.91 makes differing controls independent and allows them to overlap without superseding\nbody=%s", body)
	}

	// And the positive form, per event, so the test cannot be satisfied by a
	// change that drops EventStatus altogether.
	watt := derControlElementWith(t, body, `<opModTargetW><multiplier>0</multiplier><value>5000</value></opModTargetW>`)
	vars := derControlElementWith(t, body, `<opModTargetVar><multiplier>0</multiplier><value>250</value></opModTargetVar>`)

	if !bytes.Contains(watt, []byte(wantActiveStatusAtFirst)) {
		t.Errorf("the earlier opModTargetW control does not carry %s; it must remain Active when an independent mode is issued alongside it\nelement=%s",
			wantActiveStatusAtFirst, watt)
	}
	if !bytes.Contains(vars, []byte(wantActiveStatusAtSecond)) {
		t.Errorf("the opModTargetVar control does not carry %s\nelement=%s", wantActiveStatusAtSecond, vars)
	}

	// potentiallySuperseded stays false on both. It is the PARTIAL
	// supersession flag under 2018 rules q)3) p.91 and t)4) p.92, and a
	// disjoint mode set is not a partial overlap: setting it here would tell
	// a client to go hunting for an event that supersedes part of this one.
	if bytes.Contains(body, []byte("<potentiallySuperseded>true</potentiallySuperseded>")) {
		t.Errorf("a served DERControl carries potentiallySuperseded true after a delta on a disjoint control mode; the flag means overlap in SOME BUT NOT ALL controls (2018 Annex B p.160)\nbody=%s", body)
	}
	if bytes.Contains(body, []byte("<potentiallySupersededTime>")) {
		t.Errorf("a served DERControl carries potentiallySupersededTime with the flag false; it records the instant the flag was set (2018 p.161)\nbody=%s", body)
	}
}

// TestIssuedControlIdentityIsDeterministicAcrossRestart is the proof that the
// property the old derivation existed for is preserved rather than traded
// away.
//
// The old mRID was a pure function of (kind, device LFDI), which gave
// deterministic identity across restarts with no persisted state. That single
// line was also the supersession defect: both properties came from it. The
// resolution is to widen the derivation's INPUTS, not to abandon derivation.
// An mRID from a random source or read back from storage would fix
// supersession and lose restart determinism silently, and nothing about the
// served document would look wrong afterwards.
//
// A restart, for a bridge that persists nothing, is a second run against a
// freshly built store with the same registry and the same clock. The test
// does exactly that and requires every issued identity to match.
//
// It works at the store rather than over a real server deliberately.
// newEmbedTestServer mints a development device certificate into a fresh
// temporary directory, so two servers built from the same serial hold
// different keys and therefore different certificate-derived LFDIs. A test
// built on that would fail here for a reason that has nothing to do with the
// mRID derivation, and would keep failing after any correct fix.
func TestIssuedControlIdentityIsDeterministicAcrossRestart(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	const step = 600

	run := func() []sep2.DERControl {
		t.Helper()
		reg, st := twoDeviceFixture(t)
		policy := testControlPolicy
		policy.Control.Now = steppingClock(controlClockUnix, step)

		for _, v := range []float64{5000, 7500} {
			err := ApplyControlDelta(ctx, st, nil, reg, policy, diff.Difference{
				Object:    "mrid-a",
				Attribute: "DERControl.DERControlBase.opModTargetW",
				Value:     map[string]any{"multiplier": 0.0, "value": v},
			})
			if err != nil {
				t.Fatalf("ApplyControlDelta(%v): %v", v, err)
			}
		}

		scope := derControlScope(urlIndexFor(t, st, "mrid-a"), controlFSAID, controlDERProgramID)
		list, err := st.DERControls.List(ctx, scope, store.ListOptions{Unbounded: true})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list.Items) != 2 {
			t.Fatalf("run issued %d DERControls, want 2", len(list.Items))
		}
		return list.Items
	}

	first := run()
	second := run()

	for i := range first {
		if second[i].MRID != first[i].MRID {
			t.Errorf("DERControl %d mRID after restart = %s, want %s; deterministic identity across restarts with no persisted state is the property the derivation exists to keep",
				i, second[i].MRID, first[i].MRID)
		}
		if second[i].Href != first[i].Href {
			t.Errorf("DERControl %d href after restart = %s, want %s", i, second[i].Href, first[i].Href)
		}
	}
}

// TestDeriveEventMRIDIsAFunctionOfItsInputs states the derivation contract
// directly, one input at a time.
//
// The end-to-end restart test above proves the property holds through the
// whole write path; this one localises it, so a regression names the input
// that broke rather than reporting that two documents differ somewhere.
//
// The negative half matters as much as the positive: an mRID that did not
// move with the payload would reintroduce the exact defect this function fixes,
// and an mRID that did not move with creationTime would collide whenever the
// same value is re-commanded.
func TestDeriveEventMRIDIsAFunctionOfItsInputs(t *testing.T) {
	t.Parallel()

	const lfdi = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	watt := func(v int16) *sep2.DERControlBase {
		return &sep2.DERControlBase{OpModTargetW: &sep2.ActivePower{Multiplier: 0, Value: v}}
	}

	derive := func(kind, lfdi string, at int64, base *sep2.DERControlBase) string {
		t.Helper()
		got, err := deriveEventMRID(kind, lfdi, at, base)
		if err != nil {
			t.Fatalf("deriveEventMRID: %v", err)
		}
		assertValidMRID(t, "deriveEventMRID", got)
		return got
	}

	ref := derive(mridKindDERControl, lfdi, controlClockUnix, watt(5000))

	// Determinism: the same inputs, twice, in the same process.
	if again := derive(mridKindDERControl, lfdi, controlClockUnix, watt(5000)); again != ref {
		t.Errorf("deriveEventMRID is not deterministic: %s then %s", ref, again)
	}

	for _, tc := range []struct {
		name string
		got  string
	}{
		{"a different commanded value", derive(mridKindDERControl, lfdi, controlClockUnix, watt(7500))},
		{"a different creationTime", derive(mridKindDERControl, lfdi, controlClockUnix+1, watt(5000))},
		{"a different control mode", derive(mridKindDERControl, lfdi, controlClockUnix,
			&sep2.DERControlBase{OpModTargetVar: &sep2.ReactivePower{Multiplier: 0, Value: 5000}})},
		{"a different device", derive(mridKindDERControl, "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", controlClockUnix, watt(5000))},
		{"a different kind", derive(mridKindDERProgram, lfdi, controlClockUnix, watt(5000))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got == ref {
				t.Errorf("deriveEventMRID with %s yields the same mRID %s; each DERControl instance SHALL be uniquely identified by an mRID (2018 clause 10.10.4.2 p.120)", tc.name, ref)
			}
		})
	}

	// A nil base is representable rather than a panic: it is what a control
	// commanding nothing would carry, and refusing to mint an identity for it
	// would turn a degenerate control into a crash on the write path.
	assertValidMRID(t, "deriveEventMRID(nil base)", derive(mridKindDERControl, lfdi, controlClockUnix, nil))
}

// TestNewestControlIsServedOnTheFirstUnpagedPage guards the paging hazard the
// per-event id scheme introduced, and it is a defect of exactly the shape
// the descending-time id ordering exists to remove.
//
// Core lists a scoped collection in ascending id byte order
// (store.SortByIDAsc) and defaults an unpaged GET to ten items
// (sep2srv/paging.DefaultLimit). Once each control delta issues its own
// DERControl, a device accumulates more than ten of them well inside one
// interval. Had the id been the mRID alone, the list order would be a hash
// order, and the newest control, the whole reason the delta was issued, could
// land on page two and never be fetched by a client that does not page. The
// client would then hold a stale setpoint while the server believed it had
// issued a new one, which is the observed defect arriving by a different
// route.
//
// derControlID therefore leads with the creation instant in descending order.
// That is our choice, not the standard's: section 4.6.1 requires a defined
// list order but names none for DERControlList.
func TestNewestControlIsServedOnTheFirstUnpagedPage(t *testing.T) {
	t.Parallel()

	const (
		step   = 60
		deltas = 12 // strictly more than paging.DefaultLimit
	)
	baseURL, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = steppingClock(controlClockUnix, step)
		// Long enough that every control is still inside its own interval,
		// so none of them is eligible for any future end-of-life handling.
		cfg.DERControl.Duration = 86400
	}, "DERCPAGE1")
	d := devices[0]

	var lastValue float64
	for i := range deltas {
		lastValue = float64(1000 + i*100)
		applyTestControlDeltaValue(t, e, reg, "mrid-DERCPAGE1", "opModTargetW", lastValue)
	}

	status, body := getSEP2(t, d, baseURL+"/edev/"+d.edevID+"/fsa/"+controlFSAID+"/derp/"+controlDERProgramID+"/derc")
	if status != http.StatusOK {
		t.Fatalf("GET DERControlList status = %d, want 200\nbody=%s", status, body)
	}

	// The unpaged page is a page: this test would be vacuous if core served
	// the whole collection.
	if n := len(derControlElements(t, body)); n >= deltas {
		t.Fatalf("unpaged GET returned %d of %d DERControls; this test assumes core pages an unpaged GET (paging.DefaultLimit), and it no longer does",
			n, deltas)
	}

	newest := fmt.Sprintf(`<opModTargetW><multiplier>0</multiplier><value>%d</value></opModTargetW>`, int(lastValue))
	if !bytes.Contains(body, []byte(newest)) {
		t.Errorf("the newest control (%s) is absent from the first unpaged page of the DERControlList; a client that does not page would never see the setpoint change\nbody=%s",
			newest, body)
	}

	// And it is FIRST, not merely present: newest-first is the ordering
	// derControlID encodes, and "somewhere on the page" would still degrade
	// as the collection grows.
	if first := derControlElements(t, body)[0]; !bytes.Contains(first, []byte(newest)) {
		t.Errorf("the first served DERControl is not the newest one\nfirst=%s", first)
	}
}

// TestIssuedDERControlHrefEndsWithItsStoreKey pins the invariant
// supersedePriorControls and the admin-UI snapshot both rely on: the id a
// control is stored under is derControlID(creationTime, mRID), and its href
// is the list href plus that id.
//
// Both call sites recompute the key from the record rather than parsing it
// out of the href, because store.ListResult returns items without their keys.
// That recomputation is only sound while the two agree, so the agreement is
// asserted here rather than assumed. If it ever broke, supersedePriorControls
// would fail its Update with ErrNotFound and a superseded event would keep
// serving currentStatus 1.
func TestIssuedDERControlHrefEndsWithItsStoreKey(t *testing.T) {
	t.Parallel()

	reg, st := twoDeviceFixture(t)
	ctx := t.Context()

	applyTestStoreDelta(t, ctx, st, reg, "opModTargetW", 5000)

	edevA := urlIndexFor(t, st, "mrid-a")
	scope := derControlScope(edevA, controlFSAID, controlDERProgramID)
	control, id := soleControl(t, ctx, st, scope)

	if _, err := st.DERControls.Get(ctx, scope, id); err != nil {
		t.Fatalf("DERControls.Get(%q): %v; the recomputed key must address the stored record", id, err)
	}
	wantHref := derControlListHref(edevA, controlFSAID, controlDERProgramID) + "/" + id
	if control.Href != wantHref {
		t.Errorf("issued DERControl.Href = %q, want %q (the list href plus the store key)", control.Href, wantHref)
	}
}
