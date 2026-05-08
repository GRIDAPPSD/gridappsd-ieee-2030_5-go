//go:build gridappsd

// Integration tests for the four GAGO-026 SPARQL templates against the
// real GridAPPS-D platform stack (gridappsd-docker via
// `pixi run gridappsd-start` from sentient_gridappsd_integration).
//
// Run with:
//
//	make test-gridappsd
//
// or, if the platform is already up,
//
//	go test -tags=gridappsd -race -timeout 3m -v ./internal/cim/
//
// If the broker is unreachable, every test in this file is skipped so
// `go test ./...` (and CI without the platform) stays green.

package cim

import (
	"context"
	"testing"
	"time"

	"github.com/go-stomp/stomp/v3"

	"github.com/GRIDAPPSD/gridappsd-2030_5-go/internal/cimstomp"
)

const (
	gridappsdAddr     = "127.0.0.1:61613"
	gridappsdUser     = "system"
	gridappsdPassword = "manager"

	// ieee123pvFeederMRID is the standard IEEE 123-bus feeder shipped
	// with gridappsd-docker:develop. The dataset stores
	// c:IdentifiedObject.mRID as a bare uppercase UUID; the wrapper
	// layer accepts either form (with or without leading underscore)
	// and normalizes to bare. See queryFeederTemplate.
	ieee123pvFeederMRID = "E407CBB6-8C8D-9BC9-589C-AB83FBF0826D"
)

// requireGridAPPSD dials the platform broker as a smoke test and skips
// the test if the platform is not running. Mirrors the cimstomp
// package's gridappsd-tagged tests so the skip behavior is consistent
// across packages.
func requireGridAPPSD(t *testing.T) {
	t.Helper()
	conn, err := stomp.Dial("tcp", gridappsdAddr,
		stomp.ConnOpt.Login(gridappsdUser, gridappsdPassword),
		stomp.ConnOpt.HeartBeat(5*time.Second, 5*time.Second),
	)
	if err != nil {
		t.Skipf("GridAPPS-D platform not reachable at %s: %v "+
			"(bring it up with `pixi run gridappsd-start` from "+
			"~/repos/sentient_gridappsd_integration/)",
			gridappsdAddr, err)
	}
	_ = conn.Disconnect()
}

// newGridAPPSDClient connects a cimstomp.Client to the live platform
// broker, wraps it in a cim.Client, and registers a Cleanup to close
// the connection at test end.
func newGridAPPSDClient(t *testing.T) *Client {
	t.Helper()
	requireGridAPPSD(t)
	cs := cimstomp.NewClient(cimstomp.STOMPConfig{
		Address:  gridappsdAddr,
		User:     gridappsdUser,
		Password: gridappsdPassword,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cs.Connect(ctx); err != nil {
		t.Fatalf("cimstomp.Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return NewClient(cs)
}

// TestGridAPPSD_QueryInverter_IEEE123pv asserts the GAGO-026 template
// returns the expected 14 PowerElectronicsConnection rows against the
// IEEE 123pv feeder in gridappsd-docker:develop. The pre-fix template
// returned 0 rows because the inner join on
// c:PowerElectronicsConnection.PowerElectronicsUnit filtered out every
// PEC (the dataset has no child Units).
//
// The wrapper accepts both forms of the feederID; this test passes the
// bare UUID because that is what the dataset stores.
func TestGridAPPSD_QueryInverter_IEEE123pv(t *testing.T) {
	c := newGridAPPSDClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := c.QueryInverter(ctx, ieee123pvFeederMRID)
	if err != nil {
		t.Fatalf("QueryInverter: %v", err)
	}
	if got := len(res.Results.Bindings); got < 14 {
		t.Fatalf("len(Bindings) = %d, want at least 14 (IEEE 123pv has 14 PECs)", got)
	}
	// Spot-check the first row carries the fields downstream code
	// expects. On the current schema ?id and ?pecid are equal because
	// the OPTIONAL Unit block did not bind.
	row := res.Results.Bindings[0]
	for _, key := range []string{"name", "bus", "ratedS", "ratedU", "p", "q", "id", "pecid", "fdrid"} {
		if row[key].Value == "" {
			t.Errorf("first row missing %q; full row: %+v", key, row)
		}
	}
	if row["fdrid"].Value != ieee123pvFeederMRID {
		t.Errorf("first row fdrid = %q, want %q", row["fdrid"].Value, ieee123pvFeederMRID)
	}
}

// TestGridAPPSD_QueryInverter_UnderscorePrefixedFeederID asserts the
// wrapper accepts the underscore-prefixed feederID form (the form
// Python upstream call sites and our bridge wiring frequently pass)
// and matches the same dataset as the bare-UUID form.
func TestGridAPPSD_QueryInverter_UnderscorePrefixedFeederID(t *testing.T) {
	c := newGridAPPSDClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := c.QueryInverter(ctx, "_"+ieee123pvFeederMRID)
	if err != nil {
		t.Fatalf("QueryInverter (underscore-prefixed): %v", err)
	}
	if got := len(res.Results.Bindings); got < 14 {
		t.Fatalf("len(Bindings) = %d, want at least 14", got)
	}
}

// TestGridAPPSD_QuerySolar_IEEE123pv exercises QuerySolar against the
// live feeder. On the current schema (no Units), the OPTIONAL
// PhotovoltaicUnit block does not bind and the query returns the same
// 14 PECs QueryInverter does. On an older-schema dataset only the PV
// PECs would come back; the schema-variant compatibility is documented
// at the top of queries.go.
func TestGridAPPSD_QuerySolar_IEEE123pv(t *testing.T) {
	c := newGridAPPSDClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := c.QuerySolar(ctx, ieee123pvFeederMRID)
	if err != nil {
		t.Fatalf("QuerySolar: %v", err)
	}
	if got := len(res.Results.Bindings); got == 0 {
		t.Fatalf("len(Bindings) = 0, want non-zero (gridappsd-docker:develop has 14 PECs in IEEE 123pv)")
	}
}

// TestGridAPPSD_QueryBattery_IEEE123pv exercises QueryBattery. Same
// schema-variant note as QuerySolar: on the current schema this
// returns the full PEC set, on an older-schema dataset only battery
// PECs.
func TestGridAPPSD_QueryBattery_IEEE123pv(t *testing.T) {
	c := newGridAPPSDClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := c.QueryBattery(ctx, ieee123pvFeederMRID)
	if err != nil {
		t.Fatalf("QueryBattery: %v", err)
	}
	// Battery may legitimately return 0 rows on an older-schema dataset
	// that has only PV. On gridappsd-docker:develop's current schema
	// the OPTIONAL block falls through and we expect non-zero.
	if got := len(res.Results.Bindings); got == 0 {
		t.Fatalf("len(Bindings) = 0, want non-zero on gridappsd-docker:develop (PEC-as-leaf shape)")
	}
}

// TestGridAPPSD_QueryMeasurements_IEEE123pv asserts the GAGO-029
// measurement-mRID enumeration template returns rows binding each
// Measurement to its parent ConductingEquipment (PowerSystemResource).
// IEEE 123pv has 14 PECs and dozens of measurements per PEC, so the
// row count is bounded below by the PEC count and is expected in the
// hundreds. The first row must carry both ?measid and ?eqid as
// non-empty strings, which the side-table populator depends on.
func TestGridAPPSD_QueryMeasurements_IEEE123pv(t *testing.T) {
	c := newGridAPPSDClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := c.QueryMeasurements(ctx, ieee123pvFeederMRID)
	if err != nil {
		t.Fatalf("QueryMeasurements: %v", err)
	}
	if got := len(res.Results.Bindings); got < 14 {
		t.Fatalf("len(Bindings) = %d, want at least 14 (one Measurement per PEC at minimum)", got)
	}
	row := res.Results.Bindings[0]
	for _, key := range []string{"measid", "eqid"} {
		if row[key].Value == "" {
			t.Errorf("first row missing %q; full row: %+v", key, row)
		}
	}
}
