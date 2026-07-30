package sep2embed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/assembly"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// GAGO-094 served-bytes tests for the two defects Devi measured against the
// reference client:
//
// Defect 1 (HIGH): the DERProgram was created lazily on the first control
// delta, so a client that walked /edev/{lfdi}/fsa/1/derp before any delta
// arrived read all="0". Because that list advertises pollRate="900" the
// client honored it and did not re-walk, missing every control for up to 15
// minutes. Reproduced deterministically: two runs with the client started
// before the delta both read all="0" at T+1s and never actuated.
//
// Defect 2 (MEDIUM): successive deltas rewrote the setpoint in place under a
// stable mRID and href, so a client that had already actuated had no wire
// signal to actuate again.
//
// These tests drive the REAL router (buildHandler) and assert the served
// bytes, not the store contents, because the store cannot see either
// symptom: a record present in the store but reported by a list as all="0"
// is exactly the failure being pinned, and an mRID that is correct in memory
// but identical across two documents is the other one.

// serveSeedOnly seeds one device and returns an authenticated GET over the
// real router WITHOUT applying any control delta.
//
// The absence of the delta is the entire point: this is the ordering Devi's
// failing runs used (client first, delta later), and it is the ordering the
// old lazy-creation code failed under. Its return also exposes the stores and
// registry so a caller can inject deltas mid-test and re-read the same
// handler, which is what the two-delta sequence below needs.
func serveSeedOnly(t *testing.T) (func(t *testing.T, path string) (int, string), *assembly.Stores, *registry.Registry) {
	t.Helper()

	reg := registry.New()
	if err := reg.AddBatch([]registry.Entry{
		{MRID: "mrid-inv-1", Name: "Inverter 1", LFDI: discoveryWireLFDI, Placeholder: true},
	}); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	connect := true
	energize := true
	policy := seedPolicy{
		DefaultControl: sep2.DefaultDERControl{
			DERControlBase: &sep2.DERControlBase{
				OpModConnect:  &connect,
				OpModEnergize: &energize,
			},
		},
	}

	ctx := context.Background()
	stores := newStores()
	pin := uint32(111115)
	pollRate := uint32(300)
	policy.RegistrationPIN = &pin
	policy.RegistrationPollRate = &pollRate
	if err := seedStores(ctx, stores, reg, policy); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	h := buildHandler(assembly.RouterConfig{}, stores, reg,
		sep2srv.Identity{LFDI: "SERVER-LFDI", SFDI: "1"}, nil, telemetryConfig{}, nil)

	get := func(t *testing.T, path string) (int, string) {
		t.Helper()
		req := withIdentity(httptest.NewRequest(http.MethodGet, path, nil), discoveryWireLFDI, "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	return get, stores, reg
}

// TestServedDERProgramListIsNonEmptyBeforeAnyDelta is the direct regression
// pin for Defect 1. The measured symptom was GET
// /edev/{lfdi}/fsa/1/derp returning all="0" when no delta had been applied
// yet, which ended the client's walk for a full pollRate period.
//
// The assertion is on the list's own all and results attributes, not on a
// child count, because that is what the client reads: EPRI's poll_derpl
// branches on the list attributes and its der_program hook (which is what
// arms the fast active_poll_rate on the DERControlList underneath) only runs
// for a DERProgram that was actually parsed out of the list. An empty list
// therefore does not merely delay the derc poll, it never establishes it.
func TestServedDERProgramListIsNonEmptyBeforeAnyDelta(t *testing.T) {
	t.Parallel()

	get, _, _ := serveSeedOnly(t)
	path := derProgramListHref(discoveryWireLFDI, controlFSAID)

	code, body := get(t, path)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, code, body)
	}

	if strings.Contains(body, `all="0"`) {
		t.Errorf("DERProgramList serves all=\"0\" before any delta; the client honors pollRate=\"900\" and misses every control for up to 15 minutes:\n%s", body)
	}
	if !strings.Contains(body, `all="1"`) || !strings.Contains(body, `results="1"`) {
		t.Errorf("DERProgramList must serve all=\"1\" results=\"1\" from seed time:\n%s", body)
	}
	// A list whose attributes claim a result but whose body inlines nothing
	// is the same dead end one level down.
	if !strings.Contains(body, "<DERProgram ") {
		t.Errorf("DERProgramList claims a result but inlines no DERProgram:\n%s", body)
	}
	if !strings.Contains(body, `<DERControlListLink href="`+path+`/`+controlDERProgramID+`/derc"`) {
		t.Errorf("inlined DERProgram has no DERControlListLink; the walk cannot reach any control:\n%s", body)
	}
	if !strings.Contains(body, `<DefaultDERControlLink href="`+path+`/`+controlDERProgramID+`/dderc"`) {
		t.Errorf("inlined DERProgram has no DefaultDERControlLink:\n%s", body)
	}
	// primacy and mRID are the DERProgram's two required children (sep.xsd:
	// everything else on the type, including all four ListLinks, is
	// minOccurs="0"). Both must be present or a schema-driven parser
	// discards the whole document, which would leave the client exactly
	// where the empty list left it.
	if !strings.Contains(body, "<primacy>") {
		t.Errorf("inlined DERProgram has no <primacy>, a required element:\n%s", body)
	}
	mrids := extractElements(body, "mRID")
	if len(mrids) == 0 {
		t.Fatalf("inlined DERProgram has no <mRID>, a required element:\n%s", body)
	}
	for _, m := range mrids {
		if !isMRIDLegal(m) {
			t.Errorf("seeded DERProgram mRID %q is not a legal mRIDType; a schema-driven client discards the whole document", m)
		}
	}
}

// TestServedDERControlListIsEmptyBeforeAnyDelta asserts the boundary that
// makes seed-time program creation safe rather than a new defect.
//
// Seeding must create the program and its DefaultDERControl, and must NOT
// fabricate a DERControl. An empty DERControlList under a real DERProgram is
// legitimate on the wire (sep.xsd declares DERControlList's DERControl child
// minOccurs="0" maxOccurs="unbounded") and is semantically true: the program
// exists and currently commands nothing. A placeholder control would instead
// command the device with a setpoint no operator issued, which is a worse
// defect than the one being fixed.
func TestServedDERControlListIsEmptyBeforeAnyDelta(t *testing.T) {
	t.Parallel()

	get, _, _ := serveSeedOnly(t)
	path := derProgramListHref(discoveryWireLFDI, controlFSAID) + "/" + controlDERProgramID + "/derc"

	code, body := get(t, path)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (an empty list is served, not 404): %s", path, code, body)
	}
	if !strings.Contains(body, `all="0"`) {
		t.Errorf("DERControlList before any delta must serve all=\"0\"; anything else advertises a command that was never issued:\n%s", body)
	}
	if strings.Contains(body, "<DERControl ") || strings.Contains(body, "<DERControl>") {
		t.Errorf("DERControlList inlines a DERControl before any delta was applied; nothing may command the device:\n%s", body)
	}
	if !strings.Contains(body, `xmlns="`+sep2.Namespace+`"`) {
		t.Errorf("empty DERControlList served without the 2030.5 namespace:\n%s", body)
	}
}

// TestServedDefaultDERControlIsReachableBeforeAnyDelta walks the link a
// client follows off the seeded DERProgram. A DefaultDERControlLink that
// resolves to nothing is the CSIP hole; it must resolve from seed time, with
// the configured policy value rather than an invented one.
func TestServedDefaultDERControlIsReachableBeforeAnyDelta(t *testing.T) {
	t.Parallel()

	get, _, _ := serveSeedOnly(t)
	path := derProgramListHref(discoveryWireLFDI, controlFSAID) + "/" + controlDERProgramID + "/dderc"

	code, body := get(t, path)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, code, body)
	}
	if !strings.Contains(body, "<opModConnect>true</opModConnect>") {
		t.Errorf("served DefaultDERControl lacks the configured opModConnect=true:\n%s", body)
	}
	if !strings.Contains(body, "<opModEnergize>true</opModEnergize>") {
		t.Errorf("served DefaultDERControl lacks the configured opModEnergize=true:\n%s", body)
	}
	// The negative invariant: a target setpoint on the DEFAULT control would
	// curtail the device (opModTargetW) or disable its autonomous volt-var
	// behavior (opModTargetVar) whenever no event is active.
	if strings.Contains(body, "<opModTargetW>") {
		t.Errorf("served DefaultDERControl carries opModTargetW; a default must not command a setpoint:\n%s", body)
	}
	if strings.Contains(body, "<opModTargetVar>") {
		t.Errorf("served DefaultDERControl carries opModTargetVar; this disables autonomous volt-var per IEEE 1547-2018 clause 5.3:\n%s", body)
	}
}

// derControlIdentity is one served DERControl's wire identity: the three
// values a client uses to decide whether a document describes an event it
// has already acted on.
type derControlIdentity struct {
	mRID         string
	href         string
	creationTime int64
	targetW      string
}

// readServedControl parses the identity out of the served DERControlList.
//
// It scans the bytes rather than decoding into sep2.DERControl on purpose:
// these assertions are about what a foreign parser sees, and round-tripping
// through Go's own decoder would hide a serialization-level defect (an
// element emitted out of XSD sequence order, or dropped entirely) that is
// precisely the class being checked.
func readServedControl(t *testing.T, get func(t *testing.T, path string) (int, string)) derControlIdentity {
	t.Helper()

	path := derProgramListHref(discoveryWireLFDI, controlFSAID) + "/" + controlDERProgramID + "/derc"
	code, body := get(t, path)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, code, body)
	}
	if !strings.Contains(body, `all="1"`) || !strings.Contains(body, `results="1"`) {
		t.Fatalf("DERControlList must serve exactly one control (all=\"1\" results=\"1\"); two overlapping controls is a worse failure than a late one:\n%s", body)
	}
	if n := strings.Count(body, "<DERControl "); n != 1 {
		t.Fatalf("DERControlList inlines %d DERControl elements, want exactly 1:\n%s", n, body)
	}

	got := derControlIdentity{}

	mrids := extractElements(body, "mRID")
	if len(mrids) != 1 {
		t.Fatalf("served DERControlList carries %d mRID elements, want 1:\n%s", len(mrids), body)
	}
	got.mRID = mrids[0]
	if !isMRIDLegal(got.mRID) {
		t.Fatalf("served DERControl mRID %q is not a legal mRIDType (hexBinary, at most %d hex chars); a schema-driven client discards the whole document",
			got.mRID, mridHexChars)
	}

	creations := extractElements(body, "creationTime")
	if len(creations) != 1 {
		t.Fatalf("served DERControlList carries %d creationTime elements, want 1 (it is minOccurs=1 on Event):\n%s", len(creations), body)
	}
	parsed, err := strconv.ParseInt(creations[0], 10, 64)
	if err != nil {
		t.Fatalf("served creationTime %q does not parse as an integer: %v", creations[0], err)
	}
	got.creationTime = parsed

	got.href = memberHref(t, body)

	targets := extractElements(body, "value")
	if len(targets) == 0 {
		t.Fatalf("served DERControl carries no opModTargetW value:\n%s", body)
	}
	got.targetW = targets[0]

	return got
}

// memberHref returns the href attribute of the single inlined <DERControl>
// element in a served DERControlList.
//
// It locates the element's open tag first and reads href from inside it,
// rather than searching for the literal `<DERControl href="`, because core
// emits the child's xmlns attribute ahead of href
// (`<DERControl xmlns="..." href="...">`). Matching on attribute order would
// make this helper silently report "no href" on a document that has one,
// which is the same class of false negative the whole file exists to avoid.
func memberHref(t *testing.T, body string) string {
	t.Helper()

	const openTag = "<DERControl "
	i := strings.Index(body, openTag)
	if i < 0 {
		t.Fatalf("served list inlines no <DERControl> element:\n%s", body)
	}
	rest := body[i+len(openTag):]
	end := strings.Index(rest, ">")
	if end < 0 {
		t.Fatalf("served <DERControl> open tag is unterminated:\n%s", body)
	}
	attrs := rest[:end]

	const hrefAttr = `href="`
	j := strings.Index(attrs, hrefAttr)
	if j < 0 {
		t.Fatalf("served DERControl carries no href attribute (attributes: %q):\n%s", attrs, body)
	}
	value := attrs[j+len(hrefAttr):]
	k := strings.Index(value, `"`)
	if k < 0 {
		t.Fatalf("served DERControl href attribute is unterminated:\n%s", body)
	}
	return value[:k]
}

// applyTargetW injects one opModTargetW delta through the real DOWN path.
func applyTargetW(t *testing.T, stores *assembly.Stores, reg *registry.Registry, watts int16) {
	t.Helper()

	err := ApplyControlDelta(context.Background(), stores, nil, reg, sep2.DefaultDERControl{}, ControlDelta{
		Object:    "mrid-inv-1",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     sep2.ActivePower{Multiplier: 0, Value: watts},
	})
	if err != nil {
		t.Fatalf("ApplyControlDelta(%d W): %v", watts, err)
	}
}

// TestSuccessiveDeltasServeDistinctEventIdentities is the direct regression
// pin for Defect 2.
//
// The measured symptom: successive deltas rewrote opModTargetW while mRID
// stayed 14C3D593EEB3E4C741BCA4F4210ADDEA and the interval start stayed at
// the original instant, so a client that had already actuated had no wire
// signal to actuate again. Three independent mechanisms in the reference
// client make that invisible, which is why all three of mRID, href, and
// creationTime are asserted here rather than just one:
//
//   - mRID is the event's identity: schedule_event short-circuits on
//     hash_get(s->blocks, ev->mRID), so a known mRID creates no new event
//     block, and activate_block will not re-fire EVENT_START (the hook that
//     pushes the setpoint to the inverter) on an already-Active block.
//   - update_existing goes further: for an event whose mRID compares equal it
//     copies ONLY the EventStatus off the incoming object and frees the rest,
//     so the changed setpoint is discarded at parse time.
//   - href is how a client tracks list MEMBERSHIP (list_object keys each
//     member by href; dep_complete then subtracts the vanished hrefs and
//     fires RESOURCE_REMOVE, which is what frees the retired event block). A
//     reused href gives the client a member that never disappears, so the old
//     block is never freed and the client holds two.
//
// creationTime carries the fourth requirement: a client breaks an
// equal-primacy tie with the STRICT comparison
// x->creationTime > y->creationTime, and every control this bridge issues has
// primacy 1. A fresh mRID with a non-advancing creationTime would trade a
// silently-ignored update for a silently-rejected one, so the advance is
// asserted, not just the presence of the element.
//
// The magnitudes are deliberately distinct and non-round so a served value
// cannot be confused with a default, a placeholder, or the other delta.
func TestSuccessiveDeltasServeDistinctEventIdentities(t *testing.T) {
	t.Parallel()

	get, stores, reg := serveSeedOnly(t)

	applyTargetW(t, stores, reg, 5813)
	first := readServedControl(t, get)
	if first.targetW != "5813" {
		t.Fatalf("served opModTargetW value = %q, want %q", first.targetW, "5813")
	}

	applyTargetW(t, stores, reg, 3167)
	second := readServedControl(t, get)
	if second.targetW != "3167" {
		t.Errorf("served opModTargetW value = %q, want the replacing delta's %q; the setpoint was not updated at all", second.targetW, "3167")
	}

	if second.mRID == first.mRID {
		t.Errorf("both generations served mRID %q; the client's scheduler short-circuits on a known mRID and never re-actuates", second.mRID)
	}
	if second.href == first.href {
		t.Errorf("both generations served href %q; the client never sees the old list member vanish, so it holds two event blocks", second.href)
	}
	if second.creationTime <= first.creationTime {
		t.Errorf("creationTime did not advance: first=%d second=%d; a client breaks an equal-primacy tie with a STRICT comparison, so the replacement loses and is discarded as Superseded",
			second.creationTime, first.creationTime)
	}

	// A third generation, so the property is not an artifact of the
	// first-write path being different from the replacement path.
	applyTargetW(t, stores, reg, 7429)
	third := readServedControl(t, get)
	if third.targetW != "7429" {
		t.Errorf("served opModTargetW value = %q, want %q", third.targetW, "7429")
	}
	if third.mRID == second.mRID || third.mRID == first.mRID {
		t.Errorf("third generation reused an earlier mRID %q", third.mRID)
	}
	if third.href == second.href || third.href == first.href {
		t.Errorf("third generation reused an earlier href %q", third.href)
	}
	if third.creationTime <= second.creationTime {
		t.Errorf("creationTime did not advance on the third generation: second=%d third=%d", second.creationTime, third.creationTime)
	}
}

// TestSuccessiveDeltasNeverServeTwoOverlappingControls asserts the invariant
// that makes replacement safe: exactly ONE control is served at any instant,
// no matter how many deltas have been applied.
//
// Two simultaneously-active controls over the same interval for the same
// device is a WORSE failure than a late one, because correct behavior would
// then depend on the client resolving supersession between them. Retiring the
// prior generation rather than leaving a superseded twin in the list removes
// that dependency entirely, and this test is what keeps it removed.
func TestSuccessiveDeltasNeverServeTwoOverlappingControls(t *testing.T) {
	t.Parallel()

	get, stores, reg := serveSeedOnly(t)
	path := derProgramListHref(discoveryWireLFDI, controlFSAID) + "/" + controlDERProgramID + "/derc"

	for _, watts := range []int16{6841, 2593, 9127, 4376, 8052} {
		applyTargetW(t, stores, reg, watts)

		code, body := get(t, path)
		if code != http.StatusOK {
			t.Fatalf("GET %s after %d W = %d: %s", path, watts, code, body)
		}
		if n := strings.Count(body, "<DERControl "); n != 1 {
			t.Fatalf("after %d W the list inlines %d DERControl elements, want exactly 1:\n%s", watts, n, body)
		}
		if !strings.Contains(body, `all="1"`) {
			t.Errorf("after %d W the list does not serve all=\"1\":\n%s", watts, body)
		}
		want := "<value>" + strconv.FormatInt(int64(watts), 10) + "</value>"
		if !strings.Contains(body, want) {
			t.Errorf("after %d W the served list does not carry %s:\n%s", watts, want, body)
		}
		// No retired generation may linger with a superseded marker: this
		// bridge retires by removal, so a currentStatus of 4 (Superseded)
		// should never appear on the wire at all.
		if strings.Contains(body, "<currentStatus>4</currentStatus>") {
			t.Errorf("after %d W the list carries a Superseded control; retirement is by removal, not by marker:\n%s", watts, body)
		}
		if !strings.Contains(body, "<currentStatus>1</currentStatus>") {
			t.Errorf("after %d W the served control is not Active:\n%s", watts, body)
		}
	}
}

// TestReplacementCarriesForwardOtherOpModeFields asserts that replacing the
// event identity does not clear op-mode fields an earlier delta set.
//
// DERControlBase is a bag of independent fields and opModTargetW and
// opModTargetVar are legitimately active together, so a delta naming one must
// not erase the other. This is the invariant most at risk from the
// replacement design: constructing a fresh DERControl per generation makes it
// easy to construct a fresh DERControlBase too and silently drop half the
// command.
func TestReplacementCarriesForwardOtherOpModeFields(t *testing.T) {
	t.Parallel()

	get, stores, reg := serveSeedOnly(t)
	ctx := context.Background()

	applyTargetW(t, stores, reg, 5813)

	err := ApplyControlDelta(ctx, stores, nil, reg, sep2.DefaultDERControl{}, ControlDelta{
		Object:    "mrid-inv-1",
		Attribute: "DERControl.DERControlBase.opModTargetVar",
		Value:     sep2.ReactivePower{Multiplier: 0, Value: 1483},
	})
	if err != nil {
		t.Fatalf("ApplyControlDelta(opModTargetVar): %v", err)
	}

	path := derProgramListHref(discoveryWireLFDI, controlFSAID) + "/" + controlDERProgramID + "/derc"
	code, body := get(t, path)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, code, body)
	}

	if !strings.Contains(body, "<opModTargetW>") {
		t.Errorf("the opModTargetVar delta erased opModTargetW from the served control:\n%s", body)
	}
	if !strings.Contains(body, "<value>5813</value>") {
		t.Errorf("served control lost the earlier opModTargetW value 5813:\n%s", body)
	}
	if !strings.Contains(body, "<opModTargetVar>") {
		t.Errorf("served control is missing opModTargetVar:\n%s", body)
	}
	if !strings.Contains(body, "<value>1483</value>") {
		t.Errorf("served control lost the opModTargetVar value 1483:\n%s", body)
	}
}

// TestServedControlGenerationMRIDsAreWireLegal asserts the FORMAT, not just
// the distinctness, of every generation's mRID.
//
// Distinctness alone is not enough: the whole reason the per-generation mRID
// is HASHED rather than formatted with the generation appended is that
// appending would grow the string past the 16-octet mRIDType ceiling and add
// non-hex characters. The reference client types mRID as
// xs_type(XS_HEX_BINARY,16) and wraps every simple-value parse in a macro
// that returns NULL on failure, so one malformed mRID discards the entire
// document, not one field. A generation counter is exactly the kind of input
// that silently changes an identifier's length as it grows, so the format is
// re-asserted at each generation rather than once.
func TestServedControlGenerationMRIDsAreWireLegal(t *testing.T) {
	t.Parallel()

	get, stores, reg := serveSeedOnly(t)

	seen := make(map[string]int16)
	for _, watts := range []int16{6841, 2593, 9127, 4376, 8052, 1738} {
		applyTargetW(t, stores, reg, watts)
		got := readServedControl(t, get)

		if len(got.mRID) != mridHexChars {
			t.Errorf("generation for %d W served mRID %q of length %d, want exactly %d hex characters",
				watts, got.mRID, len(got.mRID), mridHexChars)
		}
		if got.mRID != strings.ToUpper(got.mRID) {
			t.Errorf("generation for %d W served lowercase mRID %q, want uppercase hex", watts, got.mRID)
		}
		if !isMRIDLegal(got.mRID) {
			t.Errorf("generation for %d W served mRID %q, which is not a legal mRIDType", watts, got.mRID)
		}
		if prior, dup := seen[got.mRID]; dup {
			t.Errorf("generation for %d W reused the mRID %q already served for %d W", watts, got.mRID, prior)
		}
		seen[got.mRID] = watts
	}
}

// TestServedControlHrefEncodesAdvancingGeneration asserts the href's
// generation suffix advances monotonically from 0.
//
// The generation is recovered from the STORED record's href rather than kept
// in a package-level counter (see controlGenerationOf for why: a counter is
// process state that can drift from the store and re-issue a live
// generation), which makes the href the authoritative carrier of that
// number. If the suffix ever failed to advance, the mRID derived from it
// would repeat and Defect 2 would return silently, so the suffix itself is
// pinned here.
func TestServedControlHrefEncodesAdvancingGeneration(t *testing.T) {
	t.Parallel()

	get, stores, reg := serveSeedOnly(t)
	prefix := derProgramListHref(discoveryWireLFDI, controlFSAID) + "/" + controlDERProgramID + "/derc/" + activeControlID + "-"

	for want, watts := range []int16{6841, 2593, 9127} {
		applyTargetW(t, stores, reg, watts)
		got := readServedControl(t, get)

		wantHref := prefix + strconv.Itoa(want)
		if got.href != wantHref {
			t.Errorf("generation %d served href %q, want %q", want, got.href, wantHref)
		}
	}
}

// controlWithHref returns a DERControl carrying only href, which is the sole
// field controlGenerationOf reads. Href is set through the promoted embedded
// field rather than a composite struct literal so this helper does not
// hard-code the embedding chain (Resource -> ... -> RandomizableEvent) and
// therefore does not need editing when core reshapes that hierarchy.
func controlWithHref(href string) sep2.DERControl {
	var ctrl sep2.DERControl
	ctrl.Href = href
	return ctrl
}

// TestControlGenerationOfRoundTripsControlHref pins the pairing between the
// two halves of the generation scheme: controlHref writes the number into the
// href, and controlGenerationOf reads it back. The generation lives in the
// stored record rather than in a package-level counter (see
// controlGenerationOf for why), so this round trip is load-bearing: if it
// broke, every replacement would restart at generation 0 and re-issue an mRID
// a client has already scheduled, silently reproducing Defect 2.
func TestControlGenerationOfRoundTripsControlHref(t *testing.T) {
	t.Parallel()

	const lfdi = "AAAA00000000000000000000000000000000AAAA"
	for _, want := range []uint64{0, 1, 2, 9, 10, 99, 1000, 18446744073709551615} {
		href := controlHref(lfdi, controlFSAID, controlDERProgramID, want)
		got := controlGenerationOf(controlWithHref(href))
		if got != want {
			t.Errorf("controlGenerationOf(controlHref(gen=%d)) = %d, want %d (href was %q)", want, got, want, href)
		}
	}
}

// TestControlGenerationOfFallsBackToZeroOnAMalformedHref documents the
// fallback deliberately.
//
// Zero is the same value a brand-new control uses, so the worst case is one
// generation number reused rather than a hard failure on a record this
// process did not write. Forward progress does not depend on this function
// alone: the creationTime advance (nextControlCreationTime) is what guarantees
// a replacement still wins a client's equal-primacy comparison even when the
// generation repeats. The cases below are the shapes a foreign or corrupted
// record could actually take.
func TestControlGenerationOfFallsBackToZeroOnAMalformedHref(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		href string
	}{
		{"empty href", ""},
		{"no derc segment at all", "/edev/AAAA/fsa/1/derp/1"},
		{"legacy href with no generation suffix", "/edev/AAAA/fsa/1/derp/1/derc/" + activeControlID},
		{"non-numeric suffix", "/edev/AAAA/fsa/1/derp/1/derc/" + activeControlID + "-abc"},
		{"empty suffix", "/edev/AAAA/fsa/1/derp/1/derc/" + activeControlID + "-"},
		{"negative suffix", "/edev/AAAA/fsa/1/derp/1/derc/" + activeControlID + "--3"},
		{"suffix overflows uint64", "/edev/AAAA/fsa/1/derp/1/derc/" + activeControlID + "-99999999999999999999999999"},
		{"different store key", "/edev/AAAA/fsa/1/derp/1/derc/other-4"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := controlGenerationOf(controlWithHref(tc.href))
			if got != 0 {
				t.Errorf("controlGenerationOf(%q) = %d, want 0", tc.href, got)
			}
		})
	}
}

// TestNextControlCreationTimeAlwaysAdvances is the guard on the value a
// client's supersession comparison actually reads.
//
// A client breaks an equal-primacy tie with the STRICT comparison
// x->creationTime > y->creationTime, and every control this bridge issues has
// primacy 1, so an equal or lower creationTime on a replacement means the
// replacement LOSES and is discarded as Superseded: the new setpoint is
// silently dropped. TimeType is second-resolution on the wire, so two deltas
// inside one wall-clock second (an ordinary platform burst) hit that case with
// no sub-second value to fall back on, and a backward clock step hits it for
// longer.
func TestNextControlCreationTimeAlwaysAdvances(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		prior int64
		now   int64
		want  int64
	}{
		{"clock advanced normally", 1785419217, 1785419280, 1785419280},
		{"same wall-clock second", 1785419217, 1785419217, 1785419218},
		{"clock stepped backward", 1785419217, 1785419200, 1785419218},
		{"clock stepped far backward", 1785419217, 0, 1785419218},
		{"first write with no prior", 0, 1785419217, 1785419217},
		{"advanced by exactly one second", 1785419217, 1785419218, 1785419218},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := nextControlCreationTime(tc.prior, tc.now)
			if got != tc.want {
				t.Errorf("nextControlCreationTime(prior=%d, now=%d) = %d, want %d", tc.prior, tc.now, got, tc.want)
			}
			if got <= tc.prior && tc.prior != 0 {
				t.Errorf("nextControlCreationTime(prior=%d, now=%d) = %d, which does not exceed prior; the replacement would lose the client's strict comparison",
					tc.prior, tc.now, got)
			}
		})
	}
}

// TestNextControlCreationTimeAdvancesAcrossABurst confirms the clamp composes
// over a run of same-second calls rather than only over a single pair: a burst
// of five deltas inside one second must produce five strictly increasing
// values, not one advance and four ties.
func TestNextControlCreationTimeAdvancesAcrossABurst(t *testing.T) {
	t.Parallel()

	const frozenNow int64 = 1785419217
	prior := frozenNow
	for i := 0; i < 5; i++ {
		got := nextControlCreationTime(prior, frozenNow)
		if got <= prior {
			t.Fatalf("burst step %d: nextControlCreationTime(prior=%d, now=%d) = %d, want strictly greater than prior",
				i, prior, frozenNow, got)
		}
		prior = got
	}
}
