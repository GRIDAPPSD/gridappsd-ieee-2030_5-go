package sep2embed

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/assembly"
	coresub "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/handlers/subscription"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/store"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// testDefaultControl is the GAGO-050 seed value this file's tests pass to
// ApplyControlDelta. These tests assert GAGO-034 DOWN-path field mapping
// and owner scoping, not the GAGO-050 seeding behavior itself (that is
// snapshot_test.go's job), so the zero value is deliberate: a valid but
// degenerate DefaultDERControl, sufficient for ensureDERProgram's lazy
// DERProgram creation without asserting anything about its contents here.
var testDefaultControl = sep2.DefaultDERControl{}

// testProgramSeed is the DERProgram policy this file's tests pass to
// ApplyControlDelta. It mirrors sep2config.DefaultPolicy's compiled-in
// values rather than the zero value, so a test that does assert on the
// served program sees what a default deployment serves. Primacy is spelled
// out rather than left implicit because 0 is a legal primacy, so the zero
// value would silently assert a different program than the one shipped.
var testProgramSeed = DERProgramSeed{Primacy: 1, Description: "GridAPPS-D DER program"}

// testControlSeed is the issued-DERControl temporal policy this file's tests
// pass to ApplyControlDelta. Duration mirrors
// sep2config.DefaultDERControlDuration rather than being an arbitrary test
// number, so a test asserting on an issued control sees the window a default
// deployment serves. It is spelled out rather than left zero because a zero
// Duration is refused outright (ErrDERControlDurationUnset), which is itself
// asserted by TestApplyControlDeltaRefusesUnconfiguredDuration.
//
// Now is nil, so these tests read the real clock: they assert field mapping
// and owner scoping, not timestamps. The wire-level temporal assertions in
// control_interval_test.go fix the clock instead.
var testControlSeed = DERControlSeed{Duration: 1800}

// testControlPolicy bundles the three fixtures above into the single value
// ApplyControlDelta and seedStores now take.
var testControlPolicy = ControlPolicy{
	DefaultControl: testDefaultControl,
	Program:        testProgramSeed,
	Control:        testControlSeed,
}

// twoDeviceFixture seeds a Registry and a fully populated assembly.Stores
// (via the package's own seedStores, not a parallel construction) with
// two devices, A and B, so tests below can assert owner scoping between
// them.
func twoDeviceFixture(t *testing.T) (reg *registry.Registry, st *assembly.Stores) {
	t.Helper()
	return twoDeviceFixtureWithPolicy(t, seedPolicy{
		resolvePIN: testResolvePIN,
		control:    testControlPolicy,
	})
}

// twoDeviceFixtureWithPolicy is twoDeviceFixture with the seeding policy
// supplied by the caller.
//
// It exists because seeding now writes the DERProgram and its
// DefaultDERControl, so the policy handed to seedStores is what those
// resources are built from. A test that asserts on a seeded
// DefaultDERControl must seed with the same value it later expects: passing
// a populated default control to ApplyControlDelta alone no longer reaches
// the store, because ensureDERProgram returns early once the seeded program
// exists. In cmd/bridge both sides are the same policy.DefaultControl, so
// they cannot disagree there; in a test they can, and this is how a test
// keeps them consistent.
func twoDeviceFixtureWithPolicy(t *testing.T, policy seedPolicy) (reg *registry.Registry, st *assembly.Stores) {
	t.Helper()

	reg = registry.New()
	if err := reg.AddBatch([]registry.Entry{
		{MRID: "mrid-a", Name: "Device A", LFDI: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", SFDI: "11111111111"},
		{MRID: "mrid-b", Name: "Device B", LFDI: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", SFDI: "22222222222"},
	}); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	st = newStores()
	if err := seedStores(context.Background(), st, reg, policy); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	return reg, st
}

func TestApplyControlDeltaOwnerScopingAndFieldFidelity(t *testing.T) {
	t.Parallel()

	reg, st := twoDeviceFixture(t)
	notifier := coresub.NewManager(st.Subscriptions, 2, 10)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go notifier.Start(ctx)

	var hitsA, hitsB atomic.Int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srvB.Close()

	edevA := urlIndexFor(t, st, "mrid-a")
	edevB := urlIndexFor(t, st, "mrid-b")

	if err := st.Subscriptions.Create(ctx, "sub-a", sep2.Subscription{
		SubscribableResource: sep2.SubscribableResource{Resource: sep2.Resource{Href: "/edev/" + edevA + "/sub/1"}},
		SubscribedResource:   derProgramListHref(edevA, controlFSAID),
		NotificationURI:      srvA.URL + "/notify",
	}); err != nil {
		t.Fatalf("seed subscription A: %v", err)
	}
	if err := st.Subscriptions.Create(ctx, "sub-b", sep2.Subscription{
		SubscribableResource: sep2.SubscribableResource{Resource: sep2.Resource{Href: "/edev/" + edevB + "/sub/1"}},
		SubscribedResource:   derProgramListHref(edevB, controlFSAID),
		NotificationURI:      srvB.URL + "/notify",
	}); err != nil {
		t.Fatalf("seed subscription B: %v", err)
	}

	delta := diff.Difference{
		Object:    "mrid-a",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0.0, "value": 5000.0},
	}

	if err := ApplyControlDelta(ctx, st, notifier, reg, testControlPolicy, delta); err != nil {
		t.Fatalf("ApplyControlDelta: %v", err)
	}

	// Field fidelity: A's control carries exactly the delta's value.
	scopeA := derControlScope(edevA, controlFSAID, controlDERProgramID)
	control, err := st.DERControls.Get(ctx, scopeA, activeControlID)
	if err != nil {
		t.Fatalf("DERControls.Get(A): %v", err)
	}
	if control.DERControlBase == nil || control.DERControlBase.OpModTargetW == nil {
		t.Fatalf("device A control has no OpModTargetW: %+v", control)
	}
	if control.DERControlBase.OpModTargetW.Value != 5000 || control.DERControlBase.OpModTargetW.Multiplier != 0 {
		t.Errorf("device A OpModTargetW = %+v, want {Multiplier:0 Value:5000}", control.DERControlBase.OpModTargetW)
	}

	// Owner scoping: device B's own scope carries NO control at all.
	scopeB := derControlScope(edevB, controlFSAID, controlDERProgramID)
	if _, err := st.DERControls.Get(ctx, scopeB, activeControlID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DERControls.Get(B) = (%v), want store.ErrNotFound (control must not leak to device B)", err)
	}

	// Notifier scoping: only A's subscriber is notified.
	deadline := time.After(2 * time.Second)
	for hitsA.Load() < 1 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for device A's subscriber notification (hitsA=%d)", hitsA.Load())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	// Give a full notification cycle for the fan-out to reach B if it
	// (incorrectly) were going to.
	time.Sleep(100 * time.Millisecond)
	if hitsB.Load() != 0 {
		t.Errorf("device B's subscriber received %d notifications, want 0 (cross-device notify leak)", hitsB.Load())
	}
}

func TestApplyControlDeltaRefusesUnknownDevice(t *testing.T) {
	t.Parallel()

	reg, st := twoDeviceFixture(t)
	notifier := coresub.NewManager(st.Subscriptions, 1, 10)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go notifier.Start(ctx)

	delta := diff.Difference{
		Object:    "mrid-does-not-exist",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0.0, "value": 1000.0},
	}

	err := ApplyControlDelta(ctx, st, notifier, reg, testControlPolicy, delta)
	if !errors.Is(err, ErrUnknownControlDevice) {
		t.Fatalf("ApplyControlDelta(unknown device) error = %v, want ErrUnknownControlDevice", err)
	}

	// Neither device's scope gained a control from the refused delta.
	edevA := urlIndexFor(t, st, "mrid-a")
	scopeA := derControlScope(edevA, controlFSAID, controlDERProgramID)
	if _, err := st.DERControls.Get(ctx, scopeA, activeControlID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DERControls.Get(A) after refused delta = (%v), want store.ErrNotFound", err)
	}
}

func TestApplyControlDeltaRefusesUnsupportedAttribute(t *testing.T) {
	t.Parallel()

	reg, st := twoDeviceFixture(t)
	notifier := coresub.NewManager(st.Subscriptions, 1, 10)

	tests := []struct {
		name string
		attr string
	}{
		{"wrong prefix entirely", "DERStatus.genConnectStatus"},
		{"shallow DERControl without DERControlBase", "DERControl.mRID"},
		{"unrecognized DERControlBase field", "DERControl.DERControlBase.opModNoSuchField"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			delta := diff.Difference{Object: "mrid-a", Attribute: tt.attr, Value: true}
			err := ApplyControlDelta(context.Background(), st, notifier, reg, testControlPolicy, delta)
			if !errors.Is(err, ErrUnsupportedControlAttribute) {
				t.Fatalf("ApplyControlDelta(%q) error = %v, want ErrUnsupportedControlAttribute", tt.attr, err)
			}
		})
	}
}

// TestApplyControlDeltaMergesSecondFieldNotDuplicate proves the
// supersede semantics documented on ApplyControlDelta: two deltas for
// the same device, touching two different DERControlBase fields, result
// in ONE DERControl carrying BOTH fields, not two competing controls.
func TestApplyControlDeltaMergesSecondFieldNotDuplicate(t *testing.T) {
	t.Parallel()

	reg, st := twoDeviceFixture(t)
	notifier := coresub.NewManager(st.Subscriptions, 1, 10)
	ctx := context.Background()

	first := diff.Difference{
		Object:    "mrid-a",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0.0, "value": 3000.0},
	}
	second := diff.Difference{
		Object:    "mrid-a",
		Attribute: "DERControl.DERControlBase.opModTargetVar",
		Value:     map[string]any{"multiplier": 0.0, "value": 500.0},
	}

	if err := ApplyControlDelta(ctx, st, notifier, reg, testControlPolicy, first); err != nil {
		t.Fatalf("ApplyControlDelta(first): %v", err)
	}
	if err := ApplyControlDelta(ctx, st, notifier, reg, testControlPolicy, second); err != nil {
		t.Fatalf("ApplyControlDelta(second): %v", err)
	}

	edevA := urlIndexFor(t, st, "mrid-a")
	scope := derControlScope(edevA, controlFSAID, controlDERProgramID)

	// Exactly one control exists at the active slot; List confirms no
	// second entry was created alongside it.
	list, err := st.DERControls.ForParent(scope).List(ctx, store.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if list.All != 1 {
		t.Fatalf("DERControl count for device A = %d, want 1 (merge, not duplicate)", list.All)
	}

	control, err := st.DERControls.Get(ctx, scope, activeControlID)
	if err != nil {
		t.Fatalf("DERControls.Get: %v", err)
	}
	if control.DERControlBase == nil {
		t.Fatal("merged control has nil DERControlBase")
	}
	if control.DERControlBase.OpModTargetW == nil || control.DERControlBase.OpModTargetW.Value != 3000 {
		t.Errorf("merged control OpModTargetW = %+v, want Value=3000 (preserved from first delta)", control.DERControlBase.OpModTargetW)
	}
	if control.DERControlBase.OpModTargetVar == nil || control.DERControlBase.OpModTargetVar.Value != 500 {
		t.Errorf("merged control OpModTargetVar = %+v, want Value=500 (applied by second delta)", control.DERControlBase.OpModTargetVar)
	}
}

// TestApplyControlDeltaSeedsDefaultDERControlOnEveryDERProgram is the
// GAGO-050 test: ensureDERProgram's lazy-creation seam must seed a
// DefaultDERControl (sourced from the caller-supplied defaultControl,
// never hardcoded) into stores.DefaultDERControls and point the new
// DERProgram's DefaultDERControlLink at it, closing the CSIP-mandatory
// hole Devi flagged (a client following DefaultDERControlLink from a
// DERProgram must find a well-formed DefaultDERControl).
func TestApplyControlDeltaSeedsDefaultDERControlOnEveryDERProgram(t *testing.T) {
	t.Parallel()

	connect := true
	energize := true
	seed := sep2.DefaultDERControl{
		DERControlBase: &sep2.DERControlBase{
			OpModConnect:  &connect,
			OpModEnergize: &energize,
		},
	}

	// Seeded with the same default control this test asserts on: since the
	// program and its DefaultDERControl are now written at seed time, this
	// is the policy that reaches the store. Passing the same policy to
	// ApplyControlDelta below mirrors cmd/bridge, where both come from one
	// sep2config.SEP2Policy.
	seedPolicyForTest := testControlPolicy
	seedPolicyForTest.DefaultControl = seed
	reg, st := twoDeviceFixtureWithPolicy(t, seedPolicy{
		resolvePIN: testResolvePIN,
		control:    seedPolicyForTest,
	})
	notifier := coresub.NewManager(st.Subscriptions, 1, 10)
	ctx := context.Background()

	delta := diff.Difference{
		Object:    "mrid-a",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0.0, "value": 1000.0},
	}
	if err := ApplyControlDelta(ctx, st, notifier, reg, seedPolicyForTest, delta); err != nil {
		t.Fatalf("ApplyControlDelta: %v", err)
	}

	edevA := urlIndexFor(t, st, "mrid-a")

	// The DERProgram's own DefaultDERControlLink is populated.
	program, err := st.DERPrograms.ForParent(edevA).Get(ctx, controlDERProgramID)
	if err != nil {
		t.Fatalf("DERPrograms.Get: %v", err)
	}
	if program.DefaultDERControlLink == nil || program.DefaultDERControlLink.Href == "" {
		t.Fatalf("DERProgram.DefaultDERControlLink = %+v, want a populated href (CSIP-mandatory: Devi's finding)", program.DefaultDERControlLink)
	}

	// The link resolves: the store holds a DefaultDERControl at this
	// program's scope whose Href matches the link exactly.
	scope := derControlScope(edevA, controlFSAID, controlDERProgramID)
	dderc, err := st.DefaultDERControls.Get(ctx, scope, singletonKey)
	if err != nil {
		t.Fatalf("DefaultDERControls.Get: %v", err)
	}
	if dderc.Href != program.DefaultDERControlLink.Href {
		t.Errorf("DefaultDERControl.Href = %q, want it to match DERProgram.DefaultDERControlLink.Href %q", dderc.Href, program.DefaultDERControlLink.Href)
	}
	if dderc.DERControlBase == nil {
		t.Fatal("seeded DefaultDERControl.DERControlBase = nil, want a populated base")
	}

	// Positive invariant.
	if dderc.DERControlBase.OpModConnect == nil || !*dderc.DERControlBase.OpModConnect {
		t.Errorf("seeded DefaultDERControl.OpModConnect = %+v, want true", dderc.DERControlBase.OpModConnect)
	}
	if dderc.DERControlBase.OpModEnergize == nil || !*dderc.DERControlBase.OpModEnergize {
		t.Errorf("seeded DefaultDERControl.OpModEnergize = %+v, want true", dderc.DERControlBase.OpModEnergize)
	}

	// Negative invariants: the exact CSIP/1547 hazard this card guards
	// against is a stray value in any of these four fields.
	// opModTargetVar set would silently disable autonomous volt-var per
	// 1547-2018 clause 5.3; opModTargetW set would curtail PV;
	// setGradW/setSoftGradW set would overwrite the device's own
	// commissioned ramp with no randomization.
	if dderc.DERControlBase.OpModTargetW != nil {
		t.Errorf("seeded DefaultDERControl.OpModTargetW = %+v, want nil", dderc.DERControlBase.OpModTargetW)
	}
	if dderc.DERControlBase.OpModTargetVar != nil {
		t.Errorf("seeded DefaultDERControl.OpModTargetVar = %+v, want nil", dderc.DERControlBase.OpModTargetVar)
	}
	if dderc.SetGradW != nil {
		t.Errorf("seeded DefaultDERControl.SetGradW = %+v, want nil", dderc.SetGradW)
	}
	if dderc.SetSoftGradW != nil {
		t.Errorf("seeded DefaultDERControl.SetSoftGradW = %+v, want nil", dderc.SetSoftGradW)
	}

	// Owner scoping. Device B never had ApplyControlDelta called for it. It
	// now DOES have its own seeded DERProgram (every device does), so the
	// scoping question is no longer "does B have a program" but "is B's
	// program its own": a distinct identity, and carrying none of A's
	// control.
	edevB := urlIndexFor(t, st, "mrid-b")
	programB, err := st.DERPrograms.ForParent(edevB).Get(ctx, controlDERProgramID)
	if err != nil {
		t.Fatalf("DERPrograms.Get(B): %v (every seeded device has its own program)", err)
	}
	if programB.MRID == program.MRID {
		t.Errorf("device B's DERProgram.MRID = %q, the same as device A's; each device's program must have its own identity", programB.MRID)
	}
	if _, err := st.DERControls.Get(ctx, derControlScope(edevB, controlFSAID, controlDERProgramID), activeControlID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DERControls.Get(B) = (%v), want store.ErrNotFound (A's control must not leak to device B)", err)
	}
}

func TestDecodeActivePower(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   any
		want    sep2.ActivePower
		wantErr bool
	}{
		{"typed value passthrough", sep2.ActivePower{Multiplier: 2, Value: 42}, sep2.ActivePower{Multiplier: 2, Value: 42}, false},
		{"typed pointer passthrough", func() *sep2.ActivePower { v := sep2.ActivePower{Multiplier: -1, Value: 7}; return &v }(), sep2.ActivePower{Multiplier: -1, Value: 7}, false},
		{"json-decoded map", map[string]any{"multiplier": 0.0, "value": 5000.0}, sep2.ActivePower{Multiplier: 0, Value: 5000}, false},
		{"map missing value", map[string]any{"multiplier": 0.0}, sep2.ActivePower{}, true},
		{"map fractional value refused", map[string]any{"multiplier": 0.0, "value": 5000.5}, sep2.ActivePower{}, true},
		{"unsupported type", "not a power value", sep2.ActivePower{}, true},
		{"nil typed pointer", (*sep2.ActivePower)(nil), sep2.ActivePower{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeActivePower(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("decodeActivePower(%v): want error, got %+v", tt.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeActivePower(%v): unexpected error: %v", tt.value, err)
			}
			if *got != tt.want {
				t.Errorf("decodeActivePower(%v) = %+v, want %+v", tt.value, *got, tt.want)
			}
		})
	}
}

// TestApplyDERControlBaseFieldCoversEverySupportedField exercises every
// field applyDERControlBaseField supports (not just the two exercised
// end-to-end via ApplyControlDelta above), so this table, not the
// end-to-end tests, is the source of truth for "which fields are
// mapped" and "with what decode".
func TestApplyDERControlBaseFieldCoversEverySupportedField(t *testing.T) {
	t.Parallel()

	powerVal := map[string]any{"multiplier": 1.0, "value": 250.0}

	tests := []struct {
		name      string
		field     string
		value     any
		check     func(t *testing.T, base sep2.DERControlBase)
		wantErr   bool
		wantErrIs error
	}{
		{
			name:  "opModTargetW",
			field: "opModTargetW",
			value: powerVal,
			check: func(t *testing.T, base sep2.DERControlBase) {
				if base.OpModTargetW == nil || base.OpModTargetW.Value != 250 || base.OpModTargetW.Multiplier != 1 {
					t.Errorf("OpModTargetW = %+v, want {Multiplier:1 Value:250}", base.OpModTargetW)
				}
			},
		},
		{
			name:  "opModTargetVar",
			field: "opModTargetVar",
			value: powerVal,
			check: func(t *testing.T, base sep2.DERControlBase) {
				if base.OpModTargetVar == nil || base.OpModTargetVar.Value != 250 {
					t.Errorf("OpModTargetVar = %+v, want Value=250", base.OpModTargetVar)
				}
			},
		},
		{
			name:  "opModConnect",
			field: "opModConnect",
			value: true,
			check: func(t *testing.T, base sep2.DERControlBase) {
				if base.OpModConnect == nil || *base.OpModConnect != true {
					t.Errorf("OpModConnect = %v, want true", base.OpModConnect)
				}
			},
		},
		{
			name:  "opModEnergize",
			field: "opModEnergize",
			value: false,
			check: func(t *testing.T, base sep2.DERControlBase) {
				if base.OpModEnergize == nil || *base.OpModEnergize != false {
					t.Errorf("OpModEnergize = %v, want false", base.OpModEnergize)
				}
			},
		},
		{
			name:    "opModTargetW bad decode propagates",
			field:   "opModTargetW",
			value:   "not a power",
			wantErr: true,
		},
		{
			name:    "opModTargetVar bad decode propagates",
			field:   "opModTargetVar",
			value:   "not a power",
			wantErr: true,
		},
		{
			name:    "opModConnect bad decode propagates",
			field:   "opModConnect",
			value:   "not a bool",
			wantErr: true,
		},
		{
			name:    "unrecognized field",
			field:   "opModDoesNotExist",
			value:   true,
			wantErr: true,
		},
		// HIGH-1 (Vance, power-systems review of GAGO-034 PR #9):
		// opModFixedW/opModFixedVar/opModMaxLimW are IEEE 2030.5
		// PERCENT types (SignedPercent/PercentLimit/FixedVar), not
		// absolute watts/vars, and this bridge has no seeded
		// DERCapability rtg reference to convert a GridAPPS-D absolute
		// delta against. Mapping them would silently command the wrong
		// physical setpoint, so they are refused exactly like any other
		// unsupported attribute rather than mapped incorrectly. Percent
		// support returns once DERCapability is seeded: GAGO-045.
		{
			name:      "opModFixedW refused, not mapped as absolute power",
			field:     "opModFixedW",
			value:     powerVal,
			wantErr:   true,
			wantErrIs: ErrUnsupportedControlAttribute,
		},
		{
			name:      "opModFixedVar refused, not mapped as absolute power",
			field:     "opModFixedVar",
			value:     powerVal,
			wantErr:   true,
			wantErrIs: ErrUnsupportedControlAttribute,
		},
		{
			name:      "opModMaxLimW refused, not mapped as absolute power",
			field:     "opModMaxLimW",
			value:     powerVal,
			wantErr:   true,
			wantErrIs: ErrUnsupportedControlAttribute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var base sep2.DERControlBase
			err := applyDERControlBaseField(&base, tt.field, tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("applyDERControlBaseField(%q, %v): want error, got nil", tt.field, tt.value)
				}
				if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("applyDERControlBaseField(%q, %v) error = %v, want wrapping %v", tt.field, tt.value, err, tt.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("applyDERControlBaseField(%q, %v): unexpected error: %v", tt.field, tt.value, err)
			}
			tt.check(t, base)
		})
	}
}

// TestApplyControlDeltaRefusesPercentModeAttributes is the end-to-end
// (ApplyControlDelta, not just the field-mapper) proof for HIGH-1: a
// delta targeting one of the removed percent-mode attributes is refused
// via ErrUnsupportedControlAttribute and writes no DERControl anywhere,
// the same fail-closed shape as any other unsupported attribute.
func TestApplyControlDeltaRefusesPercentModeAttributes(t *testing.T) {
	t.Parallel()

	reg, st := twoDeviceFixture(t)
	notifier := coresub.NewManager(st.Subscriptions, 1, 10)
	ctx := context.Background()

	for _, attr := range []string{
		"DERControl.DERControlBase.opModFixedW",
		"DERControl.DERControlBase.opModFixedVar",
		"DERControl.DERControlBase.opModMaxLimW",
	} {
		t.Run(attr, func(t *testing.T) {
			delta := diff.Difference{
				Object:    "mrid-a",
				Attribute: attr,
				Value:     map[string]any{"multiplier": 0.0, "value": 1000.0},
			}
			err := ApplyControlDelta(ctx, st, notifier, reg, testControlPolicy, delta)
			if !errors.Is(err, ErrUnsupportedControlAttribute) {
				t.Fatalf("ApplyControlDelta(%q) error = %v, want ErrUnsupportedControlAttribute", attr, err)
			}
		})
	}

	edevA := urlIndexFor(t, st, "mrid-a")
	scopeA := derControlScope(edevA, controlFSAID, controlDERProgramID)
	if _, err := st.DERControls.Get(ctx, scopeA, activeControlID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DERControls.Get(A) after refused percent-mode deltas = (%v), want store.ErrNotFound", err)
	}
}

// TestSignFlipConstantsPinnedEffect is the regression lock HIGH-2 asks
// for: it asserts each of activeSignFlip / reactiveSignFlip's CURRENT
// numeric effect on a mapped value, so an accidental flip of either
// constant is caught by a failing test. This is NOT a claim about which
// physical direction is correct (see activeSignFlip's doc comment):
// only GAGO-044's co-simulation loopback can verify that. If a future
// change deliberately flips a constant, this test's want values must be
// updated in the same commit as the flip, with the commit message
// stating why (e.g. "GAGO-044 confirmed reactive sign is inverted").
func TestSignFlipConstantsPinnedEffect(t *testing.T) {
	t.Parallel()

	if activeSignFlip {
		t.Fatal("activeSignFlip pinned default changed to true; update this test's want value in the same commit and state why")
	}
	if reactiveSignFlip {
		t.Fatal("reactiveSignFlip pinned default changed to true; update this test's want value in the same commit and state why")
	}

	in := map[string]any{"multiplier": 0.0, "value": 1234.0}

	var base sep2.DERControlBase
	if err := applyDERControlBaseField(&base, "opModTargetW", in); err != nil {
		t.Fatalf("applyDERControlBaseField(opModTargetW): %v", err)
	}
	if base.OpModTargetW == nil || base.OpModTargetW.Value != 1234 {
		t.Errorf("with activeSignFlip=false, OpModTargetW.Value = %v, want 1234 (no negation)", base.OpModTargetW)
	}

	base = sep2.DERControlBase{}
	if err := applyDERControlBaseField(&base, "opModTargetVar", in); err != nil {
		t.Fatalf("applyDERControlBaseField(opModTargetVar): %v", err)
	}
	if base.OpModTargetVar == nil || base.OpModTargetVar.Value != 1234 {
		t.Errorf("with reactiveSignFlip=false, OpModTargetVar.Value = %v, want 1234 (no negation)", base.OpModTargetVar)
	}
}

// TestFlipActivePowerSign and TestFlipReactivePowerSign exercise the
// flip helpers directly (both the flip=true and flip=false, nil-safe
// branches), independent of which constant value is pinned today.
func TestFlipActivePowerSign(t *testing.T) {
	t.Parallel()

	ap := &sep2.ActivePower{Multiplier: 2, Value: 500}
	if got := flipActivePowerSign(ap, false); got.Value != 500 {
		t.Errorf("flipActivePowerSign(flip=false).Value = %d, want 500", got.Value)
	}
	if got := flipActivePowerSign(ap, true); got.Value != -500 {
		t.Errorf("flipActivePowerSign(flip=true).Value = %d, want -500", got.Value)
	}
	// Original must be unmutated by the flip=true branch.
	if ap.Value != 500 {
		t.Errorf("flipActivePowerSign mutated its input: ap.Value = %d, want 500", ap.Value)
	}
	if got := flipActivePowerSign(nil, true); got != nil {
		t.Errorf("flipActivePowerSign(nil, true) = %v, want nil", got)
	}
}

func TestFlipReactivePowerSign(t *testing.T) {
	t.Parallel()

	rp := &sep2.ReactivePower{Multiplier: -1, Value: 42}
	if got := flipReactivePowerSign(rp, false); got.Value != 42 {
		t.Errorf("flipReactivePowerSign(flip=false).Value = %d, want 42", got.Value)
	}
	if got := flipReactivePowerSign(rp, true); got.Value != -42 {
		t.Errorf("flipReactivePowerSign(flip=true).Value = %d, want -42", got.Value)
	}
	if rp.Value != 42 {
		t.Errorf("flipReactivePowerSign mutated its input: rp.Value = %d, want 42", rp.Value)
	}
	if got := flipReactivePowerSign(nil, true); got != nil {
		t.Errorf("flipReactivePowerSign(nil, true) = %v, want nil", got)
	}
}

func TestDecodeReactivePower(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   any
		want    sep2.ReactivePower
		wantErr bool
	}{
		{"typed value passthrough", sep2.ReactivePower{Multiplier: 1, Value: 99}, sep2.ReactivePower{Multiplier: 1, Value: 99}, false},
		{"typed pointer passthrough", func() *sep2.ReactivePower { v := sep2.ReactivePower{Multiplier: 0, Value: 12}; return &v }(), sep2.ReactivePower{Multiplier: 0, Value: 12}, false},
		{"json-decoded map", map[string]any{"multiplier": 0.0, "value": 500.0}, sep2.ReactivePower{Multiplier: 0, Value: 500}, false},
		{"nil typed pointer", (*sep2.ReactivePower)(nil), sep2.ReactivePower{}, true},
		{"unsupported type", 42, sep2.ReactivePower{}, true},
		{"map missing multiplier", map[string]any{"value": 1.0}, sep2.ReactivePower{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeReactivePower(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("decodeReactivePower(%v): want error, got %+v", tt.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeReactivePower(%v): unexpected error: %v", tt.value, err)
			}
			if *got != tt.want {
				t.Errorf("decodeReactivePower(%v) = %+v, want %+v", tt.value, *got, tt.want)
			}
		})
	}
}

func TestToFloat64(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		value  any
		want   float64
		wantOK bool
	}{
		{"float64", 3.5, 3.5, true},
		{"int", 7, 7, true},
		{"int64", int64(9), 9, true},
		{"unsupported", "nope", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := toFloat64(tt.value)
			if ok != tt.wantOK {
				t.Fatalf("toFloat64(%v) ok = %v, want %v", tt.value, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("toFloat64(%v) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestDecodeBool(t *testing.T) {
	t.Parallel()

	trueVal := true
	tests := []struct {
		name    string
		value   any
		want    bool
		wantErr bool
	}{
		{"bool true", true, true, false},
		{"bool false", false, false, false},
		{"pointer passthrough", &trueVal, true, false},
		{"nil pointer", (*bool)(nil), false, true},
		{"unsupported type", "yes", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeBool(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("decodeBool(%v): want error, got %v", tt.value, *got)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeBool(%v): unexpected error: %v", tt.value, err)
			}
			if *got != tt.want {
				t.Errorf("decodeBool(%v) = %v, want %v", tt.value, *got, tt.want)
			}
		})
	}
}
