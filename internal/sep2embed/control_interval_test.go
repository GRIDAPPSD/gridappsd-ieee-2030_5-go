package sep2embed

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// The tests in this file pin the TEMPORAL shape of a served DERControl:
// creationTime, interval, and randomizeDuration. They assert on the bytes the
// embedded server actually writes to a client, never on a round trip through
// this project's own marshaller, for the reason spelled out at the top of
// derprogram_conformance_test.go and demonstrated by IEEECORE-103: marshal
// then unmarshal is symmetric, so it agrees with itself in the correct and
// the incorrect encoding alike. Two suites in two repos stayed green while
// every served document was unparseable to a schema-following client.
//
// The defect these tests exist to prevent is the one observed live on
// 2026-08-03. The bridge served DERControls with no interval at all. The
// reference client computed end = interval.start + interval.duration = 0,
// found end <= now on arrival, and marked the event expired: it fetched the
// control, parsed it, POSTed a conformant DERControlResponse, and discarded
// it. Every observable signal short of the device itself reported success,
// and the control output file was zero bytes. Nothing in a struct-level test
// would have failed.
//
// Schema authority is sep.xsd (IEEE 2030.5-2018, Model Build 20180301). The
// sequence pinned below is the concatenation of DERControl's base types:
//
//	RespondableResource (sep.xsd:5430): replyTo, responseRequired (ATTRIBUTES)
//	IdentifiedObject    (sep.xsd:5324): mRID, description, version
//	Event               (sep.xsd:5571): creationTime (min 1), EventStatus
//	                                    (min 1), interval (min 1)
//	RandomizableEvent   (sep.xsd:5643): randomizeDuration, randomizeStart
//	DERControl          (sep.xsd:3890): DERControlBase (min 1), deviceCategory
//
// interval itself is a DateTimeInterval (sep.xsd:5780) whose own sequence is
// duration then start, both minOccurs=1.

// controlClockUnix is the fixed instant every test in this file reads its
// clock from, and the wire forms below are that same value as it must appear
// on the wire.
//
// Both are written as literals rather than one being derived from the other.
// A derived expectation would agree with the code by construction and pass
// whatever the code emitted, which is precisely the class of test that let
// the absent-interval defect ship. TimeType is seconds since the Unix epoch
// (sep.xsd:6382), so the wire form is the decimal seconds value with no
// formatting of any kind.
const (
	controlClockUnix int64 = 1767225600
	// The duration and randomization the harness serves, and their wire
	// forms. testControlSeed.Duration is 1800.
	wantIntervalElement     = `<interval><duration>1800</duration><start>1767225600</start></interval>`
	wantCreationTimeElement = `<creationTime>1767225600</creationTime>`
	wantRandomizeElement    = `<randomizeDuration>0</randomizeDuration>`
)

// fixedControlClock returns a clock function pinned to controlClockUnix.
func fixedControlClock() func() time.Time {
	return func() time.Time { return time.Unix(controlClockUnix, 0).UTC() }
}

// applyTestControlDelta drives one opModTargetW delta through the real DOWN
// path for the given device, so what the tests below read off the wire is
// what ApplyControlDelta actually produces.
func applyTestControlDelta(t *testing.T, e *Embed, reg *registry.Registry, mrid string) {
	t.Helper()
	if err := e.ApplyControlDelta(context.Background(), reg, diff.Difference{
		Object:    mrid,
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0.0, "value": 5000.0},
	}); err != nil {
		t.Fatalf("ApplyControlDelta(%s): %v", mrid, err)
	}
}

// TestServedDERControlCarriesIntervalAndCreationTime is the wire-level
// statement of the whole fix: the exact bytes a client receives for the two
// elements sep.xsd makes mandatory on Event, in the exact schema sequence,
// as elements rather than attributes.
func TestServedDERControlCarriesIntervalAndCreationTime(t *testing.T) {
	t.Parallel()

	baseURL, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = fixedControlClock()
	}, "DERCINTERVAL1")
	d := devices[0]

	applyTestControlDelta(t, e, reg, "mrid-DERCINTERVAL1")

	dercHref := "/edev/" + d.edevID + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID + "/derc"
	// Both routes, because a client may read either and a stamp applied on
	// one path only would be invisible on the other.
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"list route", baseURL + dercHref},
		{"single-resource route", baseURL + dercHref + "/" + activeControlID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := getSEP2(t, d, tc.url)
			if status != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200\nbody=%s", tc.url, status, body)
			}

			// The elements themselves, byte for byte. interval is asserted as
			// a whole rather than as two separate children so that the
			// DateTimeInterval sequence (duration before start, sep.xsd:5785)
			// is pinned along with the values: a document carrying both
			// children in the wrong order is well-formed XML and still
			// rejected by a validating client.
			if !bytes.Contains(body, []byte(wantCreationTimeElement)) {
				t.Errorf("served DERControl does not carry %s; creationTime is minOccurs=1 (sep.xsd:5578) and is the sole tiebreaker for supersession within equal primacy\nbody=%s",
					wantCreationTimeElement, body)
			}
			if !bytes.Contains(body, []byte(wantIntervalElement)) {
				t.Errorf("served DERControl does not carry %s; interval is minOccurs=1 (sep.xsd:5584) and without it a client computes end = 0 and expires the event on arrival\nbody=%s",
					wantIntervalElement, body)
			}

			// Schema sequence across the whole inheritance chain.
			assertOrder(t, body,
				"<mRID>",
				"<creationTime>",
				"<EventStatus>",
				"<interval>",
				"<randomizeDuration>",
				"<DERControlBase>",
			)

			// ELEMENTS, not attributes. This is the IEEECORE-103 defect class
			// in the direction that applies here, and it is checked per field
			// against the XSD rather than inferred from a neighbour: on this
			// same type replyTo and responseRequired genuinely ARE attributes
			// (sep.xsd:5435 and :5440), so "what the sibling does" is exactly
			// the wrong guide.
			assertNoAttribute(t, body, "creationTime")
			assertNoAttribute(t, body, "interval")
			assertNoAttribute(t, body, "duration")
			assertNoAttribute(t, body, "start")
			assertNoAttribute(t, body, "randomizeDuration")

			// The two that ARE attributes, asserted positively, so this test
			// cannot be satisfied by a change that moves everything to
			// elements.
			if !bytes.Contains(body, []byte(`responseRequired="`)) {
				t.Errorf("served DERControl does not carry responseRequired as an ATTRIBUTE; sep.xsd:5440 declares it one\nbody=%s", body)
			}
			if !bytes.Contains(body, []byte(`replyTo="`)) {
				t.Errorf("served DERControl does not carry replyTo as an ATTRIBUTE; sep.xsd:5435 declares it one\nbody=%s", body)
			}
		})
	}
}

// TestServedDERControlIntervalEndsInTheFuture states the defect's mechanism
// arithmetically rather than by matching literals: whatever the values are,
// the end of the window must be strictly after its start, and after the
// instant the control was created.
//
// This is deliberately redundant with the byte assertions above and is not
// duplication for its own sake. The literal test pins today's exact
// configuration; this one holds for any configuration and would fail on a
// future duration change that reintroduced a zero-length window, which is
// the property a client actually evaluates (end <= now means expired).
func TestServedDERControlIntervalEndsInTheFuture(t *testing.T) {
	t.Parallel()

	baseURL, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = fixedControlClock()
	}, "DERCINTERVAL2")
	d := devices[0]

	applyTestControlDelta(t, e, reg, "mrid-DERCINTERVAL2")

	status, body := getSEP2(t, d, baseURL+"/edev/"+d.edevID+"/fsa/"+controlFSAID+"/derp/"+controlDERProgramID+"/derc")
	if status != http.StatusOK {
		t.Fatalf("GET DERControlList status = %d, want 200\nbody=%s", status, body)
	}

	start := elementInt64(t, body, "start")
	duration := elementInt64(t, body, "duration")
	creationTime := elementInt64(t, body, "creationTime")

	if duration <= 0 {
		t.Fatalf("served interval duration = %d, want > 0; the reference client computes end = start + duration and expires any event whose end is already past", duration)
	}
	if start != controlClockUnix {
		t.Errorf("served interval start = %d, want %d (the instant the control was created)", start, controlClockUnix)
	}
	if creationTime != controlClockUnix {
		t.Errorf("served creationTime = %d, want %d", creationTime, controlClockUnix)
	}
	if end := start + duration; end <= controlClockUnix {
		t.Errorf("served interval end = %d, which is not after the creation instant %d; a client marks such an event expired on arrival without ever actuating",
			end, controlClockUnix)
	}
}

// TestServedDERControlHonorsConfiguredIntervalPolicy proves the served
// temporal values come from operator policy rather than from constants
// compiled into the write path. The values chosen are deliberately unlike
// the defaults, and the randomization is negative, because a sign dropped
// somewhere in the plumbing would otherwise be invisible.
func TestServedDERControlHonorsConfiguredIntervalPolicy(t *testing.T) {
	t.Parallel()

	baseURL, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl = DERControlSeed{
			Duration:          97,
			RandomizeDuration: -31,
			Now:               fixedControlClock(),
		}
	}, "DERCINTERVAL3")
	d := devices[0]

	applyTestControlDelta(t, e, reg, "mrid-DERCINTERVAL3")

	status, body := getSEP2(t, d, baseURL+"/edev/"+d.edevID+"/fsa/"+controlFSAID+"/derp/"+controlDERProgramID+"/derc")
	if status != http.StatusOK {
		t.Fatalf("GET DERControlList status = %d, want 200\nbody=%s", status, body)
	}

	const wantInterval = `<interval><duration>97</duration><start>1767225600</start></interval>`
	if !bytes.Contains(body, []byte(wantInterval)) {
		t.Errorf("served DERControl does not carry the configured %s\nbody=%s", wantInterval, body)
	}
	const wantRandomize = `<randomizeDuration>-31</randomizeDuration>`
	if !bytes.Contains(body, []byte(wantRandomize)) {
		t.Errorf("served DERControl does not carry the configured %s\nbody=%s", wantRandomize, body)
	}
}

// TestServedDERControlAlwaysCarriesRandomizeDuration pins the decision to
// emit randomizeDuration explicitly even at its default value of zero.
//
// The field is a pointer whose nil marshals as an absent element, and
// sep.xsd:5650 says an absent element defaults to 0, so absent and
// present-as-zero are semantically identical to a conformant client. Serving
// it removes the dependence on the client applying that default, and it makes
// the configured policy visible in a packet capture, which is what an
// operator debugging a fleet that reverts in lockstep will be reading.
func TestServedDERControlAlwaysCarriesRandomizeDuration(t *testing.T) {
	t.Parallel()

	baseURL, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = fixedControlClock()
	}, "DERCINTERVAL4")
	d := devices[0]

	applyTestControlDelta(t, e, reg, "mrid-DERCINTERVAL4")

	status, body := getSEP2(t, d, baseURL+"/edev/"+d.edevID+"/fsa/"+controlFSAID+"/derp/"+controlDERProgramID+"/derc")
	if status != http.StatusOK {
		t.Fatalf("GET DERControlList status = %d, want 200\nbody=%s", status, body)
	}
	if !bytes.Contains(body, []byte(wantRandomizeElement)) {
		t.Errorf("served DERControl does not carry %s; the configured zero is stated on the wire rather than left to the client's schema default\nbody=%s",
			wantRandomizeElement, body)
	}
}

// TestSupersedingDeltaRefreshesCreationTimeAndIntervalStart covers the
// supersede path, which is the one that would silently keep a stale value.
//
// Both halves matter and for different reasons. A stale creationTime makes
// the new control rank no newer than the one the client already holds: the
// reference client compares with a strict greater-than, so equal values
// compare false in both directions and the INCOMING control is discarded,
// leaving a server unable to replace a setpoint it has already issued. A
// stale interval start means a window opened by an earlier delta expires
// while the platform is still actively commanding, dropping the device onto
// its DefaultDERControl mid-dispatch.
func TestSupersedingDeltaRefreshesCreationTimeAndIntervalStart(t *testing.T) {
	t.Parallel()

	// A clock that advances by a fixed step on each read, so the two writes
	// land on distinguishable instants without the test sleeping.
	const secondInstant = controlClockUnix + 600
	var reads int
	baseURL, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = func() time.Time {
			reads++
			if reads == 1 {
				return time.Unix(controlClockUnix, 0).UTC()
			}
			return time.Unix(secondInstant, 0).UTC()
		}
	}, "DERCSUPERSEDE1")
	d := devices[0]

	applyTestControlDelta(t, e, reg, "mrid-DERCSUPERSEDE1")
	// A second, different field so the merge path is exercised rather than a
	// rewrite of the same value.
	if err := e.ApplyControlDelta(context.Background(), reg, diff.Difference{
		Object:    "mrid-DERCSUPERSEDE1",
		Attribute: "DERControl.DERControlBase.opModTargetVar",
		Value:     map[string]any{"multiplier": 0.0, "value": 250.0},
	}); err != nil {
		t.Fatalf("ApplyControlDelta (superseding): %v", err)
	}

	status, body := getSEP2(t, d, baseURL+"/edev/"+d.edevID+"/fsa/"+controlFSAID+"/derp/"+controlDERProgramID+"/derc")
	if status != http.StatusOK {
		t.Fatalf("GET DERControlList status = %d, want 200\nbody=%s", status, body)
	}

	if got := elementInt64(t, body, "creationTime"); got != secondInstant {
		t.Errorf("served creationTime after supersede = %d, want %d (the second write's instant); "+
			"a stale value leaves the control no newer than the one the client already holds, and a strict-greater comparison then discards the incoming one",
			got, secondInstant)
	}
	if got := elementInt64(t, body, "start"); got != secondInstant {
		t.Errorf("served interval start after supersede = %d, want %d (the second write's instant); "+
			"a stale start lets a window opened by an earlier delta expire while the platform is still commanding",
			got, secondInstant)
	}

	// The merge itself still happened: superseding must not drop the field
	// the first delta set.
	if !bytes.Contains(body, []byte("<opModTargetW>")) {
		t.Errorf("served DERControl lost opModTargetW across the supersede; the merge must preserve previously-set fields\nbody=%s", body)
	}
	if !bytes.Contains(body, []byte("<opModTargetVar>")) {
		t.Errorf("served DERControl does not carry the superseding opModTargetVar\nbody=%s", body)
	}
}

// TestApplyControlDeltaRefusesUnconfiguredDuration pins the fail-closed
// behavior at the write site.
//
// Refusing is the only safe answer, and the alternative is worse than it
// looks: writing the control anyway produces an interval whose end equals its
// start, and the client then fetches it, parses it, and POSTs a conformant
// response before discarding it. The whole protocol exchange reports success
// while nothing actuates, so an invented fallback duration would hide the
// misconfiguration behind a working-looking system.
func TestApplyControlDeltaRefusesUnconfiguredDuration(t *testing.T) {
	t.Parallel()

	reg, st := twoDeviceFixture(t)
	ctx := context.Background()

	policy := testControlPolicy
	policy.Control.Duration = 0

	err := ApplyControlDelta(ctx, st, nil, reg, policy, diff.Difference{
		Object:    "mrid-a",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0.0, "value": 1000.0},
	})
	if !errors.Is(err, ErrDERControlDurationUnset) {
		t.Fatalf("ApplyControlDelta with a zero duration = %v, want ErrDERControlDurationUnset", err)
	}

	// Nothing was written. The refusal happens before any resource is
	// created, so a misconfigured bridge does not leave a half-built control
	// tree behind for the next delta to trip over.
	edevA := urlIndexFor(t, st, "mrid-a")
	scope := derControlScope(edevA, controlFSAID, controlDERProgramID)
	if _, getErr := st.DERControls.Get(ctx, scope, activeControlID); getErr == nil {
		t.Error("a DERControl was written despite the refusal; the guard must run before any store write")
	}
}

// elementInt64 reads a named element's text out of served bytes and parses it
// as the decimal integer TimeType and UInt32 both are on the wire.
//
// It goes through elementText, which walks the token stream, rather than
// unmarshalling into a struct: a struct field would accept the value carried
// as an attribute or at the wrong nesting depth, which is the defect class
// these tests exist to catch.
func elementInt64(t *testing.T, body []byte, local string) int64 {
	t.Helper()
	raw := elementText(t, body, local)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("element %q = %q, which does not parse as a decimal integer: %v\nbody=%s", local, raw, err, body)
	}
	return v
}
