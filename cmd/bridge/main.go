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

	return runPump(ctx, client, reg, cfg.SimulationID)
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
			entries = append(entries, registry.Entry{
				MRID: d.MRID,
				Name: d.Name,
				LFDI: placeholderLFDI(d.MRID),
			})
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

// runPump subscribes to the simulation output topic and runs the Pump
// until ctx is cancelled or the subscription closes. The handler logs a
// one-liner per frame and then walks each measurement, performing an
// mRID-to-LFDI lookup against reg. The first time a given measurement
// mRID is seen, the handler logs the lookup result; subsequent frames
// carrying the same mRID are deduplicated to avoid log spam.
//
// At Stage 1, measurement mRIDs (per-point identifiers in the platform
// frame) are not the same as the device mRIDs the registry is keyed on,
// so the lookup typically misses. The point of plumbing reg into the
// handler is to demonstrate the seam: a future revision can change the
// lookup key (e.g., to the parent ConductingEquipment mRID) without
// reworking the pump glue. Richer downstream consumption (IEEE 2030.5
// MirrorMeterReading mapping) is Stage 2.
func runPump(ctx context.Context, client *cimstomp.Client, reg *registry.Registry, simID string) error {
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
			if lfdi, ok := reg.LFDI(mrid); ok {
				log.Printf("bridge: frame for mrid=%s lfdi=%s (placeholder)", mrid, lfdi)
			} else {
				log.Printf("bridge: frame for mrid=%s lfdi=<not registered>", mrid)
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("pump: %w", err)
	}
	return nil
}
