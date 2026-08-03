package sep2embed

import (
	"context"
	"encoding/xml"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// TestConfigCarriesNoBusPublishSurface is the layering regression guard
// for GAGO-121. Receiving an IEEE 2030.5 request must not cause a
// GridAPPS-D bus publish, so this package must not take a bus, a bus
// destination, or a simulation id at all. Publishing is
// internal/telemetrypub's job, driven by its own timer off this
// package's store.
//
// Asserted structurally rather than behaviourally because the whole
// point is that the capability is ABSENT: there is no publish to
// observe not happening. A future edit that re-threads a publisher
// through Config to "just echo this one resource" fails here.
func TestConfigCarriesNoBusPublishSurface(t *testing.T) {
	t.Parallel()

	banned := []string{"Bus", "TelemetryDestination", "TelemetrySimulationID", "Publisher", "Destination"}
	cfgType := reflect.TypeOf(Config{})
	for _, name := range banned {
		if _, found := cfgType.FieldByName(name); found {
			t.Errorf("sep2embed.Config has field %q: the 2030.5 embed must not know about the GridAPPS-D bus. "+
				"Publishing belongs to internal/telemetrypub, off the store, on its own timer.", name)
		}
	}
}

// TestDERStatusPUTStoresAndDoesNotPublish is the request-path contract
// after GAGO-121: a device's DERStatus PUT is answered and stored, and
// that is the entire server-side effect. The values asserted here are
// the ones internal/telemetrypub will later read and publish, so this
// also pins that the store, not the request, is the seam between the
// two layers.
func TestDERStatusPUTStoresAndDoesNotPublish(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()
	_, _, caFile, err := ensureServerIdentity(certDir)
	if err != nil {
		t.Fatalf("ensureServerIdentity: %v", err)
	}
	caCertPEM, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("read ca.pem: %v", err)
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(certDir, caKeyFileName))
	if err != nil {
		t.Fatalf("read ca-key.pem: %v", err)
	}
	caCert, caKey, err := parseCAPair(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("parseCAPair: %v", err)
	}

	certA, keyA, lfdiA := mintDeviceIdentity(t, caCert, caKey, "test-serial-nopublish-a")

	reg := registry.New()
	if err := reg.AddBatch([]registry.Entry{
		{MRID: "mrid-nopublish-a", Name: "Device NoPublish A", LFDI: lfdiA},
	}); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	e, err := New(ctx, Config{
		Addr:                   "127.0.0.1:0",
		CertDir:                certDir,
		ResolveRegistrationPIN: testResolvePIN,
		ShutdownTimeout:        time.Second,
	}, reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runErr:
			if err != nil {
				t.Errorf("Run returned error after ctx cancel: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Run did not return within 3s of ctx cancel (goroutine leak)")
		}
	})

	edevA := embedURLIndex(t, e, "mrid-nopublish-a")
	client := deviceClient(t, certA, keyA, caCertPEM)

	soc := sep2.StateOfChargeStatusType{Value: 6500, DateTime: 42}
	mode := sep2.OperationalModeStatusType{Value: 2, DateTime: 43}
	put := sep2.DERStatus{StateOfChargeStatus: &soc, OperationalModeStatus: &mode, ReadingTime: 1785714218}
	body, err := xml.Marshal(&put)
	if err != nil {
		t.Fatalf("marshal DERStatus: %v", err)
	}

	assertStatus(t, client, http.MethodPut, "https://"+e.Addr()+"/edev/"+edevA+"/der/1/ders",
		body, http.StatusNoContent, "device A PUTting its own DERStatus")

	// The stored resource carries exactly what the device sent: this is
	// what the publisher reads on its next interval, so a lossy or
	// rewritten store would change what reaches the bus.
	stored, err := e.stores.DERStatuses.Get(ctx, edevA+"/1", singletonKey)
	if err != nil {
		t.Fatalf("DERStatuses.Get after PUT: %v", err)
	}
	if stored.StateOfChargeStatus == nil || stored.StateOfChargeStatus.Value != 6500 {
		t.Errorf("stored stateOfChargeStatus = %+v, want value 6500", stored.StateOfChargeStatus)
	}
	if stored.OperationalModeStatus == nil || stored.OperationalModeStatus.Value != 2 {
		t.Errorf("stored operationalModeStatus = %+v, want value 2", stored.OperationalModeStatus)
	}
	if stored.ReadingTime != 1785714218 {
		t.Errorf("stored readingTime = %d, want 1785714218", stored.ReadingTime)
	}

	// And the same values are what the publisher's read seam reports,
	// paired with this device's own CIM mRID.
	snaps, err := e.DERStatusSnapshots(ctx)
	if err != nil {
		t.Fatalf("DERStatusSnapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("DERStatusSnapshots returned %d snapshots, want 1", len(snaps))
	}
	if snaps[0].MRID != "mrid-nopublish-a" {
		t.Errorf("snapshot MRID = %q, want %q", snaps[0].MRID, "mrid-nopublish-a")
	}
	if snaps[0].Status.StateOfChargeStatus == nil || snaps[0].Status.StateOfChargeStatus.Value != 6500 {
		t.Errorf("snapshot stateOfChargeStatus = %+v, want value 6500", snaps[0].Status.StateOfChargeStatus)
	}
}
