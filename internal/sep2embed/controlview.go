package sep2embed

import (
	"context"
	"fmt"

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

// ResponsesFor returns the Responses devices have posted whose subject is the
// given event mRID, in store order. It only reads: the one writer of this
// store is the protocol listener's POST /rsps/{id}/rsp.
func (e *Embed) ResponsesFor(ctx context.Context, subject string) ([]ResponseSnapshot, error) {
	if subject == "" {
		return nil, nil
	}
	result, err := e.stores.Responses.List(ctx, coreresponse.DefaultSetID, store.ListOptions{Limit: snapshotListLimit})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("sep2embed: snapshot responses: %w", err)
	}
	var out []ResponseSnapshot
	for _, r := range result.Items {
		if r.Subject != subject {
			continue
		}
		snap := ResponseSnapshot{Subject: r.Subject, EndDeviceLFDI: r.EndDeviceLFDI, CreatedDateTime: r.CreatedDateTime}
		if r.Status != nil {
			snap.Status = *r.Status
		}
		out = append(out, snap)
	}
	return out, nil
}
