// Package telemetrypub publishes the embedded IEEE 2030.5 server's
// stored DERStatus resources to the GridAPPS-D message bus, on its own
// timer, independently of the protocol request path.
//
// The layering this package exists to enforce: receiving a
// 2030.5 request must not cause a platform-side bus publish. Per-device,
// per-request handling is correct behaviour for the 2030.5 server, and
// that is exactly where it stays: the server's responsibility ends at
// storing the resource. The store is the seam. This package reads that
// store on an interval, aggregates every device into one platform-shaped
// message, and sends it.
//
// That is the arrangement the Python upstream already uses
// (ieee_2030_5/adapters/gridappsd_adapter.py): get_message_for_bus reads
// the adapter's own 2030.5 store, aggregates across inverters into a
// single dict keyed by inverter mRID, and publish_house_aggregates sends
// it on a PublishTimer running at publish_interval_seconds (15 in the
// upstream config). This package reproduces that shape in Go, with one
// deliberate improvement: unchanged devices are omitted rather than
// republished every interval (see DirtyTracker, and the
// Config.PublishUnchanged switch that turns the improvement off).
//
// Three seams are deliberate and named, because follow-up work will use
// them: the message shape (MessageBuilder), the destination
// (Config.Destination), and the change-marking source
// (ChangeTracker.Mark, reachable as Publisher.Mark).
package telemetrypub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// DefaultInterval is the publish period when Config.Interval is zero. It
// matches the Python upstream's publish_interval_seconds (config.yml:84).
const DefaultInterval = 15 * time.Second

// BusPublisher is the minimal surface this package needs from a
// connected message bus: fire-and-forget publish to a destination.
// Defined at the consumer, per the workspace Go standards, rather than
// depending on gridappsd-go's fieldbus.MessageBus directly;
// *fieldbus.GridAPPSDMessageBus (and the fieldbus.MessageBus interface
// itself) satisfies this method set unchanged.
type BusPublisher interface {
	Send(ctx context.Context, destination, contentType string, body []byte) error
}

// StatusSource is the read side: a point-in-time snapshot of every
// device that has stored a DERStatus. *sep2embed.Embed satisfies it.
// Defined at the consumer so this package depends on a one-method read
// surface rather than on the embed's whole API.
type StatusSource interface {
	DERStatusSnapshots(ctx context.Context) ([]sep2embed.DERStatusSnapshot, error)
}

// Config configures a Publisher.
type Config struct {
	// Source is the 2030.5 store to read. Required.
	Source StatusSource

	// Bus is the connected GridAPPS-D message bus. Required.
	Bus BusPublisher

	// Destination is the bus destination every aggregate is published
	// to. Required.
	//
	// THIS IS THE DESTINATION SEAM. The bridge passes the synthetic
	// simulation-input topic (internal/cim/sim.InputTopic) today, which
	// is what the removed per-PUT relay published to. The agreed
	// eventual target is an application output topic, deferred pending
	// confirmation of who subscribes. Nothing in this package derives or
	// inspects the destination, so retargeting is a one-line change at
	// the call site in cmd/bridge.
	Destination string

	// Build renders one interval's devices into the aggregate message.
	// Required; pass DiffMessageBuilder(simulationID) for the shape this
	// bridge publishes today. See MessageBuilder.
	Build MessageBuilder

	// Fingerprint defines what "unchanged" means for suppression. Nil
	// uses DiffFingerprint, which is paired with DiffMessageBuilder.
	// Ignored when PublishUnchanged is true.
	Fingerprint Fingerprinter

	// Interval is the publish period. Zero uses DefaultInterval;
	// negative is an error.
	Interval time.Duration

	// PublishUnchanged is THE suppression switch. False (the default)
	// omits devices whose published content has not changed since the
	// last successful publish. True restores full-snapshot semantics:
	// every device with a stored DERStatus, every interval. See
	// AlwaysDirty for when that is the right answer.
	PublishUnchanged bool

	// Now is the clock stamped into each message. Nil uses time.Now.
	Now func() time.Time

	// Observe, when non-nil, receives each built message just before
	// the bus send. It runs whether or not the send succeeds, so a
	// consumer that must see what this publisher reports (the admin
	// graph history) does not depend on the bus being up. It must not
	// block and must not modify Message.Body.
	Observe func(Message)
}

// Publisher reads DERStatus resources from a StatusSource on a timer and
// publishes one aggregate message per interval. Construct with New and
// start with Run.
type Publisher struct {
	source   StatusSource
	bus      BusPublisher
	dest     string
	build    MessageBuilder
	tracker  ChangeTracker
	interval time.Duration
	now      func() time.Time
	observe  func(Message)
}

// New validates cfg and returns a Publisher. It performs no I/O and
// starts no goroutine; Run does both.
func New(cfg Config) (*Publisher, error) {
	if cfg.Source == nil {
		return nil, errors.New("telemetrypub: Config.Source is required")
	}
	if cfg.Bus == nil {
		return nil, errors.New("telemetrypub: Config.Bus is required")
	}
	if cfg.Destination == "" {
		return nil, errors.New("telemetrypub: Config.Destination is required")
	}
	if cfg.Build == nil {
		return nil, errors.New("telemetrypub: Config.Build is required")
	}
	if cfg.Interval < 0 {
		return nil, fmt.Errorf("telemetrypub: Config.Interval %s must not be negative", cfg.Interval)
	}

	interval := cfg.Interval
	if interval == 0 {
		interval = DefaultInterval
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	var tracker ChangeTracker = NewDirtyTracker(cfg.Fingerprint)
	if cfg.PublishUnchanged {
		tracker = AlwaysDirty{}
	}

	return &Publisher{
		source:   cfg.Source,
		bus:      cfg.Bus,
		dest:     cfg.Destination,
		build:    cfg.Build,
		tracker:  tracker,
		interval: interval,
		now:      now,
		observe:  cfg.Observe,
	}, nil
}

// Mark records that the named device may have new content to publish.
//
// It is the marking seam described on ChangeTracker: when core grows a
// resource-arrival observer, wiring it to this method makes the dirty
// set exact rather than inferred, with no other change to this package.
// Safe to call concurrently with the running publish loop, including
// from a 2030.5 request goroutine.
func (p *Publisher) Mark(mrid string) {
	p.tracker.Mark(mrid)
}

// Run publishes on p's interval until ctx is cancelled, then returns
// ctx.Err(). It owns no other goroutine, so a caller that observes Run
// return knows nothing of this publisher is still running.
//
// A failed cycle (store read error, message build error, bus send
// failure) is logged and the loop continues: the protocol side must keep
// serving devices while the platform side is broken, which is the whole
// point of moving this out of the request path. Nothing here can make a
// DERStatus PUT fail, block, or slow down: this loop never touches the
// request path, and it only ever reads from the store.
func (p *Publisher) Run(ctx context.Context) error {
	log.Printf("telemetrypub: publishing DERStatus aggregates to %s every %s (unchanged devices suppressed: %t)",
		p.dest, p.interval, !p.publishesUnchanged())

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := p.publishOnce(ctx); err != nil {
				log.Printf("telemetrypub: publish cycle failed: %v", err)
			}
		}
	}
}

// publishesUnchanged reports whether suppression is off, for logging.
func (p *Publisher) publishesUnchanged() bool {
	_, alwaysDirty := p.tracker.(AlwaysDirty)
	return alwaysDirty
}

// publishOnce runs exactly one publish cycle: read the store, select the
// devices worth publishing, build one aggregate message, send it, and
// only then clear those devices' pending marks.
//
// An interval with nothing to say publishes NOTHING (never an empty
// envelope) and says so in the log. A silent no-op is the failure mode
// that cost end-to-end runs 8, 9 and 10 a cycle each: a PUT succeeded, a
// relay ran, and the absence of a bus frame was indistinguishable from
// success at every layer.
func (p *Publisher) publishOnce(ctx context.Context) error {
	devices, err := p.source.DERStatusSnapshots(ctx)
	if err != nil {
		return fmt.Errorf("telemetrypub: read der statuses: %w", err)
	}

	batch := p.tracker.Select(devices)
	if len(batch.Devices) == 0 {
		log.Printf("telemetrypub: no device DERStatus changed since the last publish (%d with a stored status); nothing published to %s",
			len(devices), p.dest)
		return nil
	}

	msg, err := p.build(batch.Devices, p.now())
	if err != nil {
		if errors.Is(err, ErrNoContent) {
			// The selected devices carry no mapped field at all, so
			// there is genuinely nothing to send. Clear them: their
			// (empty) content has been accounted for, and a later
			// DERStatus carrying real fields changes the fingerprint and
			// re-selects them. Not clearing would re-log this every
			// interval forever.
			log.Printf("telemetrypub: %d device(s) changed but carried no mapped field; nothing published to %s",
				len(batch.Devices), p.dest)
			p.tracker.Published(batch)
			return nil
		}
		return fmt.Errorf("telemetrypub: build message: %w", err)
	}

	// Before the send: recording only after a successful send would
	// bring back the bus dependency. A failed send re-selects the batch
	// and records it again next cycle with a later timestamp, which is
	// acceptable.
	if p.observe != nil {
		p.observe(msg)
	}

	if err := p.bus.Send(ctx, p.dest, msg.ContentType, msg.Body); err != nil {
		// Deliberately NOT clearing the batch: the update is still
		// pending and must be republished next interval. Clearing here
		// would turn a transient bus failure into permanent, silent data
		// loss.
		return fmt.Errorf("telemetrypub: send to %s: %w", p.dest, err)
	}

	p.tracker.Published(batch)
	log.Printf("telemetrypub: published %d device DERStatus update(s) to %s", len(batch.Devices), p.dest)
	return nil
}
