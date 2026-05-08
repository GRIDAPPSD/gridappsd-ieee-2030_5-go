// Command bridge is the GridAPPS-D side (Stage 1) of the IEEE 2030.5
// to GridAPPS-D bridge. It connects to the GridAPPS-D message bus,
// queries the CIM feeder for inverter / solar / battery DERs, populates
// an in-memory mRID-to-LFDI registry, and (when a SimulationID is
// configured) subscribes to the simulation output topic and logs each
// MeasurementFrame.
//
// This binary intentionally does NOT speak IEEE 2030.5. The 2030.5
// server side is a Stage 2 follow-up filed separately; it requires the
// `ieee-2030_5-go/internal/server` package to expose a public Server
// constructor, plus the real LFDI mapping derived from device certs.
// Stage 1 ships the GridAPPS-D plumbing so the connect plus CIM query
// plus subscribe path can be validated end-to-end against a live
// gridappsd-docker stack.
//
// Lifecycle: the process runs until SIGINT or SIGTERM. Cancellation
// flows through a single context.Context root: the cimstomp.Client and
// any sim.Pump goroutines exit on ctx cancel; the bridge then closes
// the publisher (if any) and the client and returns.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/GRIDAPPSD/gridappsd-2030_5-go/internal/cim"
	"github.com/GRIDAPPSD/gridappsd-2030_5-go/internal/cim/sim"
	"github.com/GRIDAPPSD/gridappsd-2030_5-go/internal/cimstomp"
	"github.com/GRIDAPPSD/gridappsd-2030_5-go/internal/measurements"
	"github.com/GRIDAPPSD/gridappsd-2030_5-go/internal/registry"
)

const version = "0.1.0-stage1"

// connectTimeout bounds the initial STOMP dial plus auth-token
// bootstrap. The platform's broker normally responds in well under a
// second; 15s leaves comfortable headroom for slow CI machines.
const connectTimeout = 15 * time.Second

// queryTimeout bounds a single CIM SPARQL request. The 123-bus feeder
// query returns in ~100 ms locally; 30 s tolerates a busy platform.
const queryTimeout = 30 * time.Second

// statsLogInterval is how often the pump handler logs cumulative
// side-table hit/miss counters. 30 s balances signal (an operator can
// observe coverage drift over a multi-minute simulation) against log
// noise. The interval is process-local and not configurable in v0.
// It is a var rather than a const so unit tests can shrink it for a
// fast assertion without touching production behavior.
var statsLogInterval = 30 * time.Second

func main() {
	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		// flag.ErrHelp surfaces from -h / -help and is not a usage
		// error; flag.NewFlagSet has already printed the usage banner.
		// Exit 0 so shell redirection of `bridge -h` for docs works.
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		log.Fatalf("bridge: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Printf("bridge %s starting", version)
	if err := run(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("bridge: %v", err)
	}
	log.Printf("bridge: shutdown complete")
}

// run is the bridge lifecycle. It is split out from main so tests can
// drive a synthetic ctx and config without exec-ing a subprocess.
//
// Steady-state errors (handler errors, mid-run subscription drops) are
// logged and the loop continues. Startup errors (connect, CIM query,
// registry populate) are fatal: the bridge has nothing useful to do
// without them.
func run(ctx context.Context, cfg config) error {
	client, err := connectClient(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := client.Close(); cerr != nil {
			log.Printf("bridge: client close: %v", cerr)
		}
	}()

	cimClient := cim.NewClient(client)

	reg, err := bootstrapRegistry(ctx, cimClient, cfg.FeederMRID)
	if err != nil {
		return err
	}

	measTable, err := bootstrapMeasurementTable(ctx, cimClient, cfg.FeederMRID)
	if err != nil {
		return err
	}

	if cfg.PublishOnStart {
		// The publish smoke test wants to send a DifferenceBuilder
		// envelope to /topic/goss.gridappsd.simulation.input.<sim_id>.
		// At Stage 1, cimstomp.Publisher.Publish marshals a
		// sample-array body and cimstomp.Client has no public Send
		// primitive; either path is the wrong shape for a diff
		// envelope. Adding a SendRaw method is a deliberate widening
		// of the cimstomp surface and belongs in its own ticket. The
		// flag is wired so callers can opt in once the primitive
		// lands; for now we log and proceed.
		log.Printf("bridge: -publish-on-start requested; cimstomp Publish/SendRaw primitive for diff envelopes is filed as Stage 2 follow-up; skipping")
	}

	if cfg.SimulationID == "" {
		log.Printf("bridge: no SEP2_SIMULATION_ID set; skipping simulation subscribe; idling until shutdown")
		<-ctx.Done()
		return ctx.Err()
	}

	return runPump(ctx, client, reg, measTable, cfg.SimulationID)
}

// connectClient dials the GridAPPS-D STOMP broker, runs the auth-token
// bootstrap, and returns a connected Client. The connect uses its own
// timeout so a stuck platform fails fast rather than hanging on the
// caller's parent ctx.
func connectClient(ctx context.Context, cfg config) (*cimstomp.Client, error) {
	log.Printf("bridge: connecting to %s as %s", cfg.STOMPAddr, cfg.STOMPUser)

	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	client := cimstomp.NewClient(cimstomp.STOMPConfig{
		Address:  cfg.STOMPAddr,
		User:     cfg.STOMPUser,
		Password: cfg.STOMPPassword,
	})
	if err := client.Connect(cctx); err != nil {
		return nil, fmt.Errorf("connect %s: %w", cfg.STOMPAddr, err)
	}
	log.Printf("bridge: connected; auth token bootstrapped")
	return client, nil
}

// bootstrapRegistry runs the three CIM enumeration queries against the
// feeder, dedupes by mRID (a single device may surface in multiple
// queries when the upstream filter is open), and populates a fresh
// registry with a placeholder LFDI per device. Returns the populated
// registry; the caller does not need a separate add step.
func bootstrapRegistry(ctx context.Context, c *cim.Client, feederMRID string) (*registry.Registry, error) {
	log.Printf("bridge: querying CIM feeder %s", feederMRID)

	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	inverters, err := queryDevices(qctx, "inverter", c.QueryInverter, feederMRID)
	if err != nil {
		return nil, err
	}
	solar, err := queryDevices(qctx, "solar", c.QuerySolar, feederMRID)
	if err != nil {
		return nil, err
	}
	battery, err := queryDevices(qctx, "battery", c.QueryBattery, feederMRID)
	if err != nil {
		return nil, err
	}

	log.Printf("bridge: queried CIM feeder %s; %d inverters / %d solar / %d battery",
		feederMRID, len(inverters), len(solar), len(battery))

	// Dedupe across the three lists. A PhotovoltaicUnit shows up under
	// QueryInverter (open filter) and QuerySolar; the registry must
	// only carry one entry per mRID.
	seen := make(map[string]struct{})
	var entries []registry.Entry
	for _, src := range [][]device{inverters, solar, battery} {
		for _, d := range src {
			if d.MRID == "" {
				continue
			}
			if _, dup := seen[d.MRID]; dup {
				continue
			}
			seen[d.MRID] = struct{}{}
			lfdi := placeholderLFDI(d.MRID)
			entries = append(entries, registry.Entry{
				MRID:        d.MRID,
				Name:        d.Name,
				LFDI:        lfdi,
				Placeholder: true,
			})
			// One line per device at populate time so an operator can
			// grep "(placeholder)" to confirm Stage 2 LFDI work has not
			// happened yet. Logged before the summary so the order is
			// "per-device, then total".
			log.Printf("bridge: device mrid=%s lfdi=%s (placeholder)", d.MRID, lfdi)
		}
	}

	reg := registry.New()
	if err := reg.AddBatch(entries); err != nil {
		return nil, fmt.Errorf("registry populate: %w", err)
	}
	log.Printf("bridge: registry populated: %d entries (LFDI placeholder; real cert mapping is Stage 2)",
		reg.Len())
	return reg, nil
}

// device is the slim projection of a SPARQL binding row this bridge
// needs at Stage 1: identity plus name. Richer attributes (ratedS,
// ratedU, phases) stay in the raw QueryDataResult and can be lifted
// into typed structs when downstream code consumes them.
type device struct {
	MRID string
	Name string
}

// queryDevices runs one of the cim.Client Query* wrappers, projects
// each binding row down to a device, and skips rows whose ?id binding
// is missing or empty. The kind argument is only used for log
// readability. The query argument is the bound method on *cim.Client;
// passing it as a value lets the three call sites share this projection
// without a type switch.
func queryDevices(
	ctx context.Context,
	kind string,
	query func(context.Context, string) (*cim.QueryDataResult, error),
	feederMRID string,
) ([]device, error) {
	res, err := query(ctx, feederMRID)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", kind, err)
	}
	if res == nil {
		return nil, nil
	}
	out := make([]device, 0, len(res.Results.Bindings))
	for _, row := range res.Results.Bindings {
		mrid := row["id"].Value
		if mrid == "" {
			continue
		}
		out = append(out, device{
			MRID: mrid,
			Name: row["name"].Value,
		})
	}
	return out, nil
}

// bootstrapMeasurementTable runs the measurement-mRID enumeration query
// against the feeder and populates a side table mapping each measurement
// to its parent ConductingEquipment-mRID. The bridge's pump handler
// consults the side table before reaching the registry, so a frame's
// measurement-mRID can be attributed to a device entry rather than
// surfacing as an unrecognized lookup.
//
// On the IEEE 123pv feeder this query returns a few hundred rows; the
// 30 s queryTimeout shared with the DER enumeration queries is plenty.
// An empty result set is not an error: bridge logs a warning and
// proceeds with an empty side table; every frame then surfaces as a
// side-table miss, which the operator can grep for and act on.
func bootstrapMeasurementTable(ctx context.Context, c *cim.Client, feederMRID string) (*measurements.Table, error) {
	log.Printf("bridge: querying CIM measurements for feeder %s", feederMRID)

	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	res, err := c.QueryMeasurements(qctx, feederMRID)
	if err != nil {
		return nil, fmt.Errorf("query measurements: %w", err)
	}

	tbl := measurements.New()
	if res == nil || len(res.Results.Bindings) == 0 {
		log.Printf("bridge: WARN: measurement side table populated: 0 mappings (empty query result; every frame will surface as side-table miss)")
		return tbl, nil
	}

	maps := make([]measurements.Mapping, 0, len(res.Results.Bindings))
	for _, row := range res.Results.Bindings {
		measMRID := row["measid"].Value
		eqMRID := row["eqid"].Value
		if measMRID == "" || eqMRID == "" {
			continue
		}
		maps = append(maps, measurements.Mapping{
			MeasurementMRID: measMRID,
			DeviceMRID:      eqMRID,
		})
	}
	if err := tbl.AddBatch(maps); err != nil {
		return nil, fmt.Errorf("measurement table populate: %w", err)
	}
	log.Printf("bridge: measurement side table populated: %d mappings", tbl.Len())
	return tbl, nil
}

// runPump subscribes to the simulation output topic and runs the Pump
// until ctx is cancelled or the subscription closes. The handler logs a
// one-liner per frame, walks each measurement, and resolves the
// measurement-mRID through the side table to a device-mRID, then
// through the registry to an LFDI. The first time a given measurement
// mRID is seen, the handler logs its resolution outcome; subsequent
// frames carrying the same mRID are deduplicated to avoid log spam.
//
// A periodic stats line logs the side table's cumulative hit and miss
// counts so an operator can verify coverage at runtime without parsing
// the per-mRID log. The stats goroutine exits when ctx is cancelled,
// before pump.Run returns, so the function returns with no live
// goroutines.
func runPump(
	ctx context.Context,
	client *cimstomp.Client,
	reg *registry.Registry,
	tbl *measurements.Table,
	simID string,
) error {
	dest := sim.OutputTopic(simID)
	log.Printf("bridge: subscribing to %s", dest)

	pump := sim.NewPump(client, simID)

	// seen dedupes the per-mRID lookup log so a 1Hz simulation does not
	// reprint the same line every timestep. Plain map plus mutex; the
	// pump handler is invoked serially so the mutex is cheap insurance
	// against a future parallel-handler change rather than current need.
	var (
		seenMu sync.Mutex
		seen   = make(map[string]struct{})
	)

	// Periodic stats logger. Runs in its own goroutine that exits on
	// stats-ctx cancel; the done channel is the join point that ensures
	// runPump does not return with a live goroutine. tbl is a
	// precondition: bootstrapMeasurementTable always returns a non-nil
	// Table even on an empty result set, so no nil guard is needed here.
	//
	// The stats goroutine uses a derived context so that we can cancel it
	// independently of the parent ctx. pump.Run can return without parent
	// ctx being cancelled (subscription closed cleanly, or broker-side
	// error), and in those cases <-statsDone would otherwise block
	// forever. cancelStats below unblocks the join unconditionally.
	statsCtx, cancelStats := context.WithCancel(ctx)
	defer cancelStats()
	statsDone := make(chan struct{})
	go logStatsUntilDone(statsCtx, tbl, statsDone)

	err := pump.Run(ctx, func(f sim.MeasurementFrame) error {
		log.Printf("bridge: frame received simulation_id=%s timestamp=%d measurements=%d",
			f.SimulationID, f.Message.Timestamp, len(f.Message.Measurements))
		for mrid := range f.Message.Measurements {
			seenMu.Lock()
			_, dup := seen[mrid]
			if !dup {
				seen[mrid] = struct{}{}
			}
			seenMu.Unlock()
			if dup {
				continue
			}
			logResolution(mrid, resolveMeasurement(reg, tbl, mrid))
		}
		return nil
	})
	cancelStats()
	<-statsDone
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("pump: %w", err)
	}
	return nil
}

// logStatsUntilDone emits a periodic stats line for the measurement
// side table at statsLogInterval. The function returns and signals done
// when ctx is cancelled. tbl must be non-nil; the caller establishes
// that invariant via bootstrapMeasurementTable.
func logStatsUntilDone(ctx context.Context, tbl *measurements.Table, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(statsLogInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			hits, misses := tbl.Stats()
			log.Printf("bridge: measurement-resolver stats hits=%d misses=%d table_size=%d",
				hits, misses, tbl.Len())
		}
	}
}

// logResolution prints a one-line summary of one (measurement-mRID,
// resolution) pair using formatResolution to render the body.
func logResolution(measMRID string, r ResolveResult) {
	log.Printf("bridge: %s", formatResolution(measMRID, r))
}

// formatResolution renders a one-line summary of one (measurement-mRID,
// resolution) pair. The format keeps the same key=value shape across
// the three status branches so log parsers can switch on status alone.
// device and lfdi are rendered as <unknown>/<unregistered> placeholders
// when the corresponding field is empty for that status.
func formatResolution(measMRID string, r ResolveResult) string {
	device := r.DeviceMRID
	if device == "" {
		device = "<unknown>"
	}
	lfdi := r.LFDI
	if lfdi == "" {
		if r.Status == ResolveStatusUnregisteredDevice {
			lfdi = "<unregistered>"
		} else {
			lfdi = "<unknown>"
		}
	}
	return fmt.Sprintf("frame for meas=%s device=%s lfdi=%s status=%s",
		measMRID, device, lfdi, r.Status)
}
