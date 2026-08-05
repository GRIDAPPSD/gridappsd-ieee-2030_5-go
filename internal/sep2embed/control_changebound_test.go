package sep2embed

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// Wire-level tests for the CHANGE bound on the delta-application path.
//
// WHAT THEY EXIST TO CATCH. Every delta mints its own event, and a
// separate time bound bounds the resulting collection in TIME. Neither
// bounded it in SIZE, because nothing compared an incoming delta against the control already
// in force: a platform restating an unchanged setpoint every timestep minted
// one event per timestep. The store's own ErrAlreadyExists dedup could not
// catch it, because the creation instant is an input to both the mRID and the
// store id, so a restatement one second later derives a different id and the
// write proceeds. At the shipped 1800-second duration and one delta per second
// that is 1800 resident controls per device, each delta paying an unbounded
// List plus an O(n) classify: quadratic across the window. The time bound was
// doing its job; the delta rate had never been sized.
//
// Everything here asserts the BYTES the embedded server writes to a client,
// for the reason given at the top of control_interval_test.go: a round trip
// through this project's own marshaller is symmetric and agrees with itself in
// the correct and the incorrect encoding alike.
//
// The sibling bound on the same code path, the ceiling on the same-second
// creationTime tie-break, is in control_timebound_test.go.

// dercListHref is the DERControlList route for a device, which is both what a
// client polls and what every assertion below reads.
func dercListHref(edevID string) string {
	return "/edev/" + edevID + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID + "/derc"
}

// servedDERControlList GETs a device's DERControlList and returns the served
// bytes, failing the test on any status but 200.
func servedDERControlList(t *testing.T, d mupTestDevice, baseURL string) []byte {
	t.Helper()
	status, body := getSEP2(t, d, baseURL+dercListHref(d.edevID))
	if status != http.StatusOK {
		t.Fatalf("GET DERControlList status = %d, want 200\nbody=%s", status, body)
	}
	return body
}

// targetWElement is the served wire form of one opModTargetW setpoint, used to
// name the event a test means by its own commanded value rather than by list
// position.
func targetWElement(value int) string {
	return fmt.Sprintf(`<opModTargetW><multiplier>0</multiplier><value>%d</value></opModTargetW>`, value)
}

// TestRestatedControlDeltaDoesNotMintASecondEvent is the wire-level
// statement of the change bound.
//
// A platform that restates the SAME setpoint every timestep is the likely
// production cadence, and every restatement used to mint its own DERControl.
// The time bound is a bound in time, not in size: at one delta per
// second against the shipped 1800-second window it caps the collection at 1800
// resident controls per device rather than at one.
//
// The remedy is a change bound. A delta that does not change the commanded
// value is not a new Event in any sense the standard recognises: 2018 clause
// 10.2.5.6 p.95 has the client discard a duplicate, and CSIP v2.0 section
// 4.4.1 lines 282 to 283 scopes a new DERControl to changing the setting. So
// the server issues nothing.
func TestRestatedControlDeltaDoesNotMintASecondEvent(t *testing.T) {
	t.Parallel()

	const restatements = 25

	clock := newMovableClock(controlClockUnix)
	baseURL, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = clock.now
	}, "DERCRATE1")
	d := devices[0]

	// One delta per second, every one carrying the setpoint already in force.
	for i := range restatements {
		clock.set(controlClockUnix + int64(i))
		applyTestControlDeltaValue(t, e, reg, "mrid-DERCRATE1", "opModTargetW", 5000)
	}

	body := servedDERControlList(t, d, baseURL)
	if n := len(derControlElements(t, body)); n != 1 {
		t.Fatalf("%d restatements of one unchanged setpoint produced %d served DERControl elements, want 1; a delta that changes nothing is not a new Event\nbody=%s",
			restatements, n, body)
	}
	// The list's own counters, which is what a client reads before it fetches
	// anything: a server that hid the extra events from the element scan but
	// still held them would report them here.
	for _, want := range []string{`all="1"`, `results="1"`} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("served DERControlList does not carry %s after %d restatements\nbody=%s", want, restatements, body)
		}
	}

	// The surviving event is the FIRST one, unchanged. A server that replaced
	// the event on every restatement would also serve exactly one, and would
	// still be re-issuing an identity the client has to re-fetch and re-ack.
	el := derControlElementWith(t, body, targetWElement(5000))
	if got := elementInt64(t, el, "creationTime"); got != controlClockUnix {
		t.Errorf("served control creationTime = %d, want %d: the event issued by the FIRST delta, not one re-minted by a later restatement", got, controlClockUnix)
	}
	if !bytes.Contains(el, []byte(wantIntervalElement)) {
		t.Errorf("served control does not carry %s; its window is the one the first delta opened\nelement=%s", wantIntervalElement, el)
	}
	if !bytes.Contains(el, []byte(wantActiveStatusAtFirst)) {
		t.Errorf("served control does not carry %s\nelement=%s", wantActiveStatusAtFirst, el)
	}

	// Exactly one control resident in the store, which is the size half of the
	// finding: the served list and the collection behind it agree.
	soleServedControlID(t, e, d.edevID)
}

// TestChangedSetpointStillMintsAfterRestatements is the guard that keeps
// the change bound from swallowing a real change.
//
// The sequence is the production one: a setpoint restated, then changed, then
// restated again. The change MUST still issue its own event and supersede the
// one before it, and the restatements on either side of it must
// still mint nothing.
func TestChangedSetpointStillMintsAfterRestatements(t *testing.T) {
	t.Parallel()

	clock := newMovableClock(controlClockUnix)
	baseURL, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = clock.now
	}, "DERCRATE2")
	d := devices[0]

	for i, value := range []float64{5000, 5000, 5000, 7500, 7500} {
		clock.set(controlClockUnix + int64(i))
		applyTestControlDeltaValue(t, e, reg, "mrid-DERCRATE2", "opModTargetW", value)
	}

	body := servedDERControlList(t, d, baseURL)
	if n := len(derControlElements(t, body)); n != 2 {
		t.Fatalf("five deltas carrying two distinct setpoints produced %d served DERControl elements, want 2\nbody=%s", n, body)
	}

	superseded := derControlElementWith(t, body, targetWElement(5000))
	superseding := derControlElementWith(t, body, targetWElement(7500))

	// The change was issued at the fourth delta's instant, not the first
	// restatement's and not the last.
	const changedAt = controlClockUnix + 3
	if got := elementInt64(t, superseding, "creationTime"); got != changedAt {
		t.Errorf("changed control creationTime = %d, want %d (the instant the value actually changed)", got, changedAt)
	}
	if got := elementInt64(t, superseded, "creationTime"); got != controlClockUnix {
		t.Errorf("superseded control creationTime = %d, want %d unchanged", got, controlClockUnix)
	}

	// Supersession still lands, and at the superseding event's Effective Start
	// Time (2018 Annex B p.160).
	wantSuperseded := fmt.Sprintf(`<EventStatus><currentStatus>4</currentStatus><dateTime>%d</dateTime>`+
		`<potentiallySuperseded>false</potentiallySuperseded></EventStatus>`, changedAt)
	if !bytes.Contains(superseded, []byte(wantSuperseded)) {
		t.Errorf("superseded DERControl does not carry %s\nelement=%s", wantSuperseded, superseded)
	}
	if got := elementInt64(t, superseding, "currentStatus"); got != int64(sep2.EventStatusActive) {
		t.Errorf("changed control currentStatus = %d, want 1 (Active)", got)
	}
}

// TestRestatedSetpointMintsAgainOnceItsControlHasEnded pins the change bound's
// own boundary: it is scoped to the control still IN FORCE, not to every
// control the device has ever been issued.
//
// A restatement arriving after the previous control's maximum Effective
// Scheduled Period has closed is not redundant. The device has already
// reverted to the DefaultDERControl, so suppressing the new event would leave
// the platform's standing setpoint uncommanded for as long as it kept
// restating it: the change bound would have become a permanent one.
func TestRestatedSetpointMintsAgainOnceItsControlHasEnded(t *testing.T) {
	t.Parallel()

	clock := newMovableClock(controlClockUnix)
	baseURL, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = clock.now
	}, "DERCRATE3")
	d := devices[0]

	applyTestControlDeltaValue(t, e, reg, "mrid-DERCRATE3", "opModTargetW", 5000)

	// The instant the first control's window closes. Embed.ApplyControlDelta
	// sweeps before it writes, so the ended control is out of service by the
	// time the restatement is judged.
	const reopenedAt = controlClockUnix + 1800
	clock.set(reopenedAt)
	applyTestControlDeltaValue(t, e, reg, "mrid-DERCRATE3", "opModTargetW", 5000)

	body := servedDERControlList(t, d, baseURL)
	if n := len(derControlElements(t, body)); n != 1 {
		t.Fatalf("served DERControlList carries %d DERControl elements after the first control ended and the setpoint was restated, want 1\nbody=%s", n, body)
	}
	el := derControlElementWith(t, body, targetWElement(5000))
	if got := elementInt64(t, el, "creationTime"); got != reopenedAt {
		t.Errorf("served control creationTime = %d, want %d: a restatement after the previous control ended is a NEW event, because the device has already reverted", got, reopenedAt)
	}
	if got := elementInt64(t, el, "start"); got != reopenedAt {
		t.Errorf("served control interval start = %d, want %d", got, reopenedAt)
	}
}
