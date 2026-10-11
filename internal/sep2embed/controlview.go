package sep2embed

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"

	coreresponse "github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/handlers/response"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/store"
)

// ResponseSnapshot is a read only copy of one Response a device posted about
// an event. Status is the ResponseStatus code (1 received, 2 started, 3
// completed, and so on); a Response that carried none reads 0.
type ResponseSnapshot struct {
	Subject         string
	EndDeviceLFDI   string
	Status          uint8
	CreatedDateTime int64
}

// ControlSnapshot returns the served DERControl with the given store id
// (ControlSend.ControlID) for the device with the given CIM mRID. The bool
// is false when the device is unknown or the control is no longer served,
// which includes a control the lifecycle sweep has removed after its end.
func (e *Embed) ControlSnapshot(ctx context.Context, deviceMRID, controlID string) (DERControlSnapshot, bool, error) {
	edevID, ok := e.stores.EndDeviceIndexes.IndexFor(deviceMRID)
	if !ok {
		return DERControlSnapshot{}, false, nil
	}
	snaps, err := e.DERControls(ctx, edevID, controlFSAID, controlDERProgramID)
	if err != nil {
		return DERControlSnapshot{}, false, err
	}
	for _, s := range snaps {
		if s.ID == controlID {
			return s, true, nil
		}
	}
	return DERControlSnapshot{}, false, nil
}

// ControlsFor returns every DERControl still served for the device with the
// given CIM mRID, superseded and cancelled ones included. A device with no
// seeded URL index is ErrUnknownControlDevice, as it is on the send path:
// callers pass registered devices, so a missing index is drift, not "none".
func (e *Embed) ControlsFor(ctx context.Context, deviceMRID string) ([]DERControlSnapshot, error) {
	edevID, ok := e.stores.EndDeviceIndexes.IndexFor(deviceMRID)
	if !ok {
		return nil, fmt.Errorf("%w: mrid=%q has no seeded URL index", ErrUnknownControlDevice, deviceMRID)
	}
	return e.DERControls(ctx, edevID, controlFSAID, controlDERProgramID)
}

// ResponsesSince is the Unix second to pass ResponsesFor so that every
// Response to the control is read: creationTime runs at most
// maxCreationTimeLeadSeconds ahead of the issue instant, and no device can
// respond before the issue. A control's start is not such a bound, because
// a scheduled control is fetched, and Received, before it starts.
func (s DERControlSnapshot) ResponsesSince() int64 {
	return s.CreationTime - maxCreationTimeLeadSeconds
}

// maxBusIssued bounds the bus-issued control ids an Embed remembers; past
// it the oldest is dropped, and BusIssued answers false for it.
const maxBusIssued = 8192

// issuedLedger remembers the store ids of the last maxBusIssued controls
// the bus path issued. The zero value is ready.
type issuedLedger struct {
	mu    sync.Mutex
	ids   map[string]struct{}
	order []string
}

func (l *issuedLedger) add(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ids == nil {
		l.ids = map[string]struct{}{}
	}
	if _, ok := l.ids[id]; ok {
		return
	}
	l.ids[id] = struct{}{}
	l.order = append(l.order, id)
	if len(l.order) > maxBusIssued {
		delete(l.ids, l.order[0])
		l.order = l.order[1:]
	}
}

func (l *issuedLedger) has(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.ids[id]
	return ok
}

// BusIssued reports whether the bus path (ApplyControlDeltaOutcome) issued
// the control with the given store id since this Embed started, within the
// last maxBusIssued it issued. False is not proof of another issuer: the
// record is in memory and bounded.
func (e *Embed) BusIssued(controlID string) bool {
	return e.busIssued.has(controlID)
}

// ResponsesFor returns the Responses devices have posted since the Unix
// second since whose subject is the given event mRID, in store order. It only
// reads: the one writer of this store is the protocol listener's POST
// /rsps/{id}/rsp. A since of 0 reads every Response.
func (e *Embed) ResponsesFor(ctx context.Context, subject string, since int64) ([]ResponseSnapshot, error) {
	if subject == "" {
		return nil, nil
	}
	opts := store.ListOptions{Limit: snapshotListLimit, After: responseKeyBefore(since)}
	var out []ResponseSnapshot
	for {
		page, err := e.stores.Responses.List(ctx, coreresponse.DefaultSetID, opts)
		if err != nil {
			if isNotFound(err) {
				return out, nil
			}
			return nil, fmt.Errorf("sep2embed: snapshot responses: %w", err)
		}
		for _, r := range page.Items {
			if r.Subject != subject {
				continue
			}
			snap := ResponseSnapshot{Subject: r.Subject, EndDeviceLFDI: r.EndDeviceLFDI, CreatedDateTime: r.CreatedDateTime}
			if r.Status != nil {
				snap.Status = *r.Status
			}
			out = append(out, snap)
		}
		if page.Results < opts.Limit {
			return out, nil
		}
		opts.Start += page.Results
	}
}

// responseKeyBefore returns the store key just below every Response received
// at or after the Unix second since. The protocol listener keys a Response
// rsp-<UnixNano at receipt>, and the store lists keys in byte order, which is
// time order while the nanosecond count has 19 digits (2001 to 2286). Outside
// that range it returns "", which reads from the first key.
func responseKeyBefore(since int64) string {
	const minSince, maxSince = 1_000_000_001, math.MaxInt64 / 1_000_000_000
	if since < minSince || since > maxSince {
		return ""
	}
	return "rsp-" + strconv.FormatInt(since*1_000_000_000-1, 10)
}
