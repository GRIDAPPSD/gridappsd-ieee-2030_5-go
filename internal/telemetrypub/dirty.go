package telemetrypub

import (
	"log"
	"sync"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// Batch is one interval's publish candidates: the devices selected for
// publication, plus the bookkeeping a ChangeTracker needs to clear
// exactly those devices afterwards. Build it only via
// ChangeTracker.Select, and hand it back to ChangeTracker.Published ONLY
// after the send actually succeeded.
type Batch struct {
	// Devices are the snapshots to publish, in the order Select
	// received them.
	Devices []sep2embed.DERStatusSnapshot

	// gens records the dirty-mark generation each device was selected
	// at, so a mark that lands after selection is not cleared by this
	// batch's Published call. fps records the content actually sent for
	// each device, so Published records what went out rather than
	// whatever the tracker last happened to see.
	gens map[string]uint64
	fps  map[string]string
}

// ChangeTracker decides which of an interval's devices to publish, and
// records the outcome.
//
// THIS IS THE MARKING SEAM. Today Select infers arrivals by comparing
// each device's publishable content against what was last successfully
// published for it. The agreed target is an arrival signal from the core
// 2030.5 server: a resource-arrival observer that calls Mark on the
// request goroutine as each DERStatus lands. Wiring that in is a
// substitution, not a rewrite: point the observer at
// Publisher.Mark (which forwards here) and the inference in
// DirtyTracker.Select becomes redundant rather than load bearing.
//
// Mark must be safe to call concurrently with Select and Published,
// because that observer will be synchronous on the 2030.5 request
// goroutine (the same non-blocking, no-goroutine contract
// internal/controlobs and core's subscription manager observer already
// use).
type ChangeTracker interface {
	// Mark records that the named device may have new content to
	// publish. Safe for concurrent use.
	Mark(mrid string)

	// Select returns the subset of devices to publish this interval.
	Select(devices []sep2embed.DERStatusSnapshot) Batch

	// Published clears the marking for the devices in b. Call it ONLY
	// after the batch was successfully sent.
	Published(b Batch)
}

// DirtyTracker is the default ChangeTracker: a dirty set keyed by device
// mRID, plus a record of what was last successfully published for each
// device.
//
// The dirty set is the gate. Select publishes exactly the marked devices
// and nothing else, which is what makes the eventual arrival-observer
// wiring a substitution rather than a rewrite: whoever marks decides
// what gets published.
//
// The `last` map is not a second gate, it is the interim MARKER. With no
// arrival signal available yet, Select infers arrivals by comparing each
// device's publishable content against what was last successfully
// published for it, and marks on a difference. That inference is the one
// piece the core-side observer replaces; everything below it is already
// in its final shape.
//
// CLEARING SEMANTICS, deliberately chosen: a device's mark is cleared
// ONLY by Published, and only when no newer mark has landed since Select
// observed it (the generation guard). Clearing at Select time, or
// clearing unconditionally, would lose an update permanently whenever a
// publish failed on the bus or whenever an arrival landed mid-publish:
// the next interval would see a clean flag and send nothing, and the
// change would be gone with no error anywhere.
type DirtyTracker struct {
	fingerprint Fingerprinter

	mu sync.Mutex
	// gen maps mRID to the generation of its pending mark. An absent
	// key means clean.
	gen map[string]uint64
	// last maps mRID to the fingerprint of the content last
	// successfully published for that device.
	last map[string]string
}

// NewDirtyTracker returns a DirtyTracker using fp to decide what
// "changed" means. A nil fp uses DiffFingerprint, the fingerprint paired
// with this package's default message shape.
func NewDirtyTracker(fp Fingerprinter) *DirtyTracker {
	if fp == nil {
		fp = DiffFingerprint
	}
	return &DirtyTracker{
		fingerprint: fp,
		gen:         make(map[string]uint64),
		last:        make(map[string]string),
	}
}

// Mark records a possible change for mrid. Safe for concurrent use: it
// is the entry point the future core-side arrival observer calls from
// the 2030.5 request goroutine.
func (d *DirtyTracker) Mark(mrid string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gen[mrid]++
}

// Select marks any device whose publishable content has moved since its
// last successful publish (the interim arrival marker), then returns
// every device that is marked. Nothing is cleared here: see
// DirtyTracker's doc comment for why clearing belongs to Published
// alone.
func (d *DirtyTracker) Select(devices []sep2embed.DERStatusSnapshot) Batch {
	// Fingerprints are computed before the lock is taken: fingerprinting
	// is pure and can be comparatively expensive, and holding the mutex
	// across it would block the arrival observer's Mark on the protocol
	// request goroutine, which must never wait on this publisher.
	fps := make(map[string]string, len(devices))
	unfingerprintable := make(map[string]bool, len(devices))
	for _, device := range devices {
		fp, err := d.fingerprint(device)
		if err != nil {
			// Cannot tell whether it changed: mark it and publish. For
			// telemetry the safe direction is a redundant publish, never
			// a dropped reading, and the build step downstream will
			// surface the same underlying problem loudly.
			log.Printf("telemetrypub: fingerprint mrid=%s failed (%v); treating the device as changed", device.MRID, err)
			unfingerprintable[device.MRID] = true
			continue
		}
		fps[device.MRID] = fp
	}

	batch := Batch{
		gens: make(map[string]uint64, len(devices)),
		fps:  make(map[string]string, len(devices)),
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	for _, device := range devices {
		fp := fps[device.MRID]

		// The interim arrival marker: with no arrival signal available
		// yet, a content change IS the arrival. When the core-side
		// observer lands it marks on arrival instead, and these four
		// lines are all that has to go.
		if last, published := d.last[device.MRID]; unfingerprintable[device.MRID] || !published || last != fp {
			d.gen[device.MRID]++
		}

		gen, dirty := d.gen[device.MRID]
		if !dirty {
			continue
		}

		batch.Devices = append(batch.Devices, device)
		batch.gens[device.MRID] = gen
		batch.fps[device.MRID] = fp
	}

	return batch
}

// Published clears the marking for every device in b whose mark has not
// been superseded since Select observed it, and records the content that
// actually went out. Call it only after a successful send.
func (d *DirtyTracker) Published(b Batch) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, device := range b.Devices {
		if fp, ok := b.fps[device.MRID]; ok {
			// What was published, not what the tracker last saw: a
			// device that changed again mid-publish keeps its mark (the
			// generation guard below) and is republished next interval.
			d.last[device.MRID] = fp
		}
		if d.gen[device.MRID] == b.gens[device.MRID] {
			delete(d.gen, device.MRID)
		}
	}
}

// AlwaysDirty is the ChangeTracker behind the suppression switch's OFF
// position: every device, every interval, nothing suppressed and nothing
// remembered.
//
// It exists because the consumer semantics of this bridge's telemetry
// topic are not settled. If a subscriber treats each message as a FULL
// state snapshot rather than a set of incremental updates, suppressing
// unchanged devices would silently age out every device that has not
// moved. Flipping Config.PublishUnchanged to true selects this tracker
// and restores full-snapshot semantics.
type AlwaysDirty struct{}

// Mark is a no-op: everything is published anyway.
func (AlwaysDirty) Mark(string) {}

// Select returns every device it is given.
func (AlwaysDirty) Select(devices []sep2embed.DERStatusSnapshot) Batch {
	return Batch{Devices: devices}
}

// Published is a no-op: nothing is remembered between intervals.
func (AlwaysDirty) Published(Batch) {}
