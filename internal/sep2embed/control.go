package sep2embed

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/assembly"
	coresub "github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/handlers/subscription"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/store"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// DOWN path: GridAPPS-D control deltas -> DERControl.
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

// derControlListHref returns the canonical href of the DERControlList
// scoped to (edev, fsa, derp), which is both the list route a client polls
// and the prefix every issued control's own href extends.
func derControlListHref(edevID, fsaID, derpID string) string {
	return "/edev/" + edevID + "/fsa/" + fsaID + "/derp/" + derpID + "/derc"
}

// ErrUnknownControlDevice is returned by ApplyControlDelta when the
// delta's Object does not resolve to a registered, seeded EndDevice.
// Per data-invariants Rule 2, this is a refusal, never a fallback: the
// caller must not synthesize a default device for an unmapped mRID.
var ErrUnknownControlDevice = errors.New("sep2embed: control delta targets an unregistered device")

// ErrUnsupportedControlAttribute is returned by ApplyControlDelta when
// Attribute is not the recognized "DERControl.DERControlBase.<Field>"
// shape, or names a Field this bridge does not (yet) map.
var ErrUnsupportedControlAttribute = errors.New("sep2embed: unsupported control delta attribute")

// ErrDERControlDurationUnset is returned by ApplyControlDelta when the
// supplied ControlPolicy carries a zero DERControlSeed.Duration.
//
// This is a refusal, not a fallback to some invented window, for the same
// data-invariants reason ErrUnknownControlDevice is: writing the control
// anyway would produce an interval whose end equals its start, which a
// conformant client expires on arrival. The client would still fetch it,
// parse it, and POST a conformant DERControlResponse, so the failure would
// look exactly like success from every vantage point except the device that
// never moved. Refusing puts the error where an operator can see it.
var ErrDERControlDurationUnset = errors.New("sep2embed: DERControl interval duration is not configured")

// ErrControlDeltaRateUnrepresentable is returned by ApplyControlDelta when
// stamping a creationTime strictly newer than every overlapping same-mode
// control already issued would push the stamp more than
// maxCreationTimeLeadSeconds ahead of the wall clock.
//
// It is a refusal rather than a clamp for the reason ErrUnknownControlDevice
// is a refusal: the two alternatives both corrupt something silently. Clamping
// the stamp to the bound would make it EQUAL to a control already issued, and
// equal creationTimes compare false in both directions under the client's
// strict-greater comparison (2018 rule f) p.90), so the incoming control would
// be served and then discarded by every conformant client: the exact defect
// the tie-break exists to prevent. Letting the stamp run would keep the server
// issuing events whose creationTime drifts arbitrarily far from the instant
// they were created. Refusing puts the condition where an operator can see it.
//
// The condition it reports is real and not a bug in the caller: IEEE 2030.5
// orders events by a one-second TimeType (sep.xsd:6382), so more than one
// CHANGED setpoint per second per control mode is not a rate this protocol can
// express. See maxCreationTimeLeadSeconds.
var ErrControlDeltaRateUnrepresentable = errors.New("sep2embed: control delta rate exceeds the one-second event ordering IEEE 2030.5 can represent")

// DERProgramSeed carries the operator-configurable fields of the DERProgram
// this package seeds and lazily creates. It mirrors
// sep2config.DERProgramPolicy, which is where the values and their rationale
// live; the shape is duplicated here rather than imported for the same reason
// Config mirrors the rest of SEP2Policy: sep2config is a policy-only package
// that knows nothing about stores, and this package takes plain values so the
// dependency does not run the wrong way.
//
// Only these two fields are carried. mRID is derived (deriveMRID) and every
// link is structural, so neither is something an operator can usefully set.
type DERProgramSeed struct {
	// Primacy is DERProgram.primacy. sep.xsd makes it minOccurs=1, so there
	// is no absent state, and the zero value is a real primacy (highest
	// priority) rather than a stand-in for unset. Callers source it from
	// sep2config.SEP2Policy.DefaultProgram.
	Primacy uint8

	// Description is DERProgram.description, bounded at 32 characters by
	// sep.xsd's String32. Validated by
	// sep2config.SEP2Policy.ValidateDefaultProgram at boot; empty is valid
	// and marshals as absent.
	Description string
}

// DERControlSeed carries the temporal shape stamped onto every DERControl
// ApplyControlDelta issues. It mirrors sep2config.DERControlPolicy, which is
// where the values, their bounds and their reasoning live; the shape is
// duplicated here rather than imported for the same reason DERProgramSeed
// duplicates DERProgramPolicy: sep2config is a policy-only package that knows
// nothing about stores, so this package takes plain values and the dependency
// cannot run the wrong way.
//
// Callers source Duration and RandomizeDuration from
// sep2config.SEP2Policy.DERControl, validated by ValidateDERControl at boot.
// The zero value is NOT usable: a zero Duration is the defect this seed
// exists to fix (see Duration below), which is why boot validation is a hard
// reject rather than a warning.
type DERControlSeed struct {
	// Duration is the DateTimeInterval.duration, in seconds, of every issued
	// control. Zero produces an event whose end equals its start, which a
	// conformant client expires the instant it arrives.
	Duration uint32

	// RandomizeDuration is the randomizeDuration element served on every
	// issued control, in seconds, bounded to -3600..3600 by boot validation.
	// It is served explicitly even at 0.
	RandomizeDuration int32

	// Now is the clock the interval start, creationTime and EventStatus
	// timestamp are read from. Nil uses time.Now, which is what every
	// non-test caller passes.
	//
	// It exists because the values this seed produces are asserted at the
	// BYTE level: a test that cannot fix the clock cannot state the exact
	// document a client receives, and an approximate assertion would not
	// have caught the absent-element defect this work fixes. Same seam, and
	// same nil-means-time.Now contract, as telemetrypub.Config.Now.
	Now func() time.Time
}

// now returns the seed's clock, defaulting to time.Now when unset.
func (s DERControlSeed) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// ControlPolicy bundles the three operator-configured values the DOWN
// control path needs: the fallback control, the program both it and every
// issued control hang off, and the temporal shape of those issued controls.
//
// They travel together because they are only meaningful together. The
// DefaultDERControl is what applies once an issued control's interval
// elapses, that interval comes from Control, and neither resource is
// reachable except through Program. Grouping them also keeps
// ApplyControlDelta's signature from growing a parameter every time the
// operator surface does.
type ControlPolicy struct {
	// DefaultControl is the DefaultDERControl singleton seeded onto each
	// DERProgram's DefaultDERControlLink, written verbatim except for the
	// Href and MRID createDERProgram stamps. Sourced from
	// sep2config.SEP2Policy.DefaultControl.
	DefaultControl sep2.DefaultDERControl

	// Program is the DERProgram seeded for every device, sourced from
	// sep2config.SEP2Policy.DefaultProgram.
	Program DERProgramSeed

	// Control is the interval and randomization policy for issued
	// DERControls, sourced from sep2config.SEP2Policy.DERControl.
	Control DERControlSeed
}

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
// Owner scoping (data-invariants): delta.Object is a CIM
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
// SUPERSEDE SEMANTICS. Every delta issues a NEW DERControl,
// carrying exactly the one control mode the delta names, with its own mRID,
// its own href and a creationTime strictly newer than any overlapping control
// on the same mode. No previously issued control is ever rewritten. Prior
// controls stay in the DERControlList, fetchable at their own hrefs, and are
// only marked (2018 EventStatus 4) when a newer control covers the same
// control set over an overlapping window. See supersede.go for the clause
// chain; the short version is that 2018 rules q)2) p.91 and t)3) p.92 forbid
// editing a served Event, 10.2.5.6 p.95 makes a client discard a repeated
// mRID as a duplicate, and CSIP v2.0 section 4.4.1 lines 282 to 283 names the
// remedy as "a new DERControl ... to supersede or cancel the existing
// DERControl".
//
// This REPLACES an earlier merge-into-one-control model, and the difference
// is visible to a client rather than internal. Two deltas on DIFFERENT modes
// used to produce one control carrying both; they now produce two independent
// controls that both stay active, which is what 2018 rule t) p.91 requires
// ("differing controls ... are independent and are allowed to overlap or nest
// without superseding") and what CTP BASIC-024 through BASIC-026 test by
// requiring the client to execute BOTH. The earlier setpoint is not lost when
// a delta on another mode arrives: it remains in force as its own event, per
// rule t)1) and t)2) p.92, rather than by being copied forward into a
// rewritten record.
//
// TEMPORAL PLACEMENT. Every control written here
// carries a creationTime and an interval, both minOccurs=1 on Event
// (sep.xsd:5578 and :5584). Neither is decoration and neither can be left to
// a downstream layer:
//
//   - interval absent makes a client compute end = start + duration = 0,
//     find end <= now on arrival, and mark the event expired. It fetches the
//     control, parses it, POSTs a conformant DERControlResponse, and then
//     discards it, so the entire control path completes with nothing
//     actuated. That was this bridge's observed behavior before this change.
//   - creationTime absent (serialized as 0) makes supersession undecidable.
//     A client orders two overlapping controls of equal primacy by
//     creationTime and compares with a strict greater-than, so two zeros
//     compare false in both directions and the INCOMING control is the one
//     discarded. Fixing the interval alone would leave a server that cannot
//     replace a setpoint it has already issued, which is the standard's own
//     mechanism for changing one.
//
// Both are stamped fresh on every issued control. Because each delta now
// issues its own event rather than rewriting one, the creationTime carried by
// a superseding control IS what tells the client which of two overlapping
// controls to run (rule f) p.90: the larger creationTime is newer), and the
// interval start is when the new setpoint takes effect.
//
// creationTime is also the ONE field the same-second tie-break moves. The
// interval start and EventStatus.dateTime are the wall clock as read, never
// the bumped stamp, so no event is ever served with a start in the future or
// with a status timestamp that has not happened yet. See
// nextEventCreationTime for why the bump exists and what bounds it, and
// newEventStatus for the two clauses that fix the other two fields.
//
// CHANGE BOUND. A delta whose payload is byte-for-byte the payload
// already in force for its own control modes issues NOTHING: no event, no
// store write, no subscriber notification. Without that test the bound is a
// bound on the delta RATE rather than on CHANGE, because a restatement one
// second later derives a different creationTime, hence a different mRID and a
// different store id, so the store's own ErrAlreadyExists dedup never sees it.
// A platform restating its setpoints every timestep, which is the likely
// production cadence, then minted one event per timestep: at the shipped
// 1800-second duration and one delta per second, 1800 resident controls per
// device, with each delta paying an unbounded List plus an O(n) classify.
//
// The bound is scoped to the control still IN FORCE, not to every control the
// device has ever held. A restatement arriving after the previous control's
// window has closed is not redundant: the device has already reverted to the
// DefaultDERControl, so suppressing it would leave the platform's standing
// setpoint uncommanded for as long as it kept restating it. See
// restatesControlInForce.
//
// END OF LIFE. The interval stamped here is also what takes the
// control back OUT of service: lifecycle.go removes it at the close of its
// maximum Effective Scheduled Period. That removal is what bounds the
// collection this function appends to, and it is not performed here; the
// callers that drive it are Embed.ApplyControlDelta and Embed.Run's sweep.
//
// policy carries all three operator-configured inputs. DefaultControl is
// the seed value for the DERProgram's DefaultDERControl singleton,
// forwarded unchanged to ensureDERProgram. Program is the matching policy
// for the DERProgram itself; since boot seeding now creates a program for
// every registered device, ensureDERProgram below is a fallback for a device
// seeding did not cover, and it takes the same policy so the two paths
// cannot serve different programs for the same fleet. Control supplies the
// interval duration and randomizeDuration. Every one is sourced by the
// caller from sep2config.SEP2Policy (cmd/bridge/main.go) and none is
// hardcoded here: ApplyControlDelta carries no opinion on the values, only
// the plumbing to stamp them.
func ApplyControlDelta(ctx context.Context, stores *assembly.Stores, notifier *coresub.Manager, reg *registry.Registry, policy ControlPolicy, delta ControlDelta) error {
	// Checked before anything is resolved or written, so a misconfigured
	// bridge cannot create a DERProgram or a DefaultDERControl as a side
	// effect of a delta it is going to refuse.
	if policy.Control.Duration == 0 {
		return fmt.Errorf("%w: set -sep2-control-duration to at least 1 second", ErrDERControlDurationUnset)
	}

	field, ok := strings.CutPrefix(delta.Attribute, derControlAttributePrefix)
	if !ok || field == "" {
		return fmt.Errorf("%w: attribute %q (want prefix %q)", ErrUnsupportedControlAttribute, delta.Attribute, derControlAttributePrefix)
	}

	// edevID must be the device's ADVERTISED store id: the opaque URL
	// index rather than the LFDI.
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

	if err := ensureDERProgram(ctx, stores, edevID, entry.LFDI, controlFSAID, controlDERProgramID, policy); err != nil {
		return fmt.Errorf("sep2embed: control delta: ensure der program: %w", err)
	}

	scope := derControlScope(edevID, controlFSAID, controlDERProgramID)

	// The issued control carries EXACTLY the mode this delta names. It does
	// not inherit the modes of previously issued controls: those remain in
	// force as their own events (2018 rule t)1) and t)2) p.92), and copying
	// them forward would make every delta's control set a superset of the
	// last, turning independent modes into same-set supersessions.
	var base sep2.DERControlBase
	if err := applyDERControlBaseField(&base, field, delta.Value); err != nil {
		return fmt.Errorf("sep2embed: control delta: %w", err)
	}

	// Everything already stored for this device, read before anything is
	// written, so both the creation-instant guard and the supersession pass
	// below see the same snapshot.
	//
	// Unbounded is the correct paging choice for a server-internal read over
	// a collection the server itself bounds (see store.ListOptions.Unbounded):
	// a page here would silently hide events from the supersession pass, and
	// an event that is not examined is an event that is left Active.
	priorList, err := stores.DERControls.List(ctx, scope, store.ListOptions{Unbounded: true})
	if err != nil {
		return fmt.Errorf("sep2embed: control delta: list existing controls: %w", err)
	}

	// ONE clock read for the whole event. interval.start and
	// EventStatus.dateTime are both this instant, and reading the clock twice
	// could land them on different seconds, which a client comparing them has
	// no way to interpret. creationTime is derived from it below and is the
	// only one of the three that may differ, by the bounded tie-break.
	wallUnix := policy.Control.now().UTC().Unix()

	// The change bound: nothing is written for a delta that restates the setpoint
	// already in force. This runs BEFORE the creation instant is chosen and
	// before anything is written, so a restatement costs one List and one
	// comparison and leaves the served collection byte-identical: the event
	// the client already holds keeps its own mRID, its own window and its own
	// response cycle, which is the whole point of not re-issuing it.
	restatement, err := restatesControlInForce(priorList.Items, &base, wallUnix)
	if err != nil {
		return fmt.Errorf("sep2embed: control delta: %w", err)
	}
	if restatement {
		return nil
	}

	creationTime, err := nextEventCreationTime(priorList.Items, base, wallUnix)
	if err != nil {
		return fmt.Errorf("sep2embed: control delta: %w", err)
	}

	mrid, err := deriveEventMRID(mridKindDERControl, entry.LFDI, creationTime, &base)
	if err != nil {
		return fmt.Errorf("sep2embed: control delta: %w", err)
	}
	controlID := derControlID(creationTime, mrid)

	control := sep2.DERControl{}
	control.Href = derControlListHref(edevID, controlFSAID, controlDERProgramID) + "/" + controlID
	// Schema-valid hexBinary(16), and unique to THIS event rather than
	// constant per device: a conformant client aborts the whole
	// DERControlList parse on a non-hex mRID, and discards a repeated one as
	// a duplicate of an event it already holds. See deriveEventMRID.
	control.MRID = mrid
	// Required element. It is the sole tiebreaker between two overlapping
	// controls of equal primacy (rule f) p.90), which is the mechanism this
	// whole path depends on now that a setpoint change is a second event
	// rather than a rewrite of the first.
	control.CreationTime = creationTime
	// Required element. start is the wall clock, NOT the possibly-bumped
	// creationTime, because the platform's delta means "this setpoint, from
	// here" and a tie-break between two events issued in one second says
	// nothing about when either setpoint was asked for. Keeping it here is
	// also what makes the Active below correct by construction rather than by
	// luck (sep.xsd:5603). duration is operator policy, because the
	// GridAPPS-D delta contract carries no window of its own.
	control.Interval = &sep2.DateTimeInterval{
		Start:    wallUnix,
		Duration: policy.Control.Duration,
	}
	control.EventStatus = newEventStatus(servedEventEdition, control.Interval.Start, wallUnix)
	// Served explicitly, including at 0. The value is addressable rather
	// than inlined because the field is a pointer whose nil means "element
	// absent"; a pointer to 0 still reaches the wire as
	// <randomizeDuration>0</randomizeDuration>, which states the policy
	// instead of relying on the client to apply the schema's own default.
	// New refuses a Config.DERControl.RandomizeDuration outside the
	// OneHourRangeType bound (sep2config.RandomizeDurationInRange), so
	// this narrowing never wraps.
	randomizeDuration := sep2.OneHourRange(policy.Control.RandomizeDuration)
	control.RandomizeDuration = &randomizeDuration
	control.DERControlBase = &base

	// Create BEFORE marking predecessors, and in that order deliberately. If
	// the create fails after the marks were applied, the device would be left
	// with every control superseded and no replacement: a fail-open on the
	// physical side. In the order used here a failure to mark leaves both
	// controls Active, which the client still resolves correctly on its own
	// from creationTime per rule f) p.90.
	//
	// ErrAlreadyExists is not an error condition. The id is a pure function
	// of the event's content and creation instant, so an existing entry under
	// this id IS this event, already published; there is nothing to write and
	// nothing to change.
	if err := stores.DERControls.Create(ctx, scope, controlID, control); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return fmt.Errorf("sep2embed: control delta: write control: %w", err)
	}

	if err := supersedePriorControls(ctx, stores.DERControls, scope, priorList.Items, control); err != nil {
		return fmt.Errorf("sep2embed: control delta: %w", err)
	}

	if notifier != nil {
		notifier.Notify(ctx, derProgramListHref(edevID, controlFSAID), sep2.NotificationStatusChanged)
	}

	return nil
}

// nextEventCreationTime returns the creation instant to stamp on a control
// carrying base, given everything already issued for the device and the
// current wall clock.
//
// It is wallUnix, except when an already-issued control that this one would
// supersede shares or postdates that second, in which case it is one second
// past that control's creationTime.
//
// WHY. creationTime is the ONLY discriminator a client has between two
// overlapping controls of equal primacy (2018 rule f) p.90), and the
// comparison is strictly greater on both sides: the EPRI reference client's
// block_supersede evaluates `x->creationTime > y->creationTime`, so two
// controls stamped in the same wall-clock second compare false in both
// directions and the INCOMING one is discarded. That is the same defect,
// just arriving through the clock instead of through the mRID, and it is
// reachable whenever two GridAPPS-D deltas for one device land inside one
// second, which a fast simulation loop does routinely. TimeType has
// one-second resolution (sep.xsd:6382), so there is no finer stamp available
// to break the tie with.
//
// Only controls this one would actually supersede are consulted. A control on
// an independent mode may share a second freely: rule t) p.91 makes the two
// independent, so no ordering between them is ever evaluated.
//
// The standard does not say what to do when two events would share a
// creationTime; it simply assumes they do not. Advancing the stamp is OUR
// decision, and it is the conservative one, because the alternative is serving
// a control the client is required to ignore. The bump lands on creationTime
// ALONE: interval.start and EventStatus.dateTime stay on the wall clock, so a
// burst of same-second deltas still issues controls that take effect
// immediately, and no window is shifted or shortened.
//
// BOUNDED. The advance is capped at maxCreationTimeLeadSeconds
// ahead of the wall clock and a delta that would exceed it is refused with
// ErrControlDeltaRateUnrepresentable. Without the cap the stamp drifts forward
// without limit under sustained same-mode deltas above one per second, and the
// lead never decays; with it, the lead decays on its own as soon as the delta
// rate falls back under one per second, because the wall clock catches up and
// this function returns it unmodified again.
func nextEventCreationTime(prior []sep2.DERControl, base sep2.DERControlBase, wallUnix int64) (int64, error) {
	incomingModes := controlModesOf(&base)
	next := wallUnix
	for _, p := range prior {
		if p.CreationTime < next {
			continue
		}
		if classifyModes(controlModesOf(p.DERControlBase), incomingModes) == modesIndependent {
			continue
		}
		next = p.CreationTime + 1
	}
	if lead := next - wallUnix; lead > maxCreationTimeLeadSeconds {
		return 0, fmt.Errorf("%w: stamping a control newer than every overlapping same-mode control already issued would put creationTime %ds ahead of the wall clock, over the %ds bound",
			ErrControlDeltaRateUnrepresentable, lead, maxCreationTimeLeadSeconds)
	}
	return next, nil
}

// maxCreationTimeLeadSeconds is how far ahead of the wall clock
// nextEventCreationTime may stamp a control's creationTime.
//
// THIS NUMBER IS OUR DECISION, NOT THE STANDARD'S, in the same way
// eventRetentionSeconds is. sep.xsd:5578 defines creationTime only as "the
// time at which the Event was created" and fixes nothing else about it; no
// clause in 2013, 2018 or 2023 says what a server should do when two events
// would share one, which is why the tie-break exists at all.
//
// WHY A CEILING IS NEEDED. The lead is a falsification of a wire-visible
// timestamp, and left unbounded it grows for as long as the delta rate stays
// above one per second per control mode. Two consequences make that worse than
// cosmetic:
//
//   - It survives nothing. A bridge restart stamps from the true wall clock
//     again, so the first controls issued after a restart carry creationTimes
//     LOWER than the drifted ones a client is still holding. Under rule f)
//     p.90 the client keeps running the stale event and discards the new one,
//     which is exactly the defect the tie-break was added to prevent, arriving
//     by the other door.
//   - It hides the condition. A server quietly stamping events hours ahead is
//     reporting nothing an operator can act on, while the platform's real
//     problem, a delta rate the protocol cannot express, goes unnamed.
//
// WHY TEN SECONDS. It is a burst tolerance, not a rate: eleven changed
// setpoints inside one wall second are absorbed, and any backlog drains as
// soon as the rate falls back under one per second. That is far above the
// cadence this bridge is driven at in practice (Hale's co-simulation run 14:
// fifteen deltas at ten-second spacing), and small enough that a drifted stamp
// cannot survive a restart gap. Above it the request is not a burst but a
// sustained demand for sub-second event ordering, which IEEE 2030.5 has no way
// to express: TimeType is whole seconds (sep.xsd:6382).
//
// The change bound sits in front of this one, so only a delta that
// actually CHANGES the commanded value can consume any of this budget.
const maxCreationTimeLeadSeconds int64 = 10

// restatesControlInForce reports whether base is a byte-for-byte restatement
// of the control this device is already running for base's own control modes.
//
// It is what the change bound adds: an issued DERControl is an Event, and
// re-issuing one that commands what is already commanded is not a change the
// standard has any notion of. 2018 clause 10.2.5.6 p.95 has a client discard a
// duplicate Event outright, and CSIP v2.0 section 4.4.1 lines 282 to 283
// scopes issuing a new DERControl to changing the setting. What a redundant
// re-issue DOES produce is a new mRID and a new response cycle for a setpoint
// the device never stopped running.
//
// WHICH control it compares against is the whole of the logic:
//
//   - Same modes, exactly. classifyModes must report modesIdentical. A control
//     on an independent mode is not a candidate at all (2018 rule t) p.91
//     makes the two independent), and a partial overlap leaves the older
//     control still partly in force, so neither can stand in for the incoming
//     one.
//   - Still in force. A control whose window has closed has already been
//     replaced by the DefaultDERControl on the device, so a delta restating
//     its value is a real command and not a restatement of anything. The test
//     is against minEffectiveScheduledEnd, the EARLIEST instant any device
//     could have finished, because a device that randomized its duration
//     downward is already off the event while one that did not is still on it,
//     and suppressing on the strength of the slowest device would leave the
//     fastest uncommanded.
//   - The newest of them. With several same-mode controls resident, the one
//     the client is executing is the one with the largest creationTime (rule
//     f) p.90). Comparing against an older one would suppress a delta that
//     reverts to a previous setpoint, which is a genuine change.
//
// Equality is decided on the serialized payload (canonicalControlPayload),
// which is both what the client observes and what the event's own identity is
// derived from, so "same payload" here and "same mRID" in deriveEventMRID
// cannot mean two different things.
func restatesControlInForce(prior []sep2.DERControl, base *sep2.DERControlBase, wallUnix int64) (bool, error) {
	incomingModes := controlModesOf(base)

	var inForce *sep2.DERControl
	for i := range prior {
		p := &prior[i]
		// The interval test comes first because it is two integer
		// comparisons, while classifyModes builds a mode set per control.
		if end, ok := minEffectiveScheduledEnd(*p); !ok || wallUnix >= end {
			continue
		}
		if classifyModes(controlModesOf(p.DERControlBase), incomingModes) != modesIdentical {
			continue
		}
		// The mRID breaks a creationTime tie so the choice cannot depend on
		// store iteration order. Two same-mode controls sharing a creationTime
		// are what nextEventCreationTime exists to prevent, so this is a
		// determinism guard rather than a case that should occur.
		if inForce == nil || p.CreationTime > inForce.CreationTime ||
			(p.CreationTime == inForce.CreationTime && p.MRID > inForce.MRID) {
			inForce = p
		}
	}
	if inForce == nil {
		return false, nil
	}

	incoming, err := canonicalControlPayload(base)
	if err != nil {
		return false, err
	}
	existing, err := canonicalControlPayload(inForce.DERControlBase)
	if err != nil {
		return false, err
	}
	return bytes.Equal(incoming, existing), nil
}

// supersedePriorControls marks every already-issued control that the newly
// issued one supersedes, per the edition this server presents.
//
// prior is the snapshot taken BEFORE issued was created, so issued cannot
// classify against itself.
//
// Only EventStatus is written back. Rule c) p.90 permits updating an Event's
// status and nothing else, and rules q)2) p.91 and t)3) p.92 require the
// server to "maintain all Events in their entirety": the control payload, the
// interval, the creationTime and the mRID of a superseded event all survive
// untouched, and the event stays fetchable at its own href for its Effective
// Scheduled Period (rule r) p.91, and 2018 Annex B p.160, which makes
// maintaining a Superseded event for that period a server responsibility).
// Removing it when that period ends is a separate lifecycle concern and is
// deliberately not done here: it lives in lifecycle.go, which
// keys on the interval alone. The two do not interact, because an event
// whose window has closed overlaps nothing and so supersedes nothing, and a
// superseded event that later ends keeps the status 4 it legitimately held.
// TestSupersededControlEndsWithoutLosingItsSupersededStatus states that
// rather than leaving it to be inferred.
//
// A control whose status the edition leaves unchanged is not written back at
// all, which is why markSuperseded and markPotentiallySuperseded report
// whether they changed anything: under 2023 both are no-ops, and rewriting a
// record to store the value it already holds would be a needless edit of a
// served Event.
func supersedePriorControls(ctx context.Context, derControls store.ScopedStore[sep2.DERControl], scope string, prior []sep2.DERControl, issued sep2.DERControl) error {
	// The instant recorded on a superseded event is the superseding event's
	// Effective Start Time, which 2018 Annex B p.160 names explicitly: the
	// server "SHALL mark the event as Superseded at the earliest Effective
	// Start Time of the overlapping event".
	//
	// Refused rather than defaulted when it is unavailable. There is no
	// substitute instant that would be correct, and stamping a wrong one onto
	// a served Event is worse than leaving the supersession unmarked: the
	// client computes supersession from creationTime regardless, so an
	// unmarked event still resolves, while a wrongly stamped one is a
	// falsified server record.
	if issued.Interval == nil {
		return fmt.Errorf("issued control %s has no interval; cannot determine the Effective Start Time to mark predecessors at", issued.MRID)
	}
	at := issued.Interval.Start

	for _, p := range prior {
		if p.MRID == issued.MRID {
			continue
		}

		var changed bool
		switch supersedes(p, issued) {
		case modesIdentical:
			changed = markSuperseded(servedEventEdition, p.EventStatus, at)
		case modesPartial:
			changed = markPotentiallySuperseded(servedEventEdition, p.EventStatus, at)
		case modesIndependent:
			// Rule t) p.91: independent controls overlap without
			// superseding. Nothing to record.
		}
		if !changed {
			continue
		}

		// The id is recomputed from the record rather than parsed out of its
		// href, because derControlID is what produced both and a recomputation
		// cannot drift from a string the way a parse can.
		// TestIssuedDERControlHrefEndsWithItsStoreKey pins the two together.
		if err := derControls.Update(ctx, scope, derControlID(p.CreationTime, p.MRID), p); err != nil {
			return fmt.Errorf("mark control %s superseded: %w", p.MRID, err)
		}
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
// The same lazy-creation moment also seeds this program's
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
// untouched (a deliberate hard rule); the DERProgram (and its
// DefaultDERControl) this function creates is control-flow plumbing
// local to the DOWN path, created lazily on first use rather than at
// bulk seed time.
// edevID addresses the resources (it is the URL index); mridBase is the
// device's canonical LFDI and seeds the wire-level MRID fields. The two are
// deliberately separate arguments: an MRID is an identity value that must not
// become a URL-addressing artifact, and building one from the index would
// make it collide across restarts once indices are reassigned.
func ensureDERProgram(ctx context.Context, stores *assembly.Stores, edevID, mridBase, fsaID, derpID string, policy ControlPolicy) error {
	if _, err := stores.DERPrograms.Get(ctx, edevID, derpID); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("get der program: %w", err)
	}
	return createDERProgram(ctx, stores, edevID, mridBase, fsaID, derpID, policy)
}

// createDERProgram writes one DERProgram and its DefaultDERControl singleton
// unconditionally, with no get-or-create check.
//
// This is the SINGLE construction site for both. seedStores calls it at boot
// so every device has a program before any control arrives (GAGO default
// program), and ensureDERProgram calls it on the lazy path for a device that
// boot seeding did not cover. Both paths must produce byte-identical
// resources: if they drifted, a device's served program would depend on
// whether a control delta happened to arrive first, which is exactly the kind
// of ordering-dependent wire difference no test would catch.
//
// Creating the DefaultDERControl here, rather than only on the lazy path, is
// load-bearing rather than incidental. ensureDERProgram returns early when the
// program already exists, so if boot seeding created the program alone the
// lazy path would then short-circuit and the DefaultDERControl would never be
// created at all. The program and its default control are one unit and are
// written as one.
func createDERProgram(ctx context.Context, stores *assembly.Stores, edevID, mridBase, fsaID, derpID string, policy ControlPolicy) error {
	// Copy, then stamp ONLY the two addressing fields this layer owns. Every
	// other field on the operator's configured DefaultDERControl survives
	// verbatim, including an explicitly configured false: this path fills
	// what is structural and never overrides what was set.
	dderc := policy.DefaultControl.Copy()
	dderc.Href = "/edev/" + edevID + "/fsa/" + fsaID + "/derp/" + derpID + "/dderc"
	dderc.MRID = deriveMRID(mridKindDefaultDERControl, mridBase)

	scope := derControlScope(edevID, fsaID, derpID)
	if err := stores.DefaultDERControls.Create(ctx, scope, singletonKey, dderc); err != nil {
		return fmt.Errorf("create default der control: %w", err)
	}

	// DERProgram is an IdentifiedObject: sep.xsd makes mRID mandatory on it,
	// and the bridge previously served it with no mRID at all. A client that
	// parses the DERProgramList strictly cannot reach the DERControlListLink
	// below without it.
	//
	// Primacy and Description come from operator policy
	// (sep2config.DERProgramPolicy); everything else on the program is
	// derived or structural and is stamped here. Primacy is NOT defaulted
	// locally when the seed is zero: 0 is a meaningful primacy (the highest
	// priority), so a zero value is served as configured rather than
	// silently rewritten to 1.
	program := sep2.DERProgram{
		Primacy:     policy.Program.Primacy,
		Description: policy.Program.Description,
		MRID:        deriveMRID(mridKindDERProgram, mridBase),
	}
	program.Href = "/edev/" + edevID + "/fsa/" + fsaID + "/derp/" + derpID
	program.DERControlListLink = &sep2.ListLink{
		Href: "/edev/" + edevID + "/fsa/" + fsaID + "/derp/" + derpID + "/derc",
	}
	program.DefaultDERControlLink = &sep2.Link{Href: dderc.Href}

	if err := stores.DERPrograms.Create(ctx, edevID, derpID, program); err != nil {
		return fmt.Errorf("create der program: %w", err)
	}
	return nil
}

// activeSignFlip and reactiveSignFlip are the explicit, testable seam
// for the CIM-vs-IEEE-2030.5 sign convention on the real/reactive power
// target mappings below (Vance, power-systems review, PR #9,
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
// co-simulation loopback (Hale runs OpenDSS and asserts the
// inverter actually moves in the commanded direction end to end). Until
// that verification closes, this DOWN path is dev-only and MUST NOT be pointed
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
// returns once DERCapability rtg values are seeded.
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
