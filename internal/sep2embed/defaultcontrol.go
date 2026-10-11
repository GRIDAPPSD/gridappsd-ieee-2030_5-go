package sep2embed

import (
	"context"
	"errors"
	"fmt"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/store"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// DefaultTargetSet reports what SetDefaultTargetW did.
type DefaultTargetSet struct {
	// Cancelled holds the store ids of the page controls the set cancelled.
	Cancelled []string
}

// ErrDefaultCancelIncomplete is returned by SetDefaultTargetW when the new
// default is served but a page control it meant to cancel is still in force.
var ErrDefaultCancelIncomplete = errors.New("sep2embed: default set, but a page control was not cancelled")

// SetDefaultTargetW sets opModTargetW on the device's DefaultDERControl to
// watts (multiplier 0), keeping every other member, and cancels the device's
// active and scheduled opModTargetW controls that pageIssued names, so the
// device falls back to the new default instead of holding a timed control.
// pageIssued may be nil, which cancels nothing.
//
// A control the bus path issued is never cancelled, whatever pageIssued
// says: the platform owns it. The value is validated as a control delta's is.
// The default is written before anything is cancelled, so a device that falls
// back early finds the new value; a cancel failure after that is returned
// wrapped in ErrDefaultCancelIncomplete with the ids cancelled so far.
func (e *Embed) SetDefaultTargetW(ctx context.Context, reg *registry.Registry, deviceMRID string, watts int64, pageIssued func(controlID string) bool) (DefaultTargetSet, error) {
	_, base, err := resolveControlDelta(reg, ControlDelta{
		Object:    deviceMRID,
		Attribute: derControlAttributePrefix + "opModTargetW",
		Value:     map[string]any{"multiplier": 0.0, "value": float64(watts)},
	})
	if err != nil {
		return DefaultTargetSet{}, fmt.Errorf("sep2embed: default set: %w", err)
	}

	e.controlMu.Lock()
	defer e.controlMu.Unlock()
	edevID, err := e.writeDefaultTargetW(ctx, reg, deviceMRID, base.OpModTargetW)
	if err != nil {
		return DefaultTargetSet{}, fmt.Errorf("sep2embed: default set: %w", err)
	}
	if e.notifier != nil {
		defer e.notifier.Notify(ctx, derProgramListHref(edevID, controlFSAID), sep2.NotificationStatusDefault)
	}

	cancelled, err := e.cancelPageTargetW(ctx, edevID, pageIssued)
	if err != nil {
		return DefaultTargetSet{Cancelled: cancelled}, fmt.Errorf("%w: %w", ErrDefaultCancelIncomplete, err)
	}
	return DefaultTargetSet{Cancelled: cancelled}, nil
}

// ClearDefaultTargetW restores the device's DefaultDERControl opModTargetW
// to the fleet seed's value, absent when the seed has none. It cancels
// nothing.
func (e *Embed) ClearDefaultTargetW(ctx context.Context, reg *registry.Registry, deviceMRID string) error {
	var seed *sep2.ActivePower
	if b := e.policy.DefaultControl.DERControlBase; b != nil && b.OpModTargetW != nil {
		v := *b.OpModTargetW
		seed = &v
	}
	e.controlMu.Lock()
	defer e.controlMu.Unlock()
	edevID, err := e.writeDefaultTargetW(ctx, reg, deviceMRID, seed)
	if err != nil {
		return fmt.Errorf("sep2embed: default clear: %w", err)
	}
	if e.notifier != nil {
		e.notifier.Notify(ctx, derProgramListHref(edevID, controlFSAID), sep2.NotificationStatusDefault)
	}
	return nil
}

// DefaultTargetW returns the opModTargetW the device's DefaultDERControl
// serves, or nil when it serves none.
func (e *Embed) DefaultTargetW(ctx context.Context, deviceMRID string) (*sep2.ActivePower, error) {
	edevID, ok := e.stores.EndDeviceIndexes.IndexFor(deviceMRID)
	if !ok {
		return nil, fmt.Errorf("%w: mrid=%q has no seeded URL index", ErrUnknownControlDevice, deviceMRID)
	}
	dderc, err := e.stores.DefaultDERControls.Get(ctx, derControlScope(edevID, controlFSAID, controlDERProgramID), singletonKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("sep2embed: default read: %w", err)
	}
	if dderc.DERControlBase == nil || dderc.DERControlBase.OpModTargetW == nil {
		return nil, nil
	}
	v := *dderc.DERControlBase.OpModTargetW
	return &v, nil
}

// writeDefaultTargetW replaces opModTargetW on the device's DefaultDERControl
// with target and returns the device's URL index. The caller holds controlMu.
func (e *Embed) writeDefaultTargetW(ctx context.Context, reg *registry.Registry, deviceMRID string, target *sep2.ActivePower) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	entry, ok := reg.Get(deviceMRID)
	if !ok {
		return "", fmt.Errorf("%w: mrid=%q", ErrUnknownControlDevice, deviceMRID)
	}
	// IndexFor, not Allocate, and the EndDevice check, for the reasons
	// prepareControl gives: an unseeded device is refused, never minted.
	edevID, ok := e.stores.EndDeviceIndexes.IndexFor(entry.MRID)
	if !ok {
		return "", fmt.Errorf("%w: mrid=%q has no seeded URL index", ErrUnknownControlDevice, entry.MRID)
	}
	if _, err := e.stores.EndDevices.Get(ctx, edevID); err != nil {
		return "", fmt.Errorf("%w: edev %q (mrid=%q) not seeded: %v", ErrUnknownControlDevice, edevID, entry.MRID, err)
	}
	if err := ensureDERProgram(ctx, e.stores, edevID, entry.LFDI, controlFSAID, controlDERProgramID, e.policy); err != nil {
		return "", fmt.Errorf("ensure der program: %w", err)
	}

	scope := derControlScope(edevID, controlFSAID, controlDERProgramID)
	dderc, err := e.stores.DefaultDERControls.Get(ctx, scope, singletonKey)
	if err != nil {
		return "", fmt.Errorf("read default der control: %w", err)
	}
	next := dderc.Copy()
	if next.DERControlBase == nil {
		next.DERControlBase = &sep2.DERControlBase{}
	}
	next.DERControlBase.OpModTargetW = target
	// A seed with no DERControlBase is restored as none, so a clear serves
	// the seed's bytes rather than an empty element.
	if e.policy.DefaultControl.DERControlBase == nil && *next.DERControlBase == (sep2.DERControlBase{}) {
		next.DERControlBase = nil
	}
	if err := e.stores.DefaultDERControls.Update(ctx, scope, singletonKey, next); err != nil {
		return "", fmt.Errorf("write default der control: %w", err)
	}
	return edevID, nil
}

// cancelPageTargetW marks Cancelled every opModTargetW control of the device
// that is active or scheduled, has not ended, was not issued by the bus, and
// pageIssued names. It returns the store ids it cancelled. The caller holds
// controlMu.
func (e *Embed) cancelPageTargetW(ctx context.Context, edevID string, pageIssued func(string) bool) ([]string, error) {
	if pageIssued == nil {
		return nil, nil
	}
	scope := derControlScope(edevID, controlFSAID, controlDERProgramID)
	list, err := e.stores.DERControls.List(ctx, scope, store.ListOptions{Unbounded: true})
	if err != nil {
		return nil, fmt.Errorf("list controls to cancel: %w", err)
	}
	nowUnix := e.policy.Control.now().UTC().Unix()
	var cancelled []string
	for _, c := range list.Items {
		id := derControlID(c.CreationTime, c.MRID)
		if !inForceOrPending(c, nowUnix) || c.DERControlBase == nil || c.DERControlBase.OpModTargetW == nil ||
			e.busIssued.has(id) || !pageIssued(id) {
			continue
		}
		// A status edit, which 2018 rule c) p.90 permits on a served event.
		c.EventStatus.CurrentStatus = sep2.EventStatusCancelled
		c.EventStatus.DateTime = nowUnix
		if err := e.stores.DERControls.Update(ctx, scope, id, c); err != nil {
			return cancelled, fmt.Errorf("cancel control %s: %w", id, err)
		}
		cancelled = append(cancelled, id)
	}
	return cancelled, nil
}

// inForceOrPending reports whether c is active or scheduled and has not
// ended at nowUnix.
func inForceOrPending(c sep2.DERControl, nowUnix int64) bool {
	if c.EventStatus == nil || c.Interval == nil {
		return false
	}
	switch c.EventStatus.CurrentStatus {
	case sep2.EventStatusActive, sep2.EventStatusScheduled:
	default:
		return false
	}
	return c.Interval.Start+int64(c.Interval.Duration) > nowUnix
}
