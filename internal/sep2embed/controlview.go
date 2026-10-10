package sep2embed

import (
	"context"
	"fmt"
	"math"
	"strconv"

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
	const minSince, maxSince = 1_000_000_000, math.MaxInt64 / 1_000_000_000
	if since < minSince || since > maxSince {
		return ""
	}
	return "rsp-" + strconv.FormatInt(since*1_000_000_000-1, 10)
}
