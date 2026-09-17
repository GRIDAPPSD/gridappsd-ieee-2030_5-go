package sep2embed

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/store"
)

// snapshotListLimit bounds every List call this file makes. Core's
// memory.Store.List treats Limit == 0 as "return nil Items", not "return
// everything": see memory.go's early return when opts.Limit == 0. This
// bridge's device fleet is small (tens of devices, one DER and at most
// one DERProgram and one DERControl per device), so a limit well above
// any realistic fleet size is a safe stand in for "no limit" without
// reaching into core to add a real unbounded option.
const snapshotListLimit = 10000

// EndDeviceSnapshot is a plain, read only copy of one seeded EndDevice's
// operator-relevant fields plus its child DERs. It carries no mutable
// store handle: every field is a value, safe to hand to a caller (for
// example an admin UI JSON handler) with no risk of that caller mutating
// this Embed's live state.
type EndDeviceSnapshot struct {
	ID      string
	LFDI    string
	SFDI    string
	Href    string
	Enabled bool
	DERs    []DERSnapshot
}

// DERSnapshot is a plain, read only copy of one child DER resource. DER
// itself (see pkg/sep2.DER) carries only link fields in this bridge today
// (seed.go's seedOne sets nothing but Href): Href is the only value worth
// surfacing at this layer.
type DERSnapshot struct {
	ID   string
	Href string
}

// DERProgramSnapshot is a plain, read only copy of one DERProgram,
// scoped to its owning EndDevice.
type DERProgramSnapshot struct {
	ID          string
	Href        string
	MRID        string
	Description string
	Primacy     uint8

	// DefaultDERControlLink is the href of this program's
	// DefaultDERControl singleton, or empty if the program
	// somehow has none (should not happen post-ensureDERProgram: every
	// program this bridge creates seeds its default control in the same
	// call). A caller resolves it by GET against the embedded server, or
	// via this package's own DefaultDERControl accessor.
	DefaultDERControlLink string
}

// DERControlBaseSnapshot is a plain, read only copy of the operating-mode
// fields ApplyControlDelta actually writes (opModTargetW, opModTargetVar,
// opModConnect, opModEnergize; see control.go's applyDERControlBaseField).
// The other DERControlBase fields exist on the wire type but this bridge
// never maps a delta onto them, so they are omitted here rather than
// carried as an always-empty pointer soup: a caller that needs the full
// wire shape reads it over the protocol listener itself.
type DERControlBaseSnapshot struct {
	OpModTargetW   *sep2.ActivePower
	OpModTargetVar *sep2.ReactivePower
	OpModConnect   *bool
	OpModEnergize  *bool
}

// DefaultDERControlSnapshot is a plain, read only copy of one
// DefaultDERControl singleton, scoped to its owning EndDevice, FSA, and
// DERProgram.
type DefaultDERControlSnapshot struct {
	Href string
	MRID string
	Base *DERControlBaseSnapshot
}

// DERControlSnapshot is a plain, read only copy of one DERControl,
// scoped to its owning EndDevice, FSA, and DERProgram. EventStatus is
// carried by value (CurrentStatus/DateTime) since that is what a caller
// reading control-flow state cares about; the full sep2.EventStatus is
// not exposed to avoid leaking a pointer into the mutable store item it
// was copied from.
type DERControlSnapshot struct {
	ID            string
	Href          string
	MRID          string
	CurrentStatus uint8
	DateTime      int64
	Base          *DERControlBaseSnapshot
}

// edevIDFromHref recovers the EndDevice store id (the opaque URL index) from
// a served EndDevice href of the canonical shape "/edev/{id}".
//
// It exists because store List returns values without their keys, and the
// key is no longer derivable from any field on the
// value: the LFDI is identity, not addressing.
//
// It is deliberately strict. A malformed or absent href is an error rather
// than a best-effort guess, because every caller uses the result as a store
// scope key, and a wrong key is a silent empty result rather than a visible
// failure.
func edevIDFromHref(href string) (string, error) {
	const prefix = "/edev/"
	id, ok := strings.CutPrefix(href, prefix)
	if !ok {
		return "", fmt.Errorf("href %q does not have the canonical %q prefix", href, prefix)
	}
	if id == "" {
		return "", fmt.Errorf("href %q has an empty id segment", href)
	}
	if strings.Contains(id, "/") {
		return "", fmt.Errorf("href %q has more than one path segment after %q", href, prefix)
	}
	return id, nil
}

// EndDevices returns a read only snapshot of every seeded EndDevice and
// its child DERs. The returned slice and every value it contains are
// copies: no field is a shared pointer into this Embed's live stores, so
// a caller cannot mutate this Embed's state through the result.
//
// snapshotListLimit bounds the underlying List calls; see its doc
// comment for why a zero Limit is unsafe here.
func (e *Embed) EndDevices(ctx context.Context) ([]EndDeviceSnapshot, error) {
	result, err := e.stores.EndDevices.List(ctx, store.ListOptions{Limit: snapshotListLimit})
	if err != nil {
		return nil, fmt.Errorf("sep2embed: snapshot end devices: %w", err)
	}

	snaps := make([]EndDeviceSnapshot, 0, len(result.Items))
	for _, dev := range result.Items {
		// The store key is the opaque URL index, NOT the LFDI
		// List returns values without their keys, so
		// recover the key from the device's own canonical href, which seeding
		// stamped as "/edev/" + id. Using dev.LFDI here would silently scope
		// every child lookup to a key that no longer exists and report every
		// device as having zero DERs.
		id, err := edevIDFromHref(dev.Href)
		if err != nil {
			return nil, fmt.Errorf("sep2embed: snapshot end device (lfdi %q): %w", dev.LFDI, err)
		}

		ders, err := e.dersFor(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("sep2embed: snapshot end device %q: %w", id, err)
		}

		enabled := false
		if dev.Enabled != nil {
			enabled = *dev.Enabled
		}

		snaps = append(snaps, EndDeviceSnapshot{
			ID:      id,
			LFDI:    dev.LFDI,
			SFDI:    dev.SFDI,
			Href:    dev.Href,
			Enabled: enabled,
			DERs:    ders,
		})
	}

	return snaps, nil
}

// dersFor returns the read only DER snapshots scoped to one EndDevice id.
func (e *Embed) dersFor(ctx context.Context, edevID string) ([]DERSnapshot, error) {
	result, err := e.stores.DERs.List(ctx, edevID, store.ListOptions{Limit: snapshotListLimit})
	if err != nil {
		return nil, fmt.Errorf("list ders: %w", err)
	}

	snaps := make([]DERSnapshot, 0, len(result.Items))
	for i, der := range result.Items {
		snaps = append(snaps, DERSnapshot{
			ID:   derIDFor(result, i),
			Href: der.Href,
		})
	}
	return snaps, nil
}

// derIDFor is a placeholder identity fallback: store.ListResult does not
// carry each item's key alongside its value (see store.ListResult's
// fields: All, Results, Items), so the DER's own id is not recoverable
// from a list result alone. seed.go always writes id "1" for a device's
// sole DER; a future multi DER bridge would need core to carry keys
// through List to do better than this.
func derIDFor(_ store.ListResult[sep2.DER], _ int) string {
	return "1"
}

// DERPrograms returns a read only snapshot of every DERProgram scoped to
// edevID.
func (e *Embed) DERPrograms(ctx context.Context, edevID string) ([]DERProgramSnapshot, error) {
	result, err := e.stores.DERPrograms.List(ctx, edevID, store.ListOptions{Limit: snapshotListLimit})
	if err != nil {
		return nil, fmt.Errorf("sep2embed: snapshot der programs for %q: %w", edevID, err)
	}

	snaps := make([]DERProgramSnapshot, 0, len(result.Items))
	for _, p := range result.Items {
		var dderc string
		if p.DefaultDERControlLink != nil {
			dderc = p.DefaultDERControlLink.Href
		}
		snaps = append(snaps, DERProgramSnapshot{
			ID:                    controlDERProgramID,
			Href:                  p.Href,
			MRID:                  p.MRID,
			Description:           p.Description,
			Primacy:               p.Primacy,
			DefaultDERControlLink: dderc,
		})
	}
	return snaps, nil
}

// DefaultDERControl returns a read only snapshot of the DefaultDERControl
// singleton scoped to (edevID, fsaID, derpID), or nil if none has been
// written yet (a missing singleton is not an error: see core's
// HandleSingletonGetPut, which serves a default rather than 404 on GET).
func (e *Embed) DefaultDERControl(ctx context.Context, edevID, fsaID, derpID string) (*DefaultDERControlSnapshot, error) {
	scope := derControlScope(edevID, fsaID, derpID)
	dderc, err := e.stores.DefaultDERControls.Get(ctx, scope, singletonKey)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("sep2embed: snapshot default der control %q: %w", scope, err)
	}

	return &DefaultDERControlSnapshot{
		Href: dderc.Href,
		MRID: dderc.MRID,
		Base: derControlBaseSnapshotOf(dderc.DERControlBase),
	}, nil
}

// DERControls returns a read only snapshot of every DERControl scoped to
// (edevID, fsaID, derpID).
//
// The list is genuinely a list: each control delta issues its
// own DERControl rather than rewriting one, so a device carries every control
// issued within its Effective Scheduled Period, superseded ones included.
// Superseded entries are visible as CurrentStatus 4 under the 2018 semantics
// this server presents (see supersede.go).
func (e *Embed) DERControls(ctx context.Context, edevID, fsaID, derpID string) ([]DERControlSnapshot, error) {
	scope := derControlScope(edevID, fsaID, derpID)
	result, err := e.stores.DERControls.List(ctx, scope, store.ListOptions{Limit: snapshotListLimit})
	if err != nil {
		return nil, fmt.Errorf("sep2embed: snapshot der controls %q: %w", scope, err)
	}

	snaps := make([]DERControlSnapshot, 0, len(result.Items))
	for _, c := range result.Items {
		var status uint8
		var dateTime int64
		if c.EventStatus != nil {
			status = c.EventStatus.CurrentStatus
			dateTime = c.EventStatus.DateTime
		}

		snaps = append(snaps, DERControlSnapshot{
			// Recomputed from the record, for the reason given on
			// supersedePriorControls: derControlID produced the key, and
			// store.ListResult carries items without their keys.
			ID:            derControlID(c.CreationTime, c.MRID),
			Href:          c.Href,
			MRID:          c.MRID,
			CurrentStatus: status,
			DateTime:      dateTime,
			Base:          derControlBaseSnapshotOf(c.DERControlBase),
		})
	}
	return snaps, nil
}

// derControlBaseSnapshotOf copies only the fields this bridge's control
// path actually writes; see DERControlBaseSnapshot's doc comment. A nil
// base returns nil, not a zero valued snapshot, so a caller can tell
// "no DERControlBase was ever written" from "one was written with every
// field left at its zero value".
func derControlBaseSnapshotOf(base *sep2.DERControlBase) *DERControlBaseSnapshot {
	if base == nil {
		return nil
	}

	snap := &DERControlBaseSnapshot{}
	if base.OpModTargetW != nil {
		v := *base.OpModTargetW
		snap.OpModTargetW = &v
	}
	if base.OpModTargetVar != nil {
		v := *base.OpModTargetVar
		snap.OpModTargetVar = &v
	}
	if base.OpModConnect != nil {
		v := *base.OpModConnect
		snap.OpModConnect = &v
	}
	if base.OpModEnergize != nil {
		v := *base.OpModEnergize
		snap.OpModEnergize = &v
	}
	return snap
}

// singletonKey mirrors core's handlers/singleton.SingletonKey ("default"),
// the fixed key core's DefaultDERControlHandler get/put's every singleton
// resource under. Duplicated here (rather than importing the handlers
// package, which pulls in an http.HandlerFunc surface this file has no
// use for) because it is a one line constant, not a shared abstraction:
// per the workspace Go standard, a little copying is better than a little
// dependency.
const singletonKey = "default"

// isNotFound reports whether err is (or wraps) store.ErrNotFound.
func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}
