package sep2embed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/assembly"
	coresub "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/handlers/subscription"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/store"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// GAGO-034 DOWN path: GridAPPS-D control deltas -> DERControl.
//
// A ControlDelta is the same shape internal/cim/diff already builds for
// the platform's simulation input topic (diff.Difference: Object,
// Attribute, Value). This bridge does not invent a parallel wire
// format: the Python upstream reference,
// ieee_2030_5/adapters/gridappsd_adapter.py:_input_detected (in the
// gridappsd-2030_5 project this bridge reproduces), decodes exactly
// this shape off an app-input topic and requires Attribute to start
// with "DERControl", with the concrete branch it exercises being
// "DERControl.DERControlBase.<Field>" (obj_path[1] == 'DERControlBase'
// and len(obj_path) == 3). ApplyControlDelta below implements that same
// dot-path convention against core's DERControl store.
type ControlDelta = diff.Difference

// derControlAttributePrefix is the only Attribute shape ApplyControlDelta
// accepts. Any other shape (a bare "DERControl.<Field>", or a
// non-DERControl attribute entirely) is refused, not silently ignored:
// see ApplyControlDelta's doc comment.
const derControlAttributePrefix = "DERControl.DERControlBase."

// controlFSAID and controlDERProgramID are the fixed device-level
// FSA/DERProgram identifiers ApplyControlDelta seeds and writes under.
// This bridge maintains exactly one DERProgram per device (mirrors
// seed.go's DER id="1" convention: one default DER per device, not a
// multi-DER inventory), so a single, well-known (fsa, derp) pair is
// sufficient; there is no per-device schedule of competing programs to
// disambiguate.
const (
	controlFSAID        = "1"
	controlDERProgramID = "1"
)

// activeControlID is the single "active" DERControl slot ApplyControlDelta
// maintains per device. A second delta for the same device updates the
// SAME DERControl (merging the new field into its existing
// DERControlBase) rather than creating a second, competing control: see
// ApplyControlDelta's doc comment for why this is the correct
// supersede-shaped behavior for this bridge, not an accidental
// singleton limitation.
const activeControlID = "active"

// ErrUnknownControlDevice is returned by ApplyControlDelta when the
// delta's Object does not resolve to a registered, seeded EndDevice.
// Per data-invariants Rule 2, this is a refusal, never a fallback: the
// caller must not synthesize a default device for an unmapped mRID.
var ErrUnknownControlDevice = errors.New("sep2embed: control delta targets an unregistered device")

// ErrUnsupportedControlAttribute is returned by ApplyControlDelta when
// Attribute is not the recognized "DERControl.DERControlBase.<Field>"
// shape, or names a Field this bridge does not (yet) map.
var ErrUnsupportedControlAttribute = errors.New("sep2embed: unsupported control delta attribute")

// derProgramListHref returns the canonical href for the DERProgramList
// scoped to (edev, fsa). Mirrors the server-of-record's own
// derProgramListHref helper (internal/server/test_mutations.go) and
// core's own route: "GET /edev/{id}/fsa/{fsaId}/derp".
func derProgramListHref(edevID, fsaID string) string {
	return "/edev/" + edevID + "/fsa/" + fsaID + "/derp"
}

// derControlScope returns the composite parent key core's DERControls
// scoped store keys on: (EndDeviceID, FSAID, DERProgramID). Mirrors the
// server-of-record's derControlScope helper and
// assembly.scopedListHandlerDeep's own key construction
// ("id/fsaId/derpId"), so a DERControl this bridge creates is reachable
// at exactly the GET route CSIP clients already poll.
func derControlScope(edevID, fsaID, derpID string) string {
	return edevID + "/" + fsaID + "/" + derpID
}

// ApplyControlDelta maps one GridAPPS-D control delta onto the owning
// device's DERControl, then fans out a Changed notification to
// subscribers of that device's DERProgramList, mirroring the
// server-of-record's own handleDERControlAdd test-mutation hook
// (internal/server/test_mutations.go) end to end: same scope key, same
// notify target, same NotificationStatusChanged.
//
// Owner scoping (data-invariants / GAGO-043): delta.Object is a CIM
// device mRID, resolved to the owning device's LFDI via reg (the SAME
// bidirectional mRID<->LFDI mapping seed.go seeds stores.EndDevices
// from; no parallel device map is introduced here). The resulting
// DERControl is written ONLY under that device's own
// (edevID, fsa, derp) scope. A delta whose Object does not resolve to a
// registered, seeded EndDevice is refused with ErrUnknownControlDevice
// and stores.DERControls is left untouched for every device, including
// the intended target: this function never guesses a fallback device.
//
// Field-value fidelity: the delta's Value is decoded and assigned to
// exactly the DERControlBase field named by Attribute (see
// applyDERControlBaseField). ActivePower/ReactivePower's
// multiplier+value pair is carried through unchanged except for the
// explicit, currently-no-op activeSignFlip / reactiveSignFlip seam (see
// their doc comment): no other unit or sign conversion happens.
//
// Supersede semantics: a second delta for the same device does not
// create a second DERControl. ApplyControlDelta reads the device's
// existing "active" DERControl (if any), merges the new field into its
// DERControlBase (previously-set fields on other attributes are
// preserved), and writes it back with Update. This avoids the
// duplicate-conflicting-controls failure mode data-invariants warns
// about: DERControlBase is naturally a bag of independent op-mode
// fields (opModTargetW and opModTargetVar can both be active
// simultaneously), so "one active control per device, fields merged in"
// is the correct model, not an arbitrary limitation.
//
// defaultControl is GAGO-050's seed value for the DERProgram's
// DefaultDERControl singleton, forwarded unchanged to ensureDERProgram.
// It is sourced by the caller from SEP2Policy.DefaultControl
// (cmd/bridge/main.go), never hardcoded here: ApplyControlDelta itself
// carries no opinion on the value, only the plumbing to seed it once
// per (edevID, fsaID, derpID).
func ApplyControlDelta(ctx context.Context, stores *assembly.Stores, notifier *coresub.Manager, reg *registry.Registry, defaultControl sep2.DefaultDERControl, delta ControlDelta) error {
	field, ok := strings.CutPrefix(delta.Attribute, derControlAttributePrefix)
	if !ok || field == "" {
		return fmt.Errorf("%w: attribute %q (want prefix %q)", ErrUnsupportedControlAttribute, delta.Attribute, derControlAttributePrefix)
	}

	// edevID must be the device's ADVERTISED store id, which since
	// IEEECORE-URLINDEX is the opaque URL index rather than the LFDI.
	// reg.Get resolves the delta's mRID to its Entry; the index allocator
	// then maps that same mRID (its device key, as used by seed.go) to the
	// id the device is actually seeded and advertised under.
	entry, ok := reg.Get(delta.Object)
	if !ok {
		return fmt.Errorf("%w: mrid=%q", ErrUnknownControlDevice, delta.Object)
	}

	// IndexFor, not Allocate: this path must never mint an index. An mRID
	// with no assignment means the device was never seeded, which is the
	// drift condition the check below exists to catch. Allocating here would
	// manufacture an id for a device that has no EndDevice record and turn a
	// clean failure into a dangling control.
	edevID, ok := stores.EndDeviceIndexes.IndexFor(entry.MRID)
	if !ok {
		return fmt.Errorf("%w: mrid=%q has no seeded URL index", ErrUnknownControlDevice, entry.MRID)
	}

	// Defense in depth: the registry and stores.EndDevices are seeded
	// together (bridge.bootstrapRegistry + sep2embed.New), but if they
	// were ever to drift, fail closed rather than write a DERControl
	// with no corresponding seeded device.
	if _, err := stores.EndDevices.Get(ctx, edevID); err != nil {
		return fmt.Errorf("%w: edev %q (mrid=%q) not seeded: %v",
			ErrUnknownControlDevice, edevID, entry.MRID, err)
	}

	if err := ensureDERProgram(ctx, stores, edevID, entry.LFDI, controlFSAID, controlDERProgramID, defaultControl); err != nil {
		return fmt.Errorf("sep2embed: control delta: ensure der program: %w", err)
	}

	scope := derControlScope(edevID, controlFSAID, controlDERProgramID)
	controlStore := stores.DERControls.ForParent(scope)

	existing, err := controlStore.Get(ctx, activeControlID)
	var base sep2.DERControlBase
	isUpdate := false
	switch {
	case err == nil:
		isUpdate = true
		if existing.DERControlBase != nil {
			base = existing.DERControlBase.Copy()
		}
	case errors.Is(err, store.ErrNotFound):
		// Fresh control: base starts zero-valued.
	default:
		return fmt.Errorf("sep2embed: control delta: read existing control: %w", err)
	}

	if err := applyDERControlBaseField(&base, field, delta.Value); err != nil {
		return fmt.Errorf("sep2embed: control delta: %w", err)
	}

	control := sep2.DERControl{}
	control.Href = "/edev/" + edevID + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID + "/derc/" + activeControlID
	control.MRID = entry.LFDI + "-" + activeControlID
	control.EventStatus = &sep2.EventStatus{
		CurrentStatus: sep2.EventStatusActive,
		DateTime:      time.Now().UTC().Unix(),
	}
	control.DERControlBase = &base

	if isUpdate {
		err = controlStore.Update(ctx, activeControlID, control)
	} else {
		err = controlStore.Create(ctx, activeControlID, control)
	}
	if err != nil {
		return fmt.Errorf("sep2embed: control delta: write control: %w", err)
	}

	if notifier != nil {
		notifier.Notify(ctx, derProgramListHref(edevID, controlFSAID), sep2.NotificationStatusChanged)
	}

	return nil
}

// ensureDERProgram get-or-creates a minimal, valid DERProgram at
// (edevID, derpID) so a subsequent DERControl write satisfies the
// server-of-record's own precondition (handleDERControlAdd: "Verify the
// parent DERProgram exists"). DERPrograms are scoped by EndDeviceID
// alone (the fsaID path segment is accepted but not part of the store
// key: this mirrors core's own scopedListHandler and the
// server-of-record's documented contract; it is core's existing
// behavior, not something introduced here).
//
// GAGO-050: the same lazy-creation moment also seeds this program's
// DefaultDERControl singleton (into stores.DefaultDERControls, keyed by
// derControlScope + singletonKey, mirroring core's own
// DefaultDERControlHandler parent-key derivation) and points the new
// program's DefaultDERControlLink at it. This closes the CSIP-mandatory
// hole Devi flagged: a client that GETs this DERProgram and follows
// DefaultDERControlLink must find a well-formed DefaultDERControl, not
// an absent one. defaultControl is the caller-supplied seed value
// (sourced from SEP2Policy.DefaultControl, never hardcoded here); it is
// written verbatim except for Href/MRID, which this function stamps to
// match the program's own scope.
//
// This does not modify seed.go: seed.go's EndDevice/DER seeding stays
// untouched (per this card's hard rule); the DERProgram (and its
// DefaultDERControl) this function creates is control-flow plumbing
// local to the DOWN path, created lazily on first use rather than at
// bulk seed time.
// edevID addresses the resources (it is the URL index); mridBase is the
// device's canonical LFDI and seeds the wire-level MRID fields. The two are
// deliberately separate arguments: an MRID is an identity value that must not
// become a URL-addressing artifact, and building one from the index would
// make it collide across restarts once indices are reassigned.
func ensureDERProgram(ctx context.Context, stores *assembly.Stores, edevID, mridBase, fsaID, derpID string, defaultControl sep2.DefaultDERControl) error {
	inner := stores.DERPrograms.ForParent(edevID)
	if _, err := inner.Get(ctx, derpID); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("get der program: %w", err)
	}

	dderc := defaultControl.Copy()
	dderc.Href = "/edev/" + edevID + "/fsa/" + fsaID + "/derp/" + derpID + "/dderc"
	dderc.MRID = mridBase + "-dderc"

	scope := derControlScope(edevID, fsaID, derpID)
	if err := stores.DefaultDERControls.Create(ctx, scope, singletonKey, dderc); err != nil {
		return fmt.Errorf("create default der control: %w", err)
	}

	program := sep2.DERProgram{Primacy: 1}
	program.Href = "/edev/" + edevID + "/fsa/" + fsaID + "/derp/" + derpID
	program.DERControlListLink = &sep2.ListLink{
		Href: "/edev/" + edevID + "/fsa/" + fsaID + "/derp/" + derpID + "/derc",
	}
	program.DefaultDERControlLink = &sep2.Link{Href: dderc.Href}

	if err := inner.Create(ctx, derpID, program); err != nil {
		return fmt.Errorf("create der program: %w", err)
	}
	return nil
}

// activeSignFlip and reactiveSignFlip are the explicit, testable seam
// for the CIM-vs-IEEE-2030.5 sign convention on the real/reactive power
// target mappings below (Vance, power-systems review of GAGO-034 PR #9,
// HIGH-2). GridAPPS-D's PowerElectronicsConnection p/q carries the
// classic CIM load-vs-generator sign ambiguity: some CIM profiles and
// tools report p/q positive as consumed (load convention), others
// positive as produced (generator convention), and which one a given
// GridAPPS-D feeder model and app use is not something this bridge can
// infer from the wire alone. IEEE 2030.5 section 10.10 defines
// opModTargetW positive as discharging/exporting (generator-positive)
// and opModTargetVar positive as over-excited/injecting VARs.
//
// The working default below (false, false: no flip on either side)
// assumes the GridAPPS-D side is ALREADY generator-positive, matching
// IEEE 2030.5 with no conversion needed. This is Vance's placeholder,
// NOT a verified physical-direction claim: it is unverified pending a
// co-simulation loopback (GAGO-044: Hale runs OpenDSS and asserts the
// inverter actually moves in the commanded direction end to end). Until
// GAGO-044 closes, this DOWN path is dev-only and MUST NOT be pointed
// at a real inverter. Flipping either constant changes the sign of
// every OpModTargetW / OpModTargetVar value this bridge writes; see
// TestSignFlipConstantsPinnedEffect, which locks today's numeric effect
// of each constant (not a claim about which effect is physically
// correct) so an accidental flip is caught by a failing test rather
// than silently changing every commanded device's direction.
const (
	activeSignFlip   = false
	reactiveSignFlip = false
)

// applyDERControlBaseField decodes value and assigns it to the
// DERControlBase field named by field, in place. Supported fields cover
// the real/reactive power target and the connect/energize booleans
// (Vance confirmed these mappings are dimensionally correct: GridAPPS-D
// dispatches absolute watts/vars, and OpModTargetW/OpModTargetVar are
// absolute-power types). An unrecognized field, INCLUDING
// opModFixedW/opModFixedVar/opModMaxLimW (removed below, HIGH-1), is
// refused (ErrUnsupportedControlAttribute) rather than silently
// dropped.
//
// opModFixedW, opModFixedVar, and opModMaxLimW are deliberately NOT
// mapped: per IEEE 2030.5 section 10.10 these are PERCENT types
// (SignedPercent / PercentLimit / FixedVar, a percentage of the
// device's rated capability), not absolute watts/vars. Mapping a
// GridAPPS-D absolute-power delta directly onto a percent field would
// silently command the wrong physical setpoint (a "5000" watt delta
// read back as "5000%"). Converting correctly requires the device's
// rated capability (DERCapability), and this bridge's DERCapabilities
// store exists but is never seeded (no rtg values available yet), so
// there is no reference to convert against. Percent-mode support
// returns once DERCapability rtg values are seeded: GAGO-045.
func applyDERControlBaseField(base *sep2.DERControlBase, field string, value any) error {
	switch field {
	case "opModTargetW":
		ap, err := decodeActivePower(value)
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		base.OpModTargetW = flipActivePowerSign(ap, activeSignFlip)
	case "opModTargetVar":
		rp, err := decodeReactivePower(value)
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		base.OpModTargetVar = flipReactivePowerSign(rp, reactiveSignFlip)
	case "opModConnect":
		b, err := decodeBool(value)
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		base.OpModConnect = b
	case "opModEnergize":
		b, err := decodeBool(value)
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		base.OpModEnergize = b
	default:
		return fmt.Errorf("%w: field %q", ErrUnsupportedControlAttribute, field)
	}
	return nil
}

// flipActivePowerSign negates ap.Value in place (returning a copy) when
// flip is true; a nil ap or flip=false returns ap unchanged. See
// activeSignFlip's doc comment for what flip means and why it is not
// yet a verified physical-direction claim.
func flipActivePowerSign(ap *sep2.ActivePower, flip bool) *sep2.ActivePower {
	if ap == nil || !flip {
		return ap
	}
	cp := *ap
	cp.Value = -cp.Value
	return &cp
}

// flipReactivePowerSign is flipActivePowerSign's ReactivePower
// counterpart; see reactiveSignFlip's doc comment.
func flipReactivePowerSign(rp *sep2.ReactivePower, flip bool) *sep2.ReactivePower {
	if rp == nil || !flip {
		return rp
	}
	cp := *rp
	cp.Value = -cp.Value
	return &cp
}

// decodeActivePower accepts either a already-typed sep2.ActivePower (the
// convenience shape a caller constructing a ControlDelta in Go can use
// directly) or the map[string]any{"multiplier":,"value":} shape
// encoding/json produces when the delta arrived as JSON off the wire
// (matching the Python upstream's `m.ActivePower(**item['value'])`
// unpacking). Any other shape is refused.
func decodeActivePower(value any) (*sep2.ActivePower, error) {
	switch v := value.(type) {
	case sep2.ActivePower:
		return &v, nil
	case *sep2.ActivePower:
		if v == nil {
			return nil, errors.New("nil *sep2.ActivePower")
		}
		cp := *v
		return &cp, nil
	case map[string]any:
		mult, val, err := decodeMultiplierValue(v)
		if err != nil {
			return nil, err
		}
		val16, err := toInt16Checked(val)
		if err != nil {
			return nil, fmt.Errorf("ActivePower: %w", err)
		}
		return &sep2.ActivePower{Multiplier: mult, Value: val16}, nil
	default:
		return nil, fmt.Errorf("unsupported ActivePower value type %T", value)
	}
}

// decodeReactivePower is decodeActivePower's ReactivePower counterpart;
// see its doc comment for the accepted shapes.
func decodeReactivePower(value any) (*sep2.ReactivePower, error) {
	switch v := value.(type) {
	case sep2.ReactivePower:
		return &v, nil
	case *sep2.ReactivePower:
		if v == nil {
			return nil, errors.New("nil *sep2.ReactivePower")
		}
		cp := *v
		return &cp, nil
	case map[string]any:
		mult, val, err := decodeMultiplierValue(v)
		if err != nil {
			return nil, err
		}
		val16, err := toInt16Checked(val)
		if err != nil {
			return nil, fmt.Errorf("ReactivePower: %w", err)
		}
		return &sep2.ReactivePower{Multiplier: mult, Value: val16}, nil
	default:
		return nil, fmt.Errorf("unsupported ReactivePower value type %T", value)
	}
}

// decodeMultiplierValue extracts the "multiplier" and "value" numeric
// fields a JSON-decoded power object carries. encoding/json decodes JSON
// numbers into float64 regardless of the source's int/float lexical
// form, so both fields are read as float64 and narrowed; a fractional
// value in either field is refused rather than silently truncated
// (data-invariants: no synthesized-wrong-value fallback).
func decodeMultiplierValue(m map[string]any) (multiplier int8, value int64, err error) {
	multRaw, ok := m["multiplier"]
	if !ok {
		return 0, 0, errors.New(`missing "multiplier"`)
	}
	valRaw, ok := m["value"]
	if !ok {
		return 0, 0, errors.New(`missing "value"`)
	}

	multF, ok := toFloat64(multRaw)
	if !ok {
		return 0, 0, fmt.Errorf(`"multiplier": unsupported type %T`, multRaw)
	}
	valF, ok := toFloat64(valRaw)
	if !ok {
		return 0, 0, fmt.Errorf(`"value": unsupported type %T`, valRaw)
	}

	if multF != float64(int8(multF)) {
		return 0, 0, fmt.Errorf(`"multiplier" %v is not an integer in int8 range`, multRaw)
	}
	if valF != float64(int64(valF)) {
		return 0, 0, fmt.Errorf(`"value" %v is not an integer in int64 range`, valRaw)
	}

	return int8(multF), int64(valF), nil
}

// toFloat64 narrows the numeric JSON-decoded types (float64 always,
// plus int/int64 for a caller that built the map programmatically
// rather than via json.Unmarshal) to float64.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// decodeBool accepts a Go bool, a *bool, or nothing else. JSON booleans
// decode to bool via encoding/json, so this covers both the
// programmatic-Go-caller and JSON-off-the-wire cases.
func decodeBool(value any) (*bool, error) {
	switch v := value.(type) {
	case bool:
		return &v, nil
	case *bool:
		if v == nil {
			return nil, errors.New("nil *bool")
		}
		cp := *v
		return &cp, nil
	default:
		return nil, fmt.Errorf("unsupported bool value type %T", value)
	}
}
