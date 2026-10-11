package sep2embed

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	coresub "github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/handlers/subscription"
)

// Tests for the runtime per-device default watts. The served bytes are read
// over the embed's own TLS listener, as a device reads them.

func ddercURL(baseURL string, d mupTestDevice) string {
	return baseURL + "/edev/" + d.edevID + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID + "/dderc"
}

func getDDERC(t *testing.T, baseURL string, d mupTestDevice) []byte {
	t.Helper()
	status, body := getSEP2(t, d, ddercURL(baseURL, d))
	if status != http.StatusOK {
		t.Fatalf("GET dderc = %d, want 200\nbody=%s", status, body)
	}
	return body
}

func TestSetDefaultTargetWServesOnlyThatDevicesTarget(t *testing.T) {
	t.Parallel()
	baseURL, devices, e, reg := newEmbedTestServer(t, nil, "DDERCSETA1", "DDERCSETB1")
	a, b := devices[0], devices[1]
	ctx := context.Background()

	beforeA, beforeB := getDDERC(t, baseURL, a), getDDERC(t, baseURL, b)
	if bytes.Contains(beforeA, []byte("opModTargetW")) {
		t.Fatalf("seeded dderc already carries opModTargetW: %s", beforeA)
	}

	if _, err := e.SetDefaultTargetW(ctx, reg, "mrid-DDERCSETA1", -1500, nil); err != nil {
		t.Fatalf("SetDefaultTargetW: %v", err)
	}

	const element = "<opModTargetW><multiplier>0</multiplier><value>-1500</value></opModTargetW>"
	gotA := getDDERC(t, baseURL, a)
	if !bytes.Contains(gotA, []byte(element)) {
		t.Fatalf("device A dderc lacks %s\nbody=%s", element, gotA)
	}
	if rest := bytes.Replace(gotA, []byte(element), nil, 1); !bytes.Equal(rest, beforeA) {
		t.Errorf("device A dderc changed beyond opModTargetW\nbefore=%s\nafter =%s", beforeA, gotA)
	}
	if gotB := getDDERC(t, baseURL, b); !bytes.Equal(gotB, beforeB) {
		t.Errorf("device B dderc changed\nbefore=%s\nafter =%s", beforeB, gotB)
	}

	// A second set replaces the value rather than adding to it.
	if _, err := e.SetDefaultTargetW(ctx, reg, "mrid-DDERCSETA1", 2000, nil); err != nil {
		t.Fatalf("second SetDefaultTargetW: %v", err)
	}
	if got := getDDERC(t, baseURL, a); !bytes.Contains(got, []byte("<opModTargetW><multiplier>0</multiplier><value>2000</value></opModTargetW>")) ||
		bytes.Count(got, []byte("<opModTargetW>")) != 1 {
		t.Errorf("device A dderc after second set = %s, want one opModTargetW of 2000", got)
	}

	if err := e.ClearDefaultTargetW(ctx, reg, "mrid-DDERCSETA1"); err != nil {
		t.Fatalf("ClearDefaultTargetW: %v", err)
	}
	if got := getDDERC(t, baseURL, a); !bytes.Equal(got, beforeA) {
		t.Errorf("device A dderc after clear is not the fleet seed\nseed =%s\nafter=%s", beforeA, got)
	}
	if gotB := getDDERC(t, baseURL, b); !bytes.Equal(gotB, beforeB) {
		t.Errorf("device B dderc changed by A's clear\nbefore=%s\nafter =%s", beforeB, gotB)
	}
}

// A fleet seed that carries its own opModTargetW is what clear must restore,
// not an absent element.
func TestClearDefaultTargetWRestoresASeededTarget(t *testing.T) {
	t.Parallel()
	connect := true
	baseURL, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DefaultControl = sep2.DefaultDERControl{DERControlBase: &sep2.DERControlBase{
			OpModConnect: &connect,
			OpModTargetW: &sep2.ActivePower{Multiplier: 1, Value: 25},
		}}
	}, "DDERCSEED1")
	a := devices[0]
	ctx := context.Background()

	seed := getDDERC(t, baseURL, a)
	if !bytes.Contains(seed, []byte("<opModTargetW><multiplier>1</multiplier><value>25</value></opModTargetW>")) {
		t.Fatalf("seed dderc lacks the configured target: %s", seed)
	}
	if _, err := e.SetDefaultTargetW(ctx, reg, "mrid-DDERCSEED1", -700, nil); err != nil {
		t.Fatalf("SetDefaultTargetW: %v", err)
	}
	if got := getDDERC(t, baseURL, a); !bytes.Contains(got, []byte("<value>-700</value>")) {
		t.Fatalf("dderc after set = %s, want -700", got)
	}
	if err := e.ClearDefaultTargetW(ctx, reg, "mrid-DDERCSEED1"); err != nil {
		t.Fatalf("ClearDefaultTargetW: %v", err)
	}
	if got := getDDERC(t, baseURL, a); !bytes.Equal(got, seed) {
		t.Errorf("dderc after clear is not the seed\nseed =%s\nafter=%s", seed, got)
	}
}

func TestDefaultTargetWReadsBackTheStoredValue(t *testing.T) {
	t.Parallel()
	e, reg, _, _ := newSendEmbed(t, controlClockUnix)
	ctx := context.Background()

	got, err := e.DefaultTargetW(ctx, "mrid-a")
	if err != nil || got != nil {
		t.Fatalf("DefaultTargetW before set = %+v, %v; want nil, nil", got, err)
	}
	if _, err := e.SetDefaultTargetW(ctx, reg, "mrid-a", 1234, nil); err != nil {
		t.Fatalf("SetDefaultTargetW: %v", err)
	}
	got, err = e.DefaultTargetW(ctx, "mrid-a")
	if err != nil || got == nil || got.Value != 1234 || got.Multiplier != 0 {
		t.Fatalf("DefaultTargetW after set = %+v, %v; want value 1234 multiplier 0", got, err)
	}
	if other, err := e.DefaultTargetW(ctx, "mrid-b"); err != nil || other != nil {
		t.Errorf("device B DefaultTargetW = %+v, %v; want nil", other, err)
	}
	if _, err := e.DefaultTargetW(ctx, "mrid-nope"); !errors.Is(err, ErrUnknownControlDevice) {
		t.Errorf("unknown device err = %v, want ErrUnknownControlDevice", err)
	}
}

func TestSetDefaultTargetWRefusesAndWritesNothing(t *testing.T) {
	t.Parallel()
	e, reg, _, scope := newSendEmbed(t, controlClockUnix)
	ctx := context.Background()
	before, err := e.stores.DefaultDERControls.Get(ctx, scope, singletonKey)
	if err != nil {
		t.Fatalf("Get dderc: %v", err)
	}

	for _, tc := range []struct {
		name  string
		mrid  string
		watts int64
		want  error
	}{
		{"unknown device", "mrid-nope", 100, ErrUnknownControlDevice},
		{"above int16", "mrid-a", 40000, ErrControlValueInvalid},
		{"below int16", "mrid-a", -40000, ErrControlValueInvalid},
	} {
		if _, err := e.SetDefaultTargetW(ctx, reg, tc.mrid, tc.watts, nil); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if err := e.ClearDefaultTargetW(ctx, reg, "mrid-nope"); !errors.Is(err, ErrUnknownControlDevice) {
		t.Errorf("clear unknown device: err = %v, want ErrUnknownControlDevice", err)
	}
	after, err := e.stores.DefaultDERControls.Get(ctx, scope, singletonKey)
	if err != nil {
		t.Fatalf("Get dderc: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Errorf("refused sets changed the dderc: before %+v after %+v", before, after)
	}
}

func allIssued(string) bool { return true }

func TestSetDefaultTargetWCancelsPageControls(t *testing.T) {
	t.Parallel()
	const now = controlClockUnix
	e, reg, clock, scope := newSendEmbed(t, now)
	ctx := context.Background()

	send, err := e.ApplyControlFor(ctx, reg, targetWDelta("mrid-a", 0, -2000), 300)
	if err != nil {
		t.Fatalf("ApplyControlFor: %v", err)
	}
	page := map[string]bool{send.ControlID: true, send.FollowOnID: true}

	clock.set(now + 10)
	set, err := e.SetDefaultTargetW(ctx, reg, "mrid-a", 500, func(id string) bool { return page[id] })
	if err != nil {
		t.Fatalf("SetDefaultTargetW: %v", err)
	}
	if len(set.Cancelled) != 2 {
		t.Errorf("Cancelled = %v, want the control and its follow-on", set.Cancelled)
	}
	controls := storedControls(t, e, scope)
	for _, id := range []string{send.ControlID, send.FollowOnID} {
		c := controlByID(t, controls, id)
		if c.EventStatus.CurrentStatus != sep2.EventStatusCancelled || c.EventStatus.DateTime != now+10 {
			t.Errorf("control %s status = %d at %d, want Cancelled (2) at %d", id, c.EventStatus.CurrentStatus, c.EventStatus.DateTime, now+10)
		}
	}
}

func TestSetDefaultTargetWLeavesControlsThePredicateDoesNotName(t *testing.T) {
	t.Parallel()
	const now = controlClockUnix
	e, reg, _, scope := newSendEmbed(t, now)
	ctx := context.Background()

	send, err := e.ApplyControlFor(ctx, reg, targetWDelta("mrid-a", 0, -2000), 300)
	if err != nil {
		t.Fatalf("ApplyControlFor: %v", err)
	}
	set, err := e.SetDefaultTargetW(ctx, reg, "mrid-a", 500, func(string) bool { return false })
	if err != nil {
		t.Fatalf("SetDefaultTargetW: %v", err)
	}
	if len(set.Cancelled) != 0 {
		t.Errorf("Cancelled = %v, want none", set.Cancelled)
	}
	controls := storedControls(t, e, scope)
	if c := controlByID(t, controls, send.ControlID); c.EventStatus.CurrentStatus != sep2.EventStatusActive {
		t.Errorf("control status = %d, want Active", c.EventStatus.CurrentStatus)
	}
	if c := controlByID(t, controls, send.FollowOnID); c.EventStatus.CurrentStatus != sep2.EventStatusScheduled {
		t.Errorf("follow-on status = %d, want Scheduled", c.EventStatus.CurrentStatus)
	}
}

// A bus control is never cancelled, even when the caller's record would call
// it a page control.
func TestSetDefaultTargetWNeverCancelsABusControl(t *testing.T) {
	t.Parallel()
	const now = controlClockUnix
	e, reg, _, scope := newSendEmbed(t, now)
	ctx := context.Background()

	if _, err := e.ApplyControlDeltaOutcome(ctx, reg, targetWDelta("mrid-a", 0, 3000)); err != nil {
		t.Fatalf("bus delta: %v", err)
	}
	before := storedControls(t, e, scope)
	if len(before) != 1 || before[0].EventStatus.CurrentStatus != sep2.EventStatusActive {
		t.Fatalf("bus control setup = %+v, want one Active control", before)
	}

	set, err := e.SetDefaultTargetW(ctx, reg, "mrid-a", 500, allIssued)
	if err != nil {
		t.Fatalf("SetDefaultTargetW: %v", err)
	}
	if len(set.Cancelled) != 0 {
		t.Errorf("Cancelled = %v, want none", set.Cancelled)
	}
	after := storedControls(t, e, scope)
	if len(after) != 1 || after[0].EventStatus.CurrentStatus != sep2.EventStatusActive || after[0].EventStatus.DateTime != before[0].EventStatus.DateTime {
		t.Errorf("bus control after set = %+v, want untouched Active", after[0].EventStatus)
	}
}

// Another device's page controls are not touched by a set on this one.
func TestSetDefaultTargetWCancelsOnlyThatDevice(t *testing.T) {
	t.Parallel()
	const now = controlClockUnix
	e, reg, _, _ := newSendEmbed(t, now)
	ctx := context.Background()
	scopeB := derControlScope(urlIndexFor(t, e.stores, "mrid-b"), controlFSAID, controlDERProgramID)

	sendB, err := e.ApplyControlFor(ctx, reg, targetWDelta("mrid-b", 0, -2000), 300)
	if err != nil {
		t.Fatalf("ApplyControlFor: %v", err)
	}
	if _, err := e.SetDefaultTargetW(ctx, reg, "mrid-a", 500, allIssued); err != nil {
		t.Fatalf("SetDefaultTargetW: %v", err)
	}
	if c := controlByID(t, storedControls(t, e, scopeB), sendB.ControlID); c.EventStatus.CurrentStatus != sep2.EventStatusActive {
		t.Errorf("device B control status = %d, want Active", c.EventStatus.CurrentStatus)
	}
	if got, err := e.DefaultTargetW(ctx, "mrid-b"); err != nil || got != nil {
		t.Errorf("device B default = %+v, %v; want nil", got, err)
	}
}

// A subscriber to the device's DERProgramList hears of a set and of a clear,
// as it does of a send.
func TestDefaultTargetWNotifiesTheProgramList(t *testing.T) {
	t.Parallel()
	reg, st := twoDeviceFixture(t)
	notifier := coresub.NewManager(st.Subscriptions, 1, 10,
		coresub.WithDestinationPolicy(coresub.DestinationPolicy{AllowLoopback: true}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go notifier.Start(ctx)
	e := &Embed{stores: st, notifier: notifier, policy: testControlPolicy, ended: newEndedControlLedger()}

	got, _ := subscribeCapture(t, ctx, urlIndexFor(t, st, "mrid-a"), st.Subscriptions)
	if _, err := e.SetDefaultTargetW(ctx, reg, "mrid-a", 800, nil); err != nil {
		t.Fatalf("SetDefaultTargetW: %v", err)
	}
	assertStatusZero(t, awaitBody(t, got))
	if err := e.ClearDefaultTargetW(ctx, reg, "mrid-a"); err != nil {
		t.Fatalf("ClearDefaultTargetW: %v", err)
	}
	assertStatusZero(t, awaitBody(t, got))
}
