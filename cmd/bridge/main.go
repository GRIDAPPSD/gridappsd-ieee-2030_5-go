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
// The GridAPPS-D connection rides github.com/GRIDAPPSD/gridappsd-go's
// fieldbus.MessageBus (GAGO-039), adapted to this bridge's own
// internal/cim.Requester and internal/cim/sim.SubscribeClient
// interfaces via internal/gridappsdclient. internal/cimstomp, this
// repo's own STOMP implementation, stays in the tree for its
// Publisher and DifferenceBuilder types but no longer backs the
// bridge's connection.
//
// Lifecycle: the process runs until SIGINT or SIGTERM. Cancellation
// flows through a single context.Context root: the
// fieldbus.MessageBus and any sim.Pump goroutines exit on ctx cancel;
// the bridge then disconnects the bus and returns.
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

	"github.com/GRIDAPPSD/gridappsd-go/fieldbus"
	"github.com/GRIDAPPSD/gridappsd-go/gridappsd"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/sim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/gridappsdclient"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
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
	bus, err := connectClient(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := bus.Disconnect(); cerr != nil {
			log.Printf("bridge: bus disconnect: %v", cerr)
		}
	}()

	cimClient := cim.NewClient(gridappsdclient.NewRequester(bus))

	reg, err := bootstrapRegistry(ctx, cimClient, cfg.FeederMRID)
	if err != nil {
		return err
	}

	if cfg.PublishOnStart {
		// The publish smoke test wants to send a DifferenceBuilder
		// envelope to /topic/goss.gridappsd.simulation.input.<sim_id>.
		// fieldbus.MessageBus.Send can carry an arbitrary body, but
		// building the DifferenceBuilder envelope itself and wiring
		// it through here is a deliberate Stage 2 follow-up, not part
		// of this connection swap. The flag is wired so callers can
		// opt in once that follow-up lands; for now we log and
		// proceed.
		log.Printf("bridge: -publish-on-start requested; DifferenceBuilder envelope publish is filed as Stage 2 follow-up; skipping")
	}

	if cfg.SimulationID == "" {
		log.Printf("bridge: no SEP2_SIMULATION_ID set; skipping simulation subscribe; idling until shutdown")
		<-ctx.Done()
		return ctx.Err()
	}

	// ctx is the pump's root, the same signal-derived ctx that governed
	// connect and the CIM query above. gridappsd-go's router does not
	// yet surface a broker-teardown signal to fieldbus.MessageBus
	// callers (upstream gap GAG-009; see the relay doc comment in
	// internal/gridappsdclient/subscriber.go), so a mid-run broker
	// disconnect does NOT independently wake the pump: only this ctx's
	// cancellation (SIGINT/SIGTERM) does.
	return runPump(ctx, bus, reg, cfg.SimulationID)
}

// busConfig projects the bridge's config onto gridappsd-go's connection
// config. Split out from connectClient so the mapping, in particular
// the plaintext opt-in, can be asserted by a unit test without dialing
// a broker.
func busConfig(cfg config) gridappsd.Config {
	return gridappsd.Config{
		Address:        cfg.STOMPAddr,
		User:           cfg.STOMPUser,
		Password:       cfg.STOMPPassword,
		AllowPlaintext: cfg.AllowPlaintext,
	}
}

// connectClient dials the GridAPPS-D broker, runs the two-step GOSS
// token bootstrap, and returns a connected fieldbus.MessageBus. The
// connect uses its own timeout so a stuck platform fails fast rather
// than hanging on the caller's parent ctx.
//
// Transport selection is entirely cfg.AllowPlaintext's call: the zero
// value dials TLS against the system trust store (gridappsd.Config's
// fail-closed default), matching this bridge's own config default.
// cfg.AllowPlaintext must be set explicitly to reach a plaintext
// broker such as gridappsd-docker's dev stack.
func connectClient(ctx context.Context, cfg config) (fieldbus.MessageBus, error) {
	log.Printf("bridge: connecting to %s as %s", cfg.STOMPAddr, cfg.STOMPUser)

	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	bus := fieldbus.New(busConfig(cfg))
	if err := bus.Connect(cctx); err != nil {
		return nil, fmt.Errorf("connect %s: %w", cfg.STOMPAddr, err)
	}
	log.Printf("bridge: connected; auth token bootstrapped")
	return bus, nil
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
func runPump(ctx context.Context, bus fieldbus.MessageBus, reg *registry.Registry, simID string) error {
	dest := sim.OutputTopic(simID)
	log.Printf("bridge: subscribing to %s", dest)

	pump := sim.NewPump(gridappsdclient.NewSubscriber(bus), simID)

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
