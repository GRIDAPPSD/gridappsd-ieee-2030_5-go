package sep2embed

import (
	"context"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

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

	e, err := New(context.Background(), Config{Addr: "127.0.0.1:0", CertDir: t.TempDir(), ShutdownTimeout: time.Second}, reg)
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
		got, ok := byLFDI[want.LFDI]
		if !ok {
			t.Fatalf("EndDevices missing entry for LFDI %q", want.LFDI)
		}
		if got.ID != want.LFDI {
			t.Errorf("EndDevice(%q).ID = %q, want %q", want.LFDI, got.ID, want.LFDI)
		}
		if got.SFDI == "" {
			t.Errorf("EndDevice(%q).SFDI is empty, want a derived placeholder SFDI", want.LFDI)
		}
		if got.Href != "/edev/"+want.LFDI {
			t.Errorf("EndDevice(%q).Href = %q, want %q", want.LFDI, got.Href, "/edev/"+want.LFDI)
		}
		if !got.Enabled {
			t.Errorf("EndDevice(%q).Enabled = false, want true (seed.go always sets Enabled=true)", want.LFDI)
		}
		if len(got.DERs) != 1 {
			t.Fatalf("EndDevice(%q).DERs has %d items, want 1 (seed.go seeds exactly one DER per device)", want.LFDI, len(got.DERs))
		}
		wantDERHref := "/edev/" + want.LFDI + "/der/1"
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
	e, err := New(context.Background(), Config{Addr: "127.0.0.1:0", CertDir: t.TempDir(), ShutdownTimeout: time.Second}, reg)
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

// TestDefaultDERControlReturnsNilBeforeAnyDeltaApplied locks in the
// not-found-is-not-an-error contract documented on DefaultDERControl:
// core's own DefaultDERControlHandler serves a default value (HTTP 200)
// on a missing singleton rather than 404, and this bridge never writes
// a DefaultDERControl at all (only the "active" DERControl via
// ApplyControlDelta), so the accessor must report "none written" as
// (nil, nil), not as an error.
func TestDefaultDERControlReturnsNilBeforeAnyDeltaApplied(t *testing.T) {
	t.Parallel()

	e, _ := newTestEmbed(t)
	entries := fixtureEntries()
	edevID := entries[0].LFDI

	snap, err := e.DefaultDERControl(context.Background(), edevID, controlFSAID, controlDERProgramID)
	if err != nil {
		t.Fatalf("DefaultDERControl: %v", err)
	}
	if snap != nil {
		t.Fatalf("DefaultDERControl before any write = %+v, want nil", snap)
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
	edevID := entries[0].LFDI

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
		t.Fatalf("DERPrograms returned %d items, want 1 (ensureDERProgram creates exactly one)", len(programs))
	}
	if programs[0].ID != controlDERProgramID {
		t.Errorf("DERPrograms[0].ID = %q, want %q", programs[0].ID, controlDERProgramID)
	}
	wantProgramHref := "/edev/" + edevID + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID
	if programs[0].Href != wantProgramHref {
		t.Errorf("DERPrograms[0].Href = %q, want %q", programs[0].Href, wantProgramHref)
	}
	if programs[0].Primacy != 1 {
		t.Errorf("DERPrograms[0].Primacy = %d, want 1 (ensureDERProgram always writes Primacy: 1)", programs[0].Primacy)
	}

	controls, err := e.DERControls(context.Background(), edevID, controlFSAID, controlDERProgramID)
	if err != nil {
		t.Fatalf("DERControls: %v", err)
	}
	if len(controls) != 1 {
		t.Fatalf("DERControls returned %d items, want 1", len(controls))
	}
	got := controls[0]
	if got.ID != activeControlID {
		t.Errorf("DERControls[0].ID = %q, want %q", got.ID, activeControlID)
	}
	wantControlHref := "/edev/" + edevID + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID + "/derc/" + activeControlID
	if got.Href != wantControlHref {
		t.Errorf("DERControls[0].Href = %q, want %q", got.Href, wantControlHref)
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

	// The DefaultDERControl accessor must still report nil: the delta
	// wrote a DERControl, never a DefaultDERControl singleton.
	dderc, err := e.DefaultDERControl(context.Background(), edevID, controlFSAID, controlDERProgramID)
	if err != nil {
		t.Fatalf("DefaultDERControl after delta: %v", err)
	}
	if dderc != nil {
		t.Fatalf("DefaultDERControl after DERControl-only delta = %+v, want nil", dderc)
	}
}

// TestDERControlsScopedToUnknownDeviceReturnsEmpty confirms the
// accessor does not error, and returns no items, for a scope that has
// never had ensureDERProgram or ApplyControlDelta run against it: a
// bare List against an unpopulated scope key is a valid empty result,
// not a not-found error (unlike the Get-based DefaultDERControl path).
func TestDERControlsScopedToUnknownDeviceReturnsEmpty(t *testing.T) {
	t.Parallel()

	e, _ := newTestEmbed(t)
	entries := fixtureEntries()
	edevID := entries[1].LFDI // never had a delta applied

	controls, err := e.DERControls(context.Background(), edevID, controlFSAID, controlDERProgramID)
	if err != nil {
		t.Fatalf("DERControls on untouched scope: %v", err)
	}
	if len(controls) != 0 {
		t.Fatalf("DERControls on untouched scope returned %d items, want 0", len(controls))
	}

	programs, err := e.DERPrograms(context.Background(), edevID)
	if err != nil {
		t.Fatalf("DERPrograms on untouched device: %v", err)
	}
	if len(programs) != 0 {
		t.Fatalf("DERPrograms on untouched device returned %d items, want 0", len(programs))
	}
}
