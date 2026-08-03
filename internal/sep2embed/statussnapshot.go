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
// pre-GAGO-121 arrangement, where a DERStatus PUT itself triggered a bus
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

			// HasParent before Get: ScopedStore.Get routes through
			// ForParent, which CREATES an empty per-parent store as a side
			// effect of a read. This method runs on a timer against every
			// seeded device, so taking that path would have every quiet
			// device's store allocated by the reader rather than by its
			// first PUT.
			if !e.stores.DERStatuses.HasParent(scope) {
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
