//go:build gridappsd

// Live read-only check for issue #115: the House query and the
// battery-leg validation logic, against a real GridAPPS-D platform and
// the Southern co-simulation feeder. Skips if the platform is
// unreachable, matching the rest of this package's gridappsd-tagged
// tests. STOMP address/credentials and the feeder mRID are read from
// the environment (SEP2_STOMP_ADDR, SEP2_STOMP_USER, SEP2_STOMP_PASSWORD,
// SEP2_FEEDER_MRID), the same variable names the bridge itself reads, so
// sourcing an operator's .env file before running this test targets the
// same deployment the bridge would.
//
// Run with:
//
//	go test -tags=gridappsd -run TestGridAPPSD_EnergyConsumers_SouthernFeeder -v ./internal/cim/
package cim

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp"
)

// southernFeederMRID is the co-simulation feeder issue #115 targets
// (final_9_20_2024_reduced_southern), used as the default when
// SEP2_FEEDER_MRID is not set in the environment.
const southernFeederMRID = "510950FB-0686-4956-8A2A-636C049FAB3F"

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// newLiveEnergyConsumersClient connects to the broker named by the
// environment (falling back to the gridappsd-docker dev defaults) and
// registers a Cleanup to close it. Deliberately does not print any of
// the resolved address, user, or password: the caller sources these
// from an operator's credential file, and this check must not echo
// them.
func newLiveEnergyConsumersClient(t *testing.T) *Client {
	t.Helper()
	addr := envOrDefault("SEP2_STOMP_ADDR", gridappsdAddr)
	user := envOrDefault("SEP2_STOMP_USER", gridappsdUser)
	password := envOrDefault("SEP2_STOMP_PASSWORD", gridappsdPassword)

	cs := cimstomp.NewClient(cimstomp.STOMPConfig{Address: addr, User: user, Password: password})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cs.Connect(ctx); err != nil {
		t.Skipf("GridAPPS-D platform not reachable: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return NewClient(cs)
}

// TestGridAPPSD_EnergyConsumers_SouthernFeeder runs QueryEnergyConsumers
// against the real Southern feeder (read-only: no bridge server
// started, no port bound) and reports the house/non-house counts.
// It then exercises the same battery-leg validation cmd/bridge's
// resolveBatteryLegs performs, using mRIDs discovered from THIS
// query's own results (never a fresh name-pattern SPARQL query): a
// utility_bat-named row as the valid case, a house row as the
// also-a-house-load refusal case, and a mRID absent from the result set
// as the not-on-the-feeder refusal case. Filtering the already-fetched
// rows by name here is a test-only diagnostic to locate a real leg
// mRID to validate against; it is not a second production query and
// does not reintroduce name-pattern discovery into the bridge.
func TestGridAPPSD_EnergyConsumers_SouthernFeeder(t *testing.T) {
	c := newLiveEnergyConsumersClient(t)
	feederMRID := envOrDefault("SEP2_FEEDER_MRID", southernFeederMRID)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := c.QueryEnergyConsumers(ctx, feederMRID)
	if err != nil {
		t.Fatalf("QueryEnergyConsumers: %v", err)
	}

	var houseCount, nonHouseCount int
	var oneLegMRID, oneHouseMRID string
	seen := map[string]bool{}
	for _, row := range res.Results.Bindings {
		mrid := row["ecid"].Value
		if mrid == "" || seen[mrid] {
			continue
		}
		seen[mrid] = true
		isHouse := row["house"].Value != ""
		if isHouse {
			houseCount++
			if oneHouseMRID == "" {
				oneHouseMRID = mrid
			}
			continue
		}
		nonHouseCount++
		if oneLegMRID == "" && strings.HasPrefix(row["ecname"].Value, "utility_bat") {
			oneLegMRID = mrid
		}
	}
	t.Logf("feeder %s: %d distinct EnergyConsumer(s), %d house-flagged, %d non-house",
		feederMRID, len(seen), houseCount, nonHouseCount)

	if houseCount == 0 {
		t.Fatalf("houseCount = 0, want > 0 on the Southern feeder (40 tl_house_* loads expected)")
	}
	if oneLegMRID == "" {
		t.Fatalf("no utility_bat*-named EnergyConsumer found among %d non-house rows; cannot exercise resolveBatteryLegs' valid-leg path", nonHouseCount)
	}

	feederECs := make(map[string]ecRowForTest, len(seen))
	for _, row := range res.Results.Bindings {
		mrid := row["ecid"].Value
		if mrid == "" {
			continue
		}
		feederECs[mrid] = ecRowForTest{Name: row["ecname"].Value, IsHouse: row["house"].Value != ""}
	}

	t.Run("valid_leg_resolves", func(t *testing.T) {
		r, ok := feederECs[oneLegMRID]
		if !ok || r.IsHouse {
			t.Fatalf("live leg mRID %q: ok=%v isHouse=%v, want ok=true isHouse=false", oneLegMRID, ok, r.IsHouse)
		}
		t.Logf("valid leg: mRID=%s name=%s", oneLegMRID, r.Name)
	})

	t.Run("house_mrid_is_rejected_as_a_leg", func(t *testing.T) {
		r, ok := feederECs[oneHouseMRID]
		if !ok || !r.IsHouse {
			t.Fatalf("live house mRID %q: ok=%v isHouse=%v, want ok=true isHouse=true", oneHouseMRID, ok, r.IsHouse)
		}
	})

	t.Run("mrid_absent_from_feeder_is_rejected", func(t *testing.T) {
		const ghost = "00000000-0000-0000-0000-000000000000"
		if _, ok := feederECs[ghost]; ok {
			t.Fatalf("sentinel mRID %q unexpectedly present on the live feeder", ghost)
		}
	})
}

// ecRowForTest mirrors cmd/bridge's ecRow shape. Duplicated rather than
// imported: cmd/bridge is package main and internal/cim must not import
// it (that would invert the bridge's dependency direction on its own
// query package).
type ecRowForTest struct {
	Name    string
	IsHouse bool
}
