package sep2embed

import (
	"context"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// seededEmbedForStatus builds an Embed backed by real, seeded stores and
// no listener at all. DERStatusSnapshots reads only e.stores, so a
// struct literal is enough here and no certificate material or bound
// port is minted for a pure read-path test.
func seededEmbedForStatus(t *testing.T, mrids []string) (*Embed, *registry.Registry) {
	t.Helper()

	reg := fixtureRegistryFor(t, mrids)
	stores := newStores()
	if err := seedStores(context.Background(), stores, reg, seedPolicy{resolvePIN: testResolvePIN}); err != nil {
		t.Fatalf("seedStores: %v", err)
	}
	return &Embed{stores: stores}, reg
}

// putStatus writes status into the DERStatus store under exactly the
// scope core's singleton handler uses for PUT /edev/{id}/der/{derId}/ders
// (parent key "{id}/{derId}", resource key "default"), so this test
// exercises the same store layout a real device PUT produces.
func putStatus(t *testing.T, e *Embed, edevID, derID string, status sep2.DERStatus) {
	t.Helper()
	if err := e.stores.DERStatuses.Create(context.Background(), edevID+"/"+derID, singletonKey, status); err != nil {
		t.Fatalf("DERStatuses.Create(%s/%s): %v", edevID, derID, err)
	}
}

// TestDERStatusSnapshotsPairsEveryStatusWithItsOwnDeviceMRID is the
// identity invariant for the whole UP path: the aggregate publisher
// keys every published value off the mRID this method reports, so a
// snapshot that paired device A's status with device B's mRID would
// publish A's telemetry under B's identity. Two devices are seeded, each
// with a distinguishable status value, so a crossed pairing fails here
// rather than silently reaching the bus.
func TestDERStatusSnapshotsPairsEveryStatusWithItsOwnDeviceMRID(t *testing.T) {
	t.Parallel()

	e, _ := seededEmbedForStatus(t, []string{"mrid-a", "mrid-b"})
	ctx := context.Background()

	edevA := urlIndexFor(t, e.stores, "mrid-a")
	edevB := urlIndexFor(t, e.stores, "mrid-b")

	modeA := sep2.OperationalModeStatusType{Value: 2}
	modeB := sep2.OperationalModeStatusType{Value: 3}
	putStatus(t, e, edevA, "1", sep2.DERStatus{OperationalModeStatus: &modeA, ReadingTime: 1700000001})
	putStatus(t, e, edevB, "1", sep2.DERStatus{OperationalModeStatus: &modeB, ReadingTime: 1700000002})

	snaps, err := e.DERStatusSnapshots(ctx)
	if err != nil {
		t.Fatalf("DERStatusSnapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("DERStatusSnapshots returned %d snapshots, want 2", len(snaps))
	}

	byMRID := make(map[string]DERStatusSnapshot, len(snaps))
	for _, s := range snaps {
		byMRID[s.MRID] = s
	}

	a, ok := byMRID["mrid-a"]
	if !ok {
		t.Fatalf("no snapshot for mrid-a; got %+v", snaps)
	}
	if a.EDevID != edevA {
		t.Errorf("mrid-a snapshot EDevID = %q, want %q", a.EDevID, edevA)
	}
	if a.DERID != "1" {
		t.Errorf("mrid-a snapshot DERID = %q, want %q", a.DERID, "1")
	}
	if a.Status.OperationalModeStatus == nil || a.Status.OperationalModeStatus.Value != 2 {
		t.Errorf("mrid-a operationalModeStatus = %+v, want value 2", a.Status.OperationalModeStatus)
	}
	if a.Status.ReadingTime != 1700000001 {
		t.Errorf("mrid-a readingTime = %d, want 1700000001", a.Status.ReadingTime)
	}

	b, ok := byMRID["mrid-b"]
	if !ok {
		t.Fatalf("no snapshot for mrid-b; got %+v", snaps)
	}
	if b.EDevID != edevB {
		t.Errorf("mrid-b snapshot EDevID = %q, want %q", b.EDevID, edevB)
	}
	if b.Status.OperationalModeStatus == nil || b.Status.OperationalModeStatus.Value != 3 {
		t.Errorf("mrid-b operationalModeStatus = %+v, want value 3", b.Status.OperationalModeStatus)
	}
	if b.Status.ReadingTime != 1700000002 {
		t.Errorf("mrid-b readingTime = %d, want 1700000002", b.Status.ReadingTime)
	}
}

// TestDERStatusSnapshotsOmitsDevicesThatNeverPUT proves the snapshot
// reports only stored statuses: a seeded device that has never PUT one
// is absent, not present with a zero-valued DERStatus. A fabricated
// zero status would publish a synthetic all-unset reading for a device
// that never reported, which is exactly the invisible-data class the
// data-invariants rule forbids.
func TestDERStatusSnapshotsOmitsDevicesThatNeverPUT(t *testing.T) {
	t.Parallel()

	e, _ := seededEmbedForStatus(t, []string{"mrid-a", "mrid-quiet"})
	edevA := urlIndexFor(t, e.stores, "mrid-a")

	mode := sep2.OperationalModeStatusType{Value: 1}
	putStatus(t, e, edevA, "1", sep2.DERStatus{OperationalModeStatus: &mode})

	snaps, err := e.DERStatusSnapshots(context.Background())
	if err != nil {
		t.Fatalf("DERStatusSnapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("DERStatusSnapshots returned %d snapshots, want 1 (only the device that PUT)", len(snaps))
	}
	if snaps[0].MRID != "mrid-a" {
		t.Errorf("snapshot MRID = %q, want %q", snaps[0].MRID, "mrid-a")
	}
}

// TestDERStatusSnapshotsEmptyWhenNothingStored is the no-device-reported
// case the publisher's "publish nothing" branch depends on: an empty,
// non-nil result and no error, not a synthesized entry per seeded device.
func TestDERStatusSnapshotsEmptyWhenNothingStored(t *testing.T) {
	t.Parallel()

	e, _ := seededEmbedForStatus(t, []string{"mrid-a"})

	snaps, err := e.DERStatusSnapshots(context.Background())
	if err != nil {
		t.Fatalf("DERStatusSnapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Fatalf("DERStatusSnapshots returned %d snapshots, want 0", len(snaps))
	}
}

// TestDERStatusSnapshotsIsRaceFreeAgainstConcurrentPUTs runs the reader
// against concurrent writes to the same store the protocol handler
// writes on a PUT. The assertion is the race detector plus a
// well-formed result: every returned snapshot still carries the mRID of
// the device its status was stored under.
func TestDERStatusSnapshotsIsRaceFreeAgainstConcurrentPUTs(t *testing.T) {
	t.Parallel()

	e, _ := seededEmbedForStatus(t, []string{"mrid-a", "mrid-b"})
	ctx := context.Background()
	edevA := urlIndexFor(t, e.stores, "mrid-a")
	edevB := urlIndexFor(t, e.stores, "mrid-b")

	scopes := map[string]string{"mrid-a": edevA, "mrid-b": edevB}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			mode := sep2.OperationalModeStatusType{Value: uint8(i % 5)}
			status := sep2.DERStatus{OperationalModeStatus: &mode, ReadingTime: int64(i)}
			for _, edevID := range scopes {
				st := e.stores.DERStatuses.ForParent(edevID + "/1")
				if err := st.Create(ctx, singletonKey, status); err != nil {
					// Already exists after the first round: update instead,
					// which is exactly what core's singleton PUT does.
					if uerr := st.Update(ctx, singletonKey, status); uerr != nil {
						t.Errorf("update DERStatus: %v", uerr)
						return
					}
				}
			}
		}
	}()

	for range 200 {
		snaps, err := e.DERStatusSnapshots(ctx)
		if err != nil {
			t.Fatalf("DERStatusSnapshots: %v", err)
		}
		for _, s := range snaps {
			wantEDev, ok := scopes[s.MRID]
			if !ok {
				t.Fatalf("snapshot carried unknown MRID %q", s.MRID)
			}
			if s.EDevID != wantEDev {
				t.Fatalf("snapshot for %q carried EDevID %q, want %q", s.MRID, s.EDevID, wantEDev)
			}
		}
	}
	<-done
}
