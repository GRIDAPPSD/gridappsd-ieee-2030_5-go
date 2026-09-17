package sep2embed

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	coresub "github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/handlers/subscription"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
)

// Wire-level tests for the end of an issued DERControl's life.
//
// Everything here asserts the BYTES the embedded server writes to a client,
// never a round trip through this project's own marshaller, for the reason
// spelled out at the top of control_interval_test.go:
// marshal-then-unmarshal is symmetric, so it agrees with itself
// in the correct and the incorrect encoding alike.
//
// The defect these tests exist to prevent was observed live on 2026-08-03,
// run 12. After the event's interval elapsed, GET on the DERControl list
// still returned currentStatus 1 (Active) with the now-past interval. A
// client that was already attached handled it correctly, so the defect is
// invisible from that vantage point; the exposure is a client that connects
// LATER, is REQUIRED to discard the event (2018 rule l) p.90, Specified End
// Time in the past) and POSTs status 254 (Table 27 p.76). That is the third
// distinct route to a status-254 rejection found in two days, after an absent
// interval and a wrong wire encoding.

// movableClock is a test clock whose instant the test moves explicitly.
//
// It is atomic because the sweep the tests drive also runs on Embed.Run's own
// timer goroutine, so the clock genuinely has two readers.
type movableClock struct{ at atomic.Int64 }

func newMovableClock(unix int64) *movableClock {
	c := &movableClock{}
	c.at.Store(unix)
	return c
}

func (c *movableClock) now() time.Time { return time.Unix(c.at.Load(), 0).UTC() }

func (c *movableClock) set(unix int64) { c.at.Store(unix) }

// sweep runs one lifecycle sweep at the Embed's current clock instant and
// returns how many controls it removed.
func sweep(t *testing.T, e *Embed) int {
	t.Helper()
	removed, err := e.expireEndedControls(context.Background())
	if err != nil {
		t.Fatalf("expireEndedControls: %v", err)
	}
	return removed
}

// postResponse POSTs a Response naming subject at the replyTo URI core stamps
// onto every served DERControl, and returns the status and served bytes.
//
// The document is written as literal XML rather than marshalled from
// sep2.Response for the same reason the assertions read bytes: a marshalled
// document would agree with this project's own encoder by construction, and
// what is being tested is that the SERVER accepts what a client sends.
func postResponse(t *testing.T, d mupTestDevice, baseURL, subject, lfdi string, status uint8) (int, []byte) {
	t.Helper()

	doc := `<Response xmlns="urn:ieee:std:2030.5:ns">` +
		`<endDeviceLFDI>` + lfdi + `</endDeviceLFDI>` +
		`<status>` + strconv.FormatUint(uint64(status), 10) + `</status>` +
		`<subject>` + subject + `</subject>` +
		`</Response>`

	req, err := http.NewRequest(http.MethodPost, baseURL+"/rsps/1/rsp", strings.NewReader(doc))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/sep+xml")

	resp, err := d.client.Do(req)
	if err != nil {
		t.Fatalf("POST /rsps/1/rsp: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read POST /rsps/1/rsp body: %v", err)
	}
	return resp.StatusCode, body
}

// TestEndedDERControlLeavesServiceAtItsMaxEffectiveScheduledEnd is the
// wire-level statement of the whole fix, asserted at both states of the
// lifecycle against the same served routes.
//
// Before the maximum Effective Scheduled Period closes the event is served,
// Active, with its interval. After it closes the event is gone from the list
// and its own href 404s. Both halves are asserted, because either alone is
// satisfiable by the wrong code: a server that never served the event would
// pass the second half, and the pre-fix server passed the first.
//
// Removal rather than a status restatement is the choice recorded in
// lifecycle.go, and it is the only single behavior conformant under all three
// editions: 2013 and 2018 have no enumeration value meaning ended (2018 Annex
// B p.160, sep.xsd:5598 to 5619, "All other values reserved") and rule r)
// p.91 bounds the serving duty at exactly this instant; 2023 scopes the duty
// to "scheduled and active" events (rule p) p.102) and permits either removal
// or status 5. 2018 rule s) p.91 protects a client from misreading the
// removal as a cancellation, and 2023 rule p) p.102 only reads removal BEFORE
// the interval end that way.
func TestEndedDERControlLeavesServiceAtItsMaxEffectiveScheduledEnd(t *testing.T) {
	t.Parallel()

	clock := newMovableClock(controlClockUnix)
	baseURL, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = clock.now
	}, "DERCEND1")
	d := devices[0]

	applyTestControlDelta(t, e, reg, "mrid-DERCEND1")

	dercHref := "/edev/" + d.edevID + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID + "/derc"
	controlHref := dercHref + "/" + soleServedControlID(t, e, d.edevID)

	// STATE 1: in force. The exact bytes, so the "after" assertions below
	// are stating a change rather than an absence that was always there.
	status, body := getSEP2(t, d, baseURL+dercHref)
	if status != http.StatusOK {
		t.Fatalf("GET DERControlList status = %d, want 200\nbody=%s", status, body)
	}
	if !bytes.Contains(body, []byte(wantActiveStatusAtFirst)) {
		t.Fatalf("served DERControl does not carry %s while in force\nbody=%s", wantActiveStatusAtFirst, body)
	}
	if !bytes.Contains(body, []byte(wantIntervalElement)) {
		t.Fatalf("served DERControl does not carry %s while in force\nbody=%s", wantIntervalElement, body)
	}
	mrid := elementText(t, body, "mRID")
	assertValidMRID(t, "issued control mRID", mrid)

	// One second BEFORE the window closes, nothing is removed. This pins the
	// boundary from the inside: a sweep that removed on ">= start + duration
	// - 1", or that keyed on the interval start, would pass every other
	// assertion in this test.
	clock.set(controlClockUnix + int64(testControlSeed.Duration) - 1)
	if removed := sweep(t, e); removed != 0 {
		t.Fatalf("sweep removed %d control(s) one second before the maximum Effective Scheduled Period closes, want 0; removal BEFORE the end of the Effective Scheduled Period is a CANCELLATION signal to a 2023 client (rule p) p.102)", removed)
	}
	if s, b := getSEP2(t, d, baseURL+controlHref); s != http.StatusOK {
		t.Fatalf("GET %s status = %d one second before the window closes, want 200\nbody=%s", controlHref, s, b)
	}

	// STATE 2: the maximum Effective Scheduled Period has closed.
	clock.set(controlClockUnix + int64(testControlSeed.Duration))
	if removed := sweep(t, e); removed != 1 {
		t.Fatalf("sweep removed %d control(s) at the close of the maximum Effective Scheduled Period, want 1", removed)
	}

	status, body = getSEP2(t, d, baseURL+dercHref)
	if status != http.StatusOK {
		t.Fatalf("GET DERControlList after the window closed: status = %d, want 200\nbody=%s", status, body)
	}
	// The list itself still answers, and says it is empty, in the two
	// attributes ListResource makes mandatory (sep2.ListResource: all and
	// results carry no omitempty). A client that got a 404 on the LIST would
	// tear down the whole DERProgram; what must disappear is the event.
	for _, want := range []string{`all="0"`, `results="0"`} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("served DERControlList does not carry %s after its only event ended\nbody=%s", want, body)
		}
	}
	if n := len(derControlElements(t, body)); n != 0 {
		t.Errorf("served DERControlList still carries %d DERControl element(s) after the maximum Effective Scheduled Period closed\nbody=%s", n, body)
	}
	if bytes.Contains(body, []byte("<currentStatus>1</currentStatus>")) {
		t.Errorf("served DERControlList still carries currentStatus 1 (Active) after the event ended; a client connecting now is REQUIRED to discard it (2018 rule l) p.90) and POST status 254 (Table 27 p.76)\nbody=%s", body)
	}
	if bytes.Contains(body, []byte(mrid)) {
		t.Errorf("served DERControlList still carries the ended event's mRID %s\nbody=%s", mrid, body)
	}

	// The event's own href stops resolving, which is what "removed from the
	// server" means to a client that cached the URI.
	if s, b := getSEP2(t, d, baseURL+controlHref); s != http.StatusNotFound {
		t.Errorf("GET %s status = %d after the event ended, want 404\nbody=%s", controlHref, s, b)
	}
}

// TestEndedDERControlMRIDOutlivesTheServedResource is the other half of the
// removal, and the half that makes it safe.
//
// 2018 Table 27 p.75 places the status 3 (Event completed) POST at
// EffectiveEndTime, which is the same instant the event stops being served.
// Dropping the mRID together with the resource therefore RACES a conformant
// client's EventCompleted. Nothing in any edition ties Response acceptance to
// event retrievability (a confirmed silence; see eventRetentionSeconds), so
// the window is our decision, and this test is what states that we actually
// made it.
func TestEndedDERControlMRIDOutlivesTheServedResource(t *testing.T) {
	t.Parallel()

	clock := newMovableClock(controlClockUnix)
	baseURL, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = clock.now
	}, "DERCEND2")
	d := devices[0]

	applyTestControlDelta(t, e, reg, "mrid-DERCEND2")

	dercHref := "/edev/" + d.edevID + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID + "/derc"
	status, body := getSEP2(t, d, baseURL+dercHref)
	if status != http.StatusOK {
		t.Fatalf("GET DERControlList status = %d, want 200\nbody=%s", status, body)
	}
	mrid := elementText(t, body, "mRID")

	ended := controlClockUnix + int64(testControlSeed.Duration)
	clock.set(ended)
	if removed := sweep(t, e); removed != 1 {
		t.Fatalf("sweep removed %d control(s), want 1", removed)
	}

	// The server can still say which event a Response naming this mRID
	// reports on, even though the resource is gone.
	rec, ok := e.EndedControl(mrid)
	if !ok {
		t.Fatalf("EndedControl(%s) = not found immediately after removal; the mRID is the SOLE correlation key a Response carries (Response.subject), and the completion POST lands at the very instant of removal", mrid)
	}
	if rec.MRID != mrid {
		t.Errorf("retained record MRID = %q, want %q", rec.MRID, mrid)
	}
	if rec.EndedAt != ended {
		t.Errorf("retained record EndedAt = %d, want %d (the close of the maximum Effective Scheduled Period)", rec.EndedAt, ended)
	}
	if want := ended + eventRetentionSeconds; rec.RetainedUntil != want {
		t.Errorf("retained record RetainedUntil = %d, want %d (EndedAt plus twice the DERControlList poll rate)", rec.RetainedUntil, want)
	}

	// A conformant client's EventCompleted, posted against an event whose
	// interval has ended, is ACCEPTED. This is the behavior the retention
	// window exists to keep true: 201 Created with a Location, exactly as it
	// would have been while the event was in force.
	const responseStatusEventCompleted uint8 = 3
	code, rbody := postResponse(t, d, baseURL, mrid, d.lfdi, responseStatusEventCompleted)
	if code != http.StatusCreated {
		t.Fatalf("POST Response (status 3) against the ended event = %d, want 201; 2018 Table 27 p.75 puts this POST at EffectiveEndTime, so removing the event must not reject it\nbody=%s", code, rbody)
	}

	// And the recorded Response carries the ended event's mRID verbatim, so
	// the server's own audit trail (2018 clause 8.8.3.2 p.77) still names
	// the control the device actually ran.
	lstatus, lbody := getSEP2(t, d, baseURL+"/rsps/1/rsp")
	if lstatus != http.StatusOK {
		t.Fatalf("GET /rsps/1/rsp status = %d, want 200\nbody=%s", lstatus, lbody)
	}
	if !bytes.Contains(lbody, []byte("<subject>"+mrid+"</subject>")) {
		t.Errorf("recorded ResponseList does not carry <subject>%s</subject>\nbody=%s", mrid, lbody)
	}
	if !bytes.Contains(lbody, []byte("<status>3</status>")) {
		t.Errorf("recorded ResponseList does not carry <status>3</status>\nbody=%s", lbody)
	}

	// The retention window closes. It is a window, not forever: the ledger
	// is the leak the removal it supports exists to stop.
	clock.set(ended + eventRetentionSeconds)
	sweep(t, e)
	if _, ok := e.EndedControl(mrid); ok {
		t.Errorf("EndedControl(%s) still resolves %d seconds after the event ended; the retention window must close or the ledger grows for the life of the process",
			mrid, eventRetentionSeconds)
	}
}

// TestSupersededControlEndsWithoutLosingItsSupersededStatus proves the two
// lifecycles compose rather than interfere.
//
// Supersession keys on interval overlap and an ended event overlaps nothing,
// so the two should not interact. "Should not" is not a test, so this one
// drives an event through BOTH: control A is superseded by control B, and
// then A's own window closes. A must reach the right terminal state, which
// under the 2018 semantics this server presents means removed from service
// with its status 4 (Superseded) intact rather than rewritten, while B is
// untouched and still Active.
//
// The failure this guards against is concrete: an end-of-life pass that
// restamped EventStatus on the way out would silently convert a superseded
// event's record into an active one, and the record is the server's only
// remaining account of what the device was told.
func TestSupersededControlEndsWithoutLosingItsSupersededStatus(t *testing.T) {
	t.Parallel()

	const step = 600
	clock := newMovableClock(controlClockUnix)
	baseURL, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = clock.now
	}, "DERCEND3")
	d := devices[0]

	applyTestControlDeltaValue(t, e, reg, "mrid-DERCEND3", "opModTargetW", 5000)
	clock.set(controlClockUnix + step)
	applyTestControlDeltaValue(t, e, reg, "mrid-DERCEND3", "opModTargetW", 7500)

	dercHref := "/edev/" + d.edevID + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID + "/derc"
	const wantOld = `<opModTargetW><multiplier>0</multiplier><value>5000</value></opModTargetW>`
	const wantNew = `<opModTargetW><multiplier>0</multiplier><value>7500</value></opModTargetW>`

	status, body := getSEP2(t, d, baseURL+dercHref)
	if status != http.StatusOK {
		t.Fatalf("GET DERControlList status = %d, want 200\nbody=%s", status, body)
	}
	superseded := derControlElementWith(t, body, wantOld)
	if !bytes.Contains(superseded, []byte(wantSupersededStatusAtSecond)) {
		t.Fatalf("the older control is not marked superseded before its window closes; this test needs supersession behavior intact to mean anything\nelement=%s", superseded)
	}
	supersededMRID := elementText(t, superseded, "mRID")

	// A's window closes first: it started a step earlier and both carry the
	// same duration. B is still in force.
	endA := controlClockUnix + int64(testControlSeed.Duration)
	clock.set(endA)
	if removed := sweep(t, e); removed != 1 {
		t.Fatalf("sweep removed %d control(s) when only the older one's window had closed, want 1", removed)
	}

	status, body = getSEP2(t, d, baseURL+dercHref)
	if status != http.StatusOK {
		t.Fatalf("GET DERControlList status = %d, want 200\nbody=%s", status, body)
	}
	if bytes.Contains(body, []byte(wantOld)) {
		t.Errorf("the superseded control is still served after its own window closed\nbody=%s", body)
	}
	// The superseding control is untouched, byte for byte, including its
	// status: a sweep that keyed on "has a predecessor" rather than on the
	// interval would have taken the wrong one.
	surviving := derControlElementWith(t, body, wantNew)
	if !bytes.Contains(surviving, []byte(wantActiveStatusAtSecond)) {
		t.Errorf("the superseding control does not carry %s after its predecessor ended\nelement=%s", wantActiveStatusAtSecond, surviving)
	}

	// The removed event's retained record keeps the status it legitimately
	// held. Under 2013 and 2018 there is no enumeration value meaning ended,
	// so 4 Superseded is the terminal state and restamping it would falsify
	// the record.
	rec, ok := e.EndedControl(supersededMRID)
	if !ok {
		t.Fatalf("EndedControl(%s) = not found after the superseded event's window closed", supersededMRID)
	}
	if rec.CurrentStatus != sep2.EventStatusSuperseded {
		t.Errorf("retained record CurrentStatus = %d, want %d (Superseded, unchanged); under the 2018 semantics this server presents there is no value meaning ended, so the terminal state is the one the event legitimately last held",
			rec.CurrentStatus, sep2.EventStatusSuperseded)
	}
	if rec.DateTime != controlClockUnix+step {
		t.Errorf("retained record DateTime = %d, want %d (the superseding event's Effective Start Time, per 2018 Annex B p.160), unchanged by the end-of-life pass",
			rec.DateTime, controlClockUnix+step)
	}

	// B's window closes in its turn and the device is left with an empty,
	// well-formed list.
	clock.set(controlClockUnix + step + int64(testControlSeed.Duration))
	if removed := sweep(t, e); removed != 1 {
		t.Fatalf("sweep removed %d control(s) when the surviving one's window had closed, want 1", removed)
	}
	status, body = getSEP2(t, d, baseURL+dercHref)
	if status != http.StatusOK {
		t.Fatalf("GET DERControlList status = %d, want 200\nbody=%s", status, body)
	}
	if n := len(derControlElements(t, body)); n != 0 {
		t.Errorf("served DERControlList carries %d DERControl element(s) after every window closed\nbody=%s", n, body)
	}
}

// TestIssuedControlCountIsBoundedByOneScheduledPeriod states the operational
// property this change buys, and the one that made it urgent enough to
// follow supersession immediately.
//
// Supersession made every control delta issue its own DERControl. Nothing then
// removed any of them, so a device's list grew for the life of the process:
// at a live delta cadence, hundreds of events per device inside one default
// 1800-second window and no ceiling at all beyond it. Removal at the end of
// each event's maximum Effective Scheduled Period is what bounds it. The
// bound is "deltas issued within one scheduled period", not "one event", and
// this test states it as such: the count tracks the sliding window, and
// events older than it are gone.
func TestIssuedControlCountIsBoundedByOneScheduledPeriod(t *testing.T) {
	t.Parallel()

	clock := newMovableClock(controlClockUnix)
	_, _, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = clock.now
	}, "DERCEND4")

	// Twelve deltas at 300-second spacing: 3300 seconds of wall clock
	// against a 1800-second window, so the earliest six fall out of it.
	const spacing = 300
	const deltas = 12
	for i := range deltas {
		clock.set(controlClockUnix + int64(i)*spacing)
		applyTestControlDeltaValue(t, e, reg, "mrid-DERCEND4", "opModTargetW", float64(1000+i))
	}

	snaps, err := e.DERControls(context.Background(), embedURLIndex(t, e, "mrid-DERCEND4"), controlFSAID, controlDERProgramID)
	if err != nil {
		t.Fatalf("DERControls: %v", err)
	}

	// Whatever the exact arithmetic, the count must be strictly less than
	// the number of deltas issued: that inequality IS the bound, and before
	// this change it was an equality for the life of the process.
	if len(snaps) >= deltas {
		t.Fatalf("device holds %d DERControls after %d deltas spanning %d seconds against a %d-second window; the collection is not bounded by the scheduled period",
			len(snaps), deltas, (deltas-1)*spacing, testControlSeed.Duration)
	}

	// And precisely: only the events whose maximum Effective Scheduled
	// Period is still open survive. The last delta landed at
	// controlClockUnix + 3300, so an event created at T survives when
	// T + 1800 > 3300 + controlClockUnix, i.e. T > controlClockUnix + 1500:
	// the deltas at 1800, 2100, 2400, 2700, 3000 and 3300.
	const wantLive = 6
	if len(snaps) != wantLive {
		t.Errorf("device holds %d DERControls, want %d (the deltas whose window is still open at the last delta's instant)", len(snaps), wantLive)
	}

	// Nothing is silently dropped: every removed event is still resolvable
	// by mRID for its retention window.
	if got := e.ended.len(); got != deltas-wantLive {
		t.Errorf("retention ledger holds %d record(s), want %d (one per removed event)", got, deltas-wantLive)
	}
}

// TestDERControlListAdvertisesTheAssumedPollRate pins the number the
// retention window is derived from.
//
// eventRetentionSeconds is twice the poll rate the DERControlList advertises,
// borrowed from the only numeric retention figure in the family (2023 rule p)
// p.102, which governs cancelled events). Core hardcodes that poll rate and
// does not export it, so derControlListPollRate is a duplicate. Reading it
// back off the served bytes is what stops the duplicate from drifting: a
// change on core's side fails here instead of silently resizing our window.
func TestDERControlListAdvertisesTheAssumedPollRate(t *testing.T) {
	t.Parallel()

	baseURL, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = fixedControlClock()
	}, "DERCEND5")
	d := devices[0]

	applyTestControlDelta(t, e, reg, "mrid-DERCEND5")

	status, body := getSEP2(t, d, baseURL+"/edev/"+d.edevID+"/fsa/"+controlFSAID+"/derp/"+controlDERProgramID+"/derc")
	if status != http.StatusOK {
		t.Fatalf("GET DERControlList status = %d, want 200\nbody=%s", status, body)
	}

	want := `pollRate="` + strconv.FormatInt(derControlListPollRate, 10) + `"`
	if !bytes.Contains(body, []byte(want)) {
		t.Errorf("served DERControlList does not advertise %s; eventRetentionSeconds is twice this value, so a change here resizes the retention window\nbody=%s", want, body)
	}
	if eventRetentionSeconds != 2*derControlListPollRate {
		t.Errorf("eventRetentionSeconds = %d, want %d (twice the advertised poll rate, 2023 rule p) p.102)", eventRetentionSeconds, 2*derControlListPollRate)
	}
}

// TestMaxEffectiveScheduledEnd covers the arithmetic the whole lifecycle
// keys on, including the two cases a served fleet does not exercise today.
//
// Positive randomization EXTENDS the window and negative randomization is
// ignored, because the value being computed is the instant after which NO
// device can still be executing the event. Sizing on the earliest possible
// finish would take an event out of service while a device that randomized
// the other way is still running it.
func TestMaxEffectiveScheduledEnd(t *testing.T) {
	t.Parallel()

	i32 := func(v int32) *sep2.OneHourRange { r := sep2.OneHourRange(v); return &r }

	for _, tc := range []struct {
		name    string
		control sep2.DERControl
		want    int64
		wantOK  bool
	}{
		{
			name:    "no interval is not datable and is never removed",
			control: sep2.DERControl{},
			wantOK:  false,
		},
		{
			name:    "start plus duration",
			control: derControlWithInterval(1000, 1800, nil, nil),
			want:    2800,
			wantOK:  true,
		},
		{
			name:    "positive randomizeDuration extends the window",
			control: derControlWithInterval(1000, 1800, i32(60), nil),
			want:    2860,
			wantOK:  true,
		},
		{
			name:    "positive randomizeStart extends the window",
			control: derControlWithInterval(1000, 1800, nil, i32(45)),
			want:    2845,
			wantOK:  true,
		},
		{
			name:    "both positive randomizations extend the window",
			control: derControlWithInterval(1000, 1800, i32(60), i32(45)),
			want:    2905,
			wantOK:  true,
		},
		{
			name:    "negative randomizeDuration does not shorten the window",
			control: derControlWithInterval(1000, 1800, i32(-60), nil),
			want:    2800,
			wantOK:  true,
		},
		{
			name:    "negative randomizeStart does not shorten the window",
			control: derControlWithInterval(1000, 1800, nil, i32(-45)),
			want:    2800,
			wantOK:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := maxEffectiveScheduledEnd(tc.control)
			if ok != tc.wantOK {
				t.Fatalf("maxEffectiveScheduledEnd ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("maxEffectiveScheduledEnd = %d, want %d", got, tc.want)
			}
		})
	}
}

// derControlWithInterval builds a DERControl carrying just the temporal
// fields maxEffectiveScheduledEnd reads.
func derControlWithInterval(start int64, duration uint32, randomizeDuration, randomizeStart *sep2.OneHourRange) sep2.DERControl {
	var c sep2.DERControl
	c.Interval = &sep2.DateTimeInterval{Start: start, Duration: duration}
	c.RandomizeDuration = randomizeDuration
	c.RandomizeStart = randomizeStart
	return c
}

// TestMarkEndedPerEdition covers the terminal status, which is the CTP
// CORE-022 transition ("[S] Update the DERControl#N currentStatus/dateTime
// values at each state of the DER event", CTP v1.2 pp.67 to 69, required for
// the Server profile) applied at the end of the lifecycle.
//
// The edition split is the point. Under 2013 and 2018 there is no value
// meaning ended: the enumeration is 0 Scheduled, 1 Active, 2 Cancelled,
// 3 Cancelled with Randomization, 4 Superseded, "All other values reserved"
// (Annex B p.160; sep.xsd:5598 to 5619). Under 2023 there is exactly one,
// and setting it is a SHALL (Annex B p.169), with a carve-out for an event
// that was cancelled rather than completed.
//
// Both editions are exercised even though only 2018 is served today
// (servedEventEdition), for the reason supersession's design gave: an untested branch is
// not a mechanism, it is a comment.
func TestMarkEndedPerEdition(t *testing.T) {
	t.Parallel()

	const at int64 = 4242

	for _, tc := range []struct {
		name        string
		edition     eventEdition
		start       uint8
		wantChanged bool
		wantStatus  uint8
		wantTime    int64
	}{
		{
			name:        "2018 active event has no terminal value to move to",
			edition:     edition2018,
			start:       sep2.EventStatusActive,
			wantChanged: false,
			wantStatus:  sep2.EventStatusActive,
			wantTime:    1,
		},
		{
			name:        "2018 superseded event keeps status 4",
			edition:     edition2018,
			start:       sep2.EventStatusSuperseded,
			wantChanged: false,
			wantStatus:  sep2.EventStatusSuperseded,
			wantTime:    1,
		},
		{
			name:        "2023 active event completes",
			edition:     edition2023,
			start:       sep2.EventStatusActive,
			wantChanged: true,
			wantStatus:  sep2.EventStatusComplete,
			wantTime:    at,
		},
		{
			name:        "2023 superseded event completes, since 4 is deprecated there anyway",
			edition:     edition2023,
			start:       sep2.EventStatusSuperseded,
			wantChanged: true,
			wantStatus:  sep2.EventStatusComplete,
			wantTime:    at,
		},
		{
			name:        "2023 cancelled event keeps its cancellation",
			edition:     edition2023,
			start:       sep2.EventStatusCancelled,
			wantChanged: false,
			wantStatus:  sep2.EventStatusCancelled,
			wantTime:    1,
		},
		{
			name:        "2023 cancelled-with-randomization event keeps its cancellation",
			edition:     edition2023,
			start:       eventStatusCancelledWithRandomization,
			wantChanged: false,
			wantStatus:  eventStatusCancelledWithRandomization,
			wantTime:    1,
		},
		{
			name:        "2023 already-completed event is not restamped",
			edition:     edition2023,
			start:       sep2.EventStatusComplete,
			wantChanged: false,
			wantStatus:  sep2.EventStatusComplete,
			wantTime:    1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			es := &sep2.EventStatus{CurrentStatus: tc.start, DateTime: 1}
			if got := markEnded(tc.edition, es, at); got != tc.wantChanged {
				t.Errorf("markEnded changed = %v, want %v", got, tc.wantChanged)
			}
			if es.CurrentStatus != tc.wantStatus {
				t.Errorf("currentStatus = %d, want %d", es.CurrentStatus, tc.wantStatus)
			}
			if es.DateTime != tc.wantTime {
				t.Errorf("dateTime = %d, want %d", es.DateTime, tc.wantTime)
			}
		})
	}

	// A nil EventStatus is a record no path in this package produces; it must
	// not panic the fleet-wide sweep.
	if markEnded(edition2023, nil, at) {
		t.Error("markEnded reported a change on a nil EventStatus")
	}
}

// TestExpireEndedControlsSweepDoesNotMaterializeAnUntouchedDevicesControlBucket
// guards the core IEEECORE-111 read contract at the one call site in this
// package where a regression could reach it silently. The periodic sweep
// (expireEndedControls) lists every seeded device and, for each, reads that
// device's own DERControls scope (expireDeviceControls) to decide whether
// anything needs removing. Reaching that read through ForParent, the
// create-on-miss path core still exports for callers that hold the concrete
// type, would allocate a parent bucket for a device the sweep only READ, not
// wrote to. That allocation would then run on a timer for every quiet device
// in the fleet, with nothing else in this package positioned to observe it:
// the sweep never inspects HasParent itself, only List and Delete.
//
// Device A gets one issued control; device B is seeded but never touched by
// anything on the control path, so it is the sweep's own read that must not
// leave a trace.
func TestExpireEndedControlsSweepDoesNotMaterializeAnUntouchedDevicesControlBucket(t *testing.T) {
	t.Parallel()

	reg, st := twoDeviceFixture(t)
	ctx := context.Background()
	notifier := coresub.NewManager(st.Subscriptions, 1, 10)

	edevA := urlIndexFor(t, st, "mrid-a")
	edevB := urlIndexFor(t, st, "mrid-b")
	scopeA := derControlScope(edevA, controlFSAID, controlDERProgramID)
	scopeB := derControlScope(edevB, controlFSAID, controlDERProgramID)

	delta := diff.Difference{
		Object:    "mrid-a",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0.0, "value": 1000.0},
	}
	if err := ApplyControlDelta(ctx, st, notifier, reg, testControlPolicy, delta); err != nil {
		t.Fatalf("ApplyControlDelta: %v", err)
	}

	// A's own bucket is expected to exist: its Create is the genuine,
	// intentional materialization in this path.
	hasA, err := st.DERControls.HasParent(ctx, scopeA)
	if err != nil {
		t.Fatalf("HasParent(A): %v", err)
	}
	if !hasA {
		t.Fatalf("HasParent(A) = false after ApplyControlDelta issued a control there, want true")
	}

	// Run the sweep, which visits every seeded device including B.
	ledger := newEndedControlLedger()
	if _, err := expireEndedControls(ctx, st, notifier, ledger, servedEventEdition, 0); err != nil {
		t.Fatalf("expireEndedControls: %v", err)
	}

	hasB, err := st.DERControls.HasParent(ctx, scopeB)
	if err != nil {
		t.Fatalf("HasParent(B): %v", err)
	}
	if hasB {
		t.Fatalf("HasParent(B) = true after a sweep that never wrote anything for device B; the sweep's read materialized a parent bucket for a device with no issued DERControl, reintroducing the allocation IEEECORE-111 removed")
	}
}
