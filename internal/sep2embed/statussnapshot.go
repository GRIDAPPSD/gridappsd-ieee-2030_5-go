package sep2embed

import (
	"context"
	"fmt"
	"log"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// DERStatusSnapshot is a read only copy of one device's currently stored
// DERStatus, together with the identity and addressing needed to publish
// it: the CIM mRID that owns it, and the (EDevID, DERID) scope it was
// read from.
//
// MRID is the identity half and is load bearing. It is resolved through
// the same EndDeviceIndexes allocator seeding used to assign the device
// its URL index, so it is the CIM device key, never the LFDI and never
// the URL segment. A consumer keys its published values off this field;
// pairing a status with the wrong mRID would publish one device's
// telemetry under another device's identity, which is why
// DERStatusSnapshots skips a device it cannot resolve rather than
// guessing (see its doc comment).
//
// Status is a value copy taken from core's store under that store's own
// read lock (memory.Store.Get returns resource.Copy()), so a caller can
// read it freely while devices continue to PUT.
type DERStatusSnapshot struct {
	MRID   string
	EDevID string
	DERID  string
	Status sep2.DERStatus
}

// DERStatusSnapshots returns the currently stored DERStatus of every
// seeded device that has PUT one, in EndDevice store order.
//
// This is the read seam the GridAPPS-D telemetry publisher
// (internal/telemetrypub) polls on its own timer. It replaces the
// prior arrangement, where a DERStatus PUT itself triggered a bus
// send from inside the protocol request path: the 2030.5 server's
// responsibility now ends at storing the resource, and the store is the
// seam between the protocol layer and the platform layer, exactly as the
// Python upstream arranges it (ieee_2030_5/adapters/gridappsd_adapter.py's
// get_message_for_bus reads its own 2030.5 store on a PublishTimer).
//
// A device that has never PUT a DERStatus is OMITTED, not reported with
// a zero-valued Status: core's singleton handler serves a spec-valid
// empty default on GET without writing anything, so "no entry in the
// store" genuinely means "this device has never reported", and
// synthesizing an all-unset reading for it would put a fabricated
// observation on the bus.
//
// A device whose URL index has no reverse mRID mapping is skipped with a
// log line rather than published under a guessed identity: fail closed.
// That condition means the index allocator and the seeded fleet have
// drifted, which is a bug worth seeing, not a value worth inventing.
func (e *Embed) DERStatusSnapshots(ctx context.Context) ([]DERStatusSnapshot, error) {
	devices, err := e.EndDevices(ctx)
	if err != nil {
		return nil, fmt.Errorf("sep2embed: der status snapshots: %w", err)
	}

	snaps := make([]DERStatusSnapshot, 0, len(devices))
	for _, dev := range devices {
		mrid, ok := e.stores.EndDeviceIndexes.DeviceKey(dev.ID)
		if !ok {
			log.Printf("sep2embed: der status snapshots: no registered mRID for edev=%s; skipping", dev.ID)
			continue
		}

		for _, der := range dev.DERs {
			scope := dev.ID + "/" + der.ID

			// HasParent before Get: as of core v0.14.1, ScopedStore.Get
			// routes through the unexported parentStore lookup (core's
			// memory/scoped.go:125), which looks a parent up WITHOUT
			// creating one; Get on an unknown parent now reports
			// ErrNotFound rather than materializing a bucket. The HasParent
			// call here is kept anyway: it lets this method skip a device
			// that has never PUT a DERStatus without paying for a Get call
			// whose only outcome would be a discarded ErrNotFound, and doing
			// so on a fallible query (see below) rather than a local nil
			// comparison keeps a backend fault visible instead of silently
			// read as "quiet device".
			//
			// The check is fallible by contract (store.ScopedReader:
			// "on a durable backend this is a query, not a cheap local map
			// lookup"), so an error is surfaced rather than absorbed into
			// the skip branch. Absorbing it would report a device that the
			// store could not be asked about as an ordinary quiet device,
			// which is the same wire result as "no reading" and would hide
			// a backend fault behind a plausible-looking empty snapshot.
			has, err := e.stores.DERStatuses.HasParent(ctx, scope)
			if err != nil {
				return nil, fmt.Errorf("sep2embed: der status snapshots: has parent %q: %w", scope, err)
			}
			if !has {
				continue
			}

			status, err := e.stores.DERStatuses.Get(ctx, scope, singletonKey)
			if err != nil {
				if isNotFound(err) {
					continue
				}
				return nil, fmt.Errorf("sep2embed: der status snapshots: read %q: %w", scope, err)
			}

			snaps = append(snaps, DERStatusSnapshot{
				MRID:   mrid,
				EDevID: dev.ID,
				DERID:  der.ID,
				Status: status,
			})
		}
	}

	return snaps, nil
}
