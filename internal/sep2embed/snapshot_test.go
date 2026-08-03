package sep2embed

import (
	"context"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// testDefaultControlSnapshot is the GAGO-050 seed value newTestEmbed
// configures every Embed with in this file: a realistic, non-zero
// DefaultDERControl (opModConnect and opModEnergize true, everything
// else nil), mirroring sep2config.DefaultPolicy()'s own value without
// importing that package here (this file is testing sep2embed's own
// seeding mechanics, not sep2config's policy defaults).
func testDefaultControlSnapshot() sep2.DefaultDERControl {
	connect := true
	energize := true
	return sep2.DefaultDERControl{
		DERControlBase: &sep2.DERControlBase{
			OpModConnect:  &connect,
			OpModEnergize: &energize,
		},
	}
}

// newTestEmbed builds an Embed seeded from fixtureEntries, using ":0" so
// no fixed port is claimed, and returns it alongside the same reg for a
// caller that also wants to drive ApplyControlDelta. ctx scopes only
// the seeding writes inside New; see New's own doc comment.
func newTestEmbed(t *testing.T) (*Embed, *registry.Registry) {
	t.Helper()

	reg := registry.New()
	if err := reg.AddBatch(fixtureEntries()); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	e, err := New(context.Background(), Config{
		Addr:                   "127.0.0.1:0",
		CertDir:                t.TempDir(),
		ResolveRegistrationPIN: testResolvePIN,
		ShutdownTimeout:        time.Second,
		DefaultControl:         testDefaultControlSnapshot(),
		DefaultProgram:         testProgramSeed,
		DERControl:             testControlSeed,
	}, reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, reg
}

func TestEndDevicesReturnsSeededFieldValues(t *testing.T) {
	t.Parallel()

	e, _ := newTestEmbed(t)
	entries := fixtureEntries()

	snaps, err := e.EndDevices(context.Background())
	if err != nil {
		t.Fatalf("EndDevices: %v", err)
	}
	if len(snaps) != len(entries) {
		t.Fatalf("EndDevices returned %d snapshots, want %d", len(snaps), len(entries))
	}

	byLFDI := make(map[string]EndDeviceSnapshot, len(snaps))
	for _, s := range snaps {
		byLFDI[s.LFDI] = s
	}

	for _, want := range entries {
		// The snapshot is keyed by LFDI (identity), but ID and every href
		// carry the opaque URL index (addressing). Asserting both against
		// the right source is the point: an assertion that expected the
		// LFDI in the href would be asserting the bug this change removed.
		wantID := embedURLIndex(t, e, want.MRID)

		got, ok := byLFDI[want.LFDI]
		if !ok {
			t.Fatalf("EndDevices missing entry for LFDI %q", want.LFDI)
		}
		if got.ID != wantID {
			t.Errorf("EndDevice(%q).ID = %q, want %q", want.LFDI, got.ID, wantID)
		}
		if got.LFDI != want.LFDI {
			t.Errorf("EndDevice(%q).LFDI = %q, want %q: identity must stay the LFDI", want.LFDI, got.LFDI, want.LFDI)
		}
		if got.SFDI == "" {
			t.Errorf("EndDevice(%q).SFDI is empty, want a derived placeholder SFDI", want.LFDI)
		}
		if got.Href != "/edev/"+wantID {
			t.Errorf("EndDevice(%q).Href = %q, want %q", want.LFDI, got.Href, "/edev/"+wantID)
		}
		if !got.Enabled {
			t.Errorf("EndDevice(%q).Enabled = false, want true (seed.go always sets Enabled=true)", want.LFDI)
		}
		if len(got.DERs) != 1 {
			t.Fatalf("EndDevice(%q).DERs has %d items, want 1 (seed.go seeds exactly one DER per device)", want.LFDI, len(got.DERs))
		}
		wantDERHref := "/edev/" + wantID + "/der/1"
		if got.DERs[0].Href != wantDERHref {
			t.Errorf("EndDevice(%q).DERs[0].Href = %q, want %q", want.LFDI, got.DERs[0].Href, wantDERHref)
		}
		if got.DERs[0].ID != "1" {
			t.Errorf("EndDevice(%q).DERs[0].ID = %q, want %q", want.LFDI, got.DERs[0].ID, "1")
		}
	}
}

func TestEndDevicesOnEmptyRegistryReturnsEmptyNotNilError(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	e, err := New(context.Background(), Config{Addr: "127.0.0.1:0", CertDir: t.TempDir(), ShutdownTimeout: time.Second, ResolveRegistrationPIN: testResolvePIN}, reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	snaps, err := e.EndDevices(context.Background())
	if err != nil {
		t.Fatalf("EndDevices on empty registry: %v", err)
	}
	if len(snaps) != 0 {
		t.Fatalf("EndDevices on empty registry returned %d snapshots, want 0", len(snaps))
	}
}

// TestDefaultDERControlIsReachableBeforeAnyDeltaApplied pins the reason the
// default DERProgram is seeded at boot rather than created lazily.
//
// DefaultDERControl is reachable only through its containing DERProgram's
// DefaultDERControlLink. While the program was created lazily, on the first
// control delta for a device, a device that had never been controlled served
// an empty DERProgramList, so the operator's configured DefaultDERControl,
// the control that applies when no event is active, could not be reached at
// all. CSIP is explicit that this is the resource a DER falls back to: "in
// the absence of any active events, the inverter executes the
// DefaultDERControl of the DERProgram with the highest priority"
// (CSIP Implementation Guide v2.0, section 8).
//
// This test asserts the inverse of what it used to: the singleton is present
// on a freshly seeded device that has received no delta.
func TestDefaultDERControlIsReachableBeforeAnyDeltaApplied(t *testing.T) {
	t.Parallel()

	e, _ := newTestEmbed(t)
	entries := fixtureEntries()
	edevID := embedURLIndex(t, e, entries[0].MRID)

	snap, err := e.DefaultDERControl(context.Background(), edevID, controlFSAID, controlDERProgramID)
	if err != nil {
		t.Fatalf("DefaultDERControl: %v", err)
	}
	if snap == nil {
		t.Fatal("DefaultDERControl before any delta = nil; the configured default control is unreachable until a control arrives, which is the defect seeding the program exists to fix")
	}

	// Value, not just presence: the seeded singleton must carry the
	// operator's configured control rather than a zero one, and a
	// schema-valid mRID.
	wantHref := "/edev/" + edevID + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID + "/dderc"
	if snap.Href != wantHref {
		t.Errorf("DefaultDERControl.Href = %q, want %q", snap.Href, wantHref)
	}
	assertValidMRID(t, "DefaultDERControl.MRID", snap.MRID)
	if snap.Base == nil {
		t.Fatal("DefaultDERControl.Base is nil, want the configured DERControlBase")
	}
	if snap.Base.OpModConnect == nil || !*snap.Base.OpModConnect {
		t.Errorf("DefaultDERControl.Base.OpModConnect = %v, want true (the configured value from testDefaultControlSnapshot)", snap.Base.OpModConnect)
	}
}

// TestDERProgramsAndDERControlsReflectAppliedDelta drives one real
// control delta through ApplyControlDelta (the same DOWN path
// runControlSubscriber will call in cmd/bridge), then asserts the
// GAGO-056 accessors report exactly the field values that delta wrote:
// not just non-nil, per data-invariants.
func TestDERProgramsAndDERControlsReflectAppliedDelta(t *testing.T) {
	t.Parallel()

	e, reg := newTestEmbed(t)
	entries := fixtureEntries()
	targetMRID := entries[0].MRID
	edevID := embedURLIndex(t, e, entries[0].MRID)

	delta := diff.Difference{
		Object:    targetMRID,
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0, "value": int64(4200)},
	}
	if err := e.ApplyControlDelta(context.Background(), reg, delta); err != nil {
		t.Fatalf("ApplyControlDelta: %v", err)
	}

	programs, err := e.DERPrograms(context.Background(), edevID)
	if err != nil {
		t.Fatalf("DERPrograms: %v", err)
	}
	if len(programs) != 1 {
		t.Fatalf("DERPrograms returned %d items, want 1 (seeding creates exactly one, and the delta must reuse it rather than add a second)", len(programs))
	}
	if programs[0].ID != controlDERProgramID {
		t.Errorf("DERPrograms[0].ID = %q, want %q", programs[0].ID, controlDERProgramID)
	}
	wantProgramHref := "/edev/" + edevID + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID
	if programs[0].Href != wantProgramHref {
		t.Errorf("DERPrograms[0].Href = %q, want %q", programs[0].Href, wantProgramHref)
	}
	// Primacy comes from the configured policy (testProgramSeed), not from
	// a value hardcoded in the control path. Asserting the configured value
	// rather than a literal 1 is what makes this a test of the policy
	// plumbing instead of a restatement of a constant.
	if programs[0].Primacy != testProgramSeed.Primacy {
		t.Errorf("DERPrograms[0].Primacy = %d, want %d (the configured DefaultProgram.Primacy)", programs[0].Primacy, testProgramSeed.Primacy)
	}
	// GAGO-050: every DERProgram carries a non-empty DefaultDERControlLink,
	// and it resolves to the seeded DefaultDERControl below (asserted
	// after the dderc fetch, so the same href is checked from both the
	// program's own link and the singleton's own Href field).
	if programs[0].DefaultDERControlLink == "" {
		t.Fatalf("DERPrograms[0].DefaultDERControlLink is empty, want a populated href (CSIP-mandatory: Devi's finding)")
	}

	controls, err := e.DERControls(context.Background(), edevID, controlFSAID, controlDERProgramID)
	if err != nil {
		t.Fatalf("DERControls: %v", err)
	}
	if len(controls) != 1 {
		t.Fatalf("DERControls returned %d items, want 1", len(controls))
	}
	got := controls[0]
	// The snapshot's ID is the store key the control is actually held at,
	// which since GAGO-133 is per-event rather than the fixed "active" slot.
	// The href is asserted to END with that id rather than to equal a literal:
	// the id embeds the event's own creation instant and mRID, so a literal
	// would have to be recomputed here and would then agree with the
	// implementation by construction.
	if got.ID == "" {
		t.Error("DERControls[0].ID is empty, want the control's store key")
	}
	wantControlPrefix := "/edev/" + edevID + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID + "/derc/"
	if got.Href != wantControlPrefix+got.ID {
		t.Errorf("DERControls[0].Href = %q, want %q", got.Href, wantControlPrefix+got.ID)
	}
	if got.CurrentStatus != 1 { // sep2.EventStatusActive
		t.Errorf("DERControls[0].CurrentStatus = %d, want 1 (EventStatusActive)", got.CurrentStatus)
	}
	if got.DateTime == 0 {
		t.Errorf("DERControls[0].DateTime is zero, want a real unix timestamp")
	}
	if got.Base == nil {
		t.Fatalf("DERControls[0].Base is nil, want a populated DERControlBaseSnapshot")
	}
	if got.Base.OpModTargetW == nil {
		t.Fatalf("DERControls[0].Base.OpModTargetW is nil, want the applied delta's value")
	}
	if got.Base.OpModTargetW.Value != 4200 {
		t.Errorf("DERControls[0].Base.OpModTargetW.Value = %d, want 4200", got.Base.OpModTargetW.Value)
	}
	if got.Base.OpModTargetW.Multiplier != 0 {
		t.Errorf("DERControls[0].Base.OpModTargetW.Multiplier = %d, want 0", got.Base.OpModTargetW.Multiplier)
	}
	if got.Base.OpModTargetVar != nil {
		t.Errorf("DERControls[0].Base.OpModTargetVar = %+v, want nil (delta never touched this field)", got.Base.OpModTargetVar)
	}

	// GAGO-050: ensureDERProgram seeds the DefaultDERControl singleton at
	// the same moment it lazily creates the DERProgram itself (the first
	// ApplyControlDelta for this device, above), from the Embed's own
	// configured DefaultControl (testDefaultControlSnapshot, set in
	// newTestEmbed). The accessor must now report that seeded value, not
	// nil: the CSIP-mandatory hole Devi flagged is exactly a client
	// following DefaultDERControlLink and finding nothing there.
	dderc, err := e.DefaultDERControl(context.Background(), edevID, controlFSAID, controlDERProgramID)
	if err != nil {
		t.Fatalf("DefaultDERControl after delta: %v", err)
	}
	if dderc == nil {
		t.Fatal("DefaultDERControl after ensureDERProgram = nil, want the seeded default (CSIP-mandatory: Devi's finding)")
	}
	if dderc.Href != programs[0].DefaultDERControlLink {
		t.Errorf("DefaultDERControl.Href = %q, want it to match DERProgram's own DefaultDERControlLink %q", dderc.Href, programs[0].DefaultDERControlLink)
	}
	if dderc.Base == nil {
		t.Fatal("DefaultDERControl.Base = nil, want a populated DERControlBaseSnapshot")
	}

	// Positive invariant: opModConnect and opModEnergize are both true,
	// exactly as sep2config.DefaultPolicy() (and this file's
	// testDefaultControlSnapshot) configure them.
	if dderc.Base.OpModConnect == nil || !*dderc.Base.OpModConnect {
		t.Errorf("DefaultDERControl.Base.OpModConnect = %+v, want true", dderc.Base.OpModConnect)
	}
	if dderc.Base.OpModEnergize == nil || !*dderc.Base.OpModEnergize {
		t.Errorf("DefaultDERControl.Base.OpModEnergize = %+v, want true", dderc.Base.OpModEnergize)
	}

	// Negative invariant (the actual hazard this card guards against):
	// opModTargetW and opModTargetVar must stay nil on the seeded
	// default. A stray value here would silently disable the device's
	// own autonomous volt-var / curtailment behavior per IEEE 1547-2018
	// clause 5.3's mutual exclusivity, exactly the failure mode Vance's
	// physics verdict warned about.
	if dderc.Base.OpModTargetW != nil {
		t.Errorf("DefaultDERControl.Base.OpModTargetW = %+v, want nil (stray value would curtail PV)", dderc.Base.OpModTargetW)
	}
	if dderc.Base.OpModTargetVar != nil {
		t.Errorf("DefaultDERControl.Base.OpModTargetVar = %+v, want nil (stray value would disable autonomous volt-var per 1547-2018 5.3)", dderc.Base.OpModTargetVar)
	}
}

// TestDERControlsScopedToUncontrolledDeviceReturnsEmptyButProgramExists
// pins the two halves of the seeding contract that pull in opposite
// directions on a device no control delta has ever targeted:
//
//   - Its DERControlList is empty. Seeding a program must NOT fabricate a
//     control; a control means an active event, and inventing one would put
//     the DER under a command no operator issued.
//   - Its DERProgramList is NOT empty. The program is the operator's control
//     channel and the only route to DefaultDERControl, so it exists from
//     boot whether or not a control is currently active.
//
// The empty control list is also a valid empty List result rather than a
// not-found error, unlike the Get-based DefaultDERControl path.
func TestDERControlsScopedToUncontrolledDeviceReturnsEmptyButProgramExists(t *testing.T) {
	t.Parallel()

	e, _ := newTestEmbed(t)
	entries := fixtureEntries()
	edevID := embedURLIndex(t, e, entries[1].MRID) // never had a delta applied

	controls, err := e.DERControls(context.Background(), edevID, controlFSAID, controlDERProgramID)
	if err != nil {
		t.Fatalf("DERControls on uncontrolled scope: %v", err)
	}
	if len(controls) != 0 {
		t.Fatalf("DERControls on uncontrolled scope returned %d items, want 0 (seeding a program must not fabricate a control)", len(controls))
	}

	programs, err := e.DERPrograms(context.Background(), edevID)
	if err != nil {
		t.Fatalf("DERPrograms on uncontrolled device: %v", err)
	}
	if len(programs) != 1 {
		t.Fatalf("DERPrograms on uncontrolled device returned %d items, want 1 (the program is seeded at boot, not on first control)", len(programs))
	}
	if programs[0].Primacy != testProgramSeed.Primacy {
		t.Errorf("DERPrograms[0].Primacy = %d, want %d (the configured DefaultProgram.Primacy)", programs[0].Primacy, testProgramSeed.Primacy)
	}
	if programs[0].DefaultDERControlLink == "" {
		t.Error("DERPrograms[0].DefaultDERControlLink is empty on an uncontrolled device; the configured default control is unreachable")
	}
}
