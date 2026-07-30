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

// activeControlID is the STORE KEY of the single DERControl slot
// ApplyControlDelta maintains per device. Exactly one control per device
// exists at any instant, so the served DERControlList always has exactly
// one member: two simultaneously-active controls over the same interval is
// a worse failure than a late one, and keeping the store to a single slot
// makes "one active control" a structural property rather than one
// contingent on a client evaluating supersession correctly.
//
// This constant IS on the wire: it is the last path segment of the served
// href (see controlHref), and core mounts a single-resource route at
// GET /edev/{id}/fsa/{fsaId}/derp/{derpId}/derc/{dercId} that resolves a
// DERControl by exactly this store key. Store key and href segment are
// deliberately the same string, because an activated event's href must stay
// fetchable: see controlHref for why a per-generation href was wrong.
//
// The mRID, by contrast, DOES vary per generation, so a client still sees
// each successive command as a new event. See ApplyControlDelta's
// supersession section.
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
// Supersede semantics (GAGO-094, Devi's MEDIUM finding). A second delta
// for the same device REPLACES the device's control with a new event
// identity rather than mutating the existing one in place. Each delta
// produces a fresh generation: a new mRID, a new href, a new creationTime,
// and a fresh interval start, while the DERControlBase carries forward the
// previously-set op-mode fields so a delta on opModTargetVar does not erase
// an earlier opModTargetW (DERControlBase is a bag of independent fields,
// and opModTargetW and opModTargetVar are legitimately active together).
// Exactly one control exists per device at any instant, so the served list
// never contains two overlapping controls.
//
// Why in-place mutation was wrong, measured on the EPRI reference client.
// Rewriting the values under an unchanged mRID produces bytes a client
// parses and stores and then does nothing with, because mRID is the event's
// IDENTITY:
//
//   - schedule_event short-circuits on hash_get(s->blocks, ev->mRID): a
//     known mRID creates no new EventBlock, it only refreshes primacy.
//   - update_existing goes further: for an event whose mRID compares equal
//     it copies ONLY the EventStatus off the incoming object and frees the
//     rest, so a changed opModTargetW is discarded at parse time and never
//     reaches the scheduler at all.
//   - activate_block calls insert_event(eb, EVENT_START, 0), the hook that
//     actually pushes the setpoint to the inverter, only when the block is
//     not already Active.
//
// So a client that had already actuated the first command had no wire
// signal to actuate the second. Observed directly: a run published 6137 W
// while 4291 W was the commanded value, because a stale control was
// indistinguishable from a fresh one.
//
// Why replacement rather than a superseded-marker pair. IEEE 2030.5 does
// define server-marked supersession (EventStatus currentStatus 4), and a
// server MAY keep the superseded event in its collection for the remainder
// of its scheduled period. That is the right shape for a server publishing
// a SCHEDULE of future events, where a client needs to see both the
// replaced and the replacing event to reason about the timeline. This
// bridge publishes a single live setpoint with no schedule: it has exactly
// one control, always already active, always ending in the future. Serving
// a superseded twin would put two overlapping DERControls in the list and
// make correct behavior depend on the client resolving supersession, which
// is a strictly larger failure surface for zero benefit here. Deleting the
// prior control is the same outcome the standard's supersession is meant to
// produce, reached without the overlap; the client's own removal path
// handles it cleanly (dep_complete subtracts the vanished href and fires
// RESOURCE_REMOVE, which frees the old block via delete_blocks).
//
// Why a fresh mRID is NOT sufficient on its own, and creationTime is
// required with it. block_supersede breaks an equal-primacy tie by
// x->creationTime > y->creationTime. Every control this bridge issues has
// primacy 1, so with creationTime absent (or equal) both events parse as
// the same creation instant, the incoming block LOSES, and insert_active
// marks the NEW control Superseded and discards it. A fresh mRID without an
// advancing creationTime would therefore trade a silently-ignored update
// for a silently-rejected one. Each generation stamps CreationTime, and it
// is guaranteed to advance: see nextControlCreationTime.
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

	// edevID must be the device's ADVERTISED store id, which is Entry.LFDI:
	// seed.go keys stores.EndDevices (and every /edev/{id} href) by the
	// canonical LFDI alone, with no separate alias. reg.Get resolves the
	// delta's mRID to its Entry; entry.LFDI is then the same id the
	// device is advertised under.
	entry, ok := reg.Get(delta.Object)
	if !ok {
		return fmt.Errorf("%w: mrid=%q", ErrUnknownControlDevice, delta.Object)
	}
	edevID := entry.LFDI

	// Defense in depth: the registry and stores.EndDevices are seeded
	// together (bridge.bootstrapRegistry + sep2embed.New), but if they
	// were ever to drift, fail closed rather than write a DERControl
	// with no corresponding seeded device.
	if _, err := stores.EndDevices.Get(ctx, edevID); err != nil {
		return fmt.Errorf("%w: edev %q not seeded: %v", ErrUnknownControlDevice, edevID, err)
	}

	if err := ensureDERProgram(ctx, stores, edevID, controlFSAID, controlDERProgramID, defaultControl); err != nil {
		return fmt.Errorf("sep2embed: control delta: ensure der program: %w", err)
	}

	scope := derControlScope(edevID, controlFSAID, controlDERProgramID)
	controlStore := stores.DERControls.ForParent(scope)

	existing, err := controlStore.Get(ctx, activeControlID)
	var base sep2.DERControlBase
	isReplacement := false
	var priorCreationTime int64
	switch {
	case err == nil:
		isReplacement = true
		// Op-mode fields already set by earlier deltas carry forward, so a
		// delta naming one field does not silently clear another.
		if existing.DERControlBase != nil {
			base = existing.DERControlBase.Copy()
		}
		priorCreationTime = existing.CreationTime
	case errors.Is(err, store.ErrNotFound):
		// Fresh control: base starts zero-valued.
	default:
		return fmt.Errorf("sep2embed: control delta: read existing control: %w", err)
	}

	if err := applyDERControlBaseField(&base, field, delta.Value); err != nil {
		return fmt.Errorf("sep2embed: control delta: %w", err)
	}

	now := time.Now().UTC().Unix()
	creationTime := now
	if isReplacement {
		creationTime = nextControlCreationTime(priorCreationTime, now)
	}

	control := sep2.DERControl{}
	control.Href = controlHref(edevID, controlFSAID, controlDERProgramID)

	// CreationTime is what makes this generation WIN the client's
	// equal-primacy supersession comparison against the generation it
	// replaces; see ApplyControlDelta's doc comment and
	// nextControlCreationTime. It is a required wire element regardless.
	control.CreationTime = creationTime

	// The mRID is derived FROM creationTime, so the event identity and the
	// supersession discriminator advance together by construction and cannot
	// disagree. creationTime is strictly increasing across generations
	// (nextControlCreationTime guarantees it even within one wall-clock
	// second), so successive controls always carry distinct mRIDs, which is
	// what stops a client's schedule_event from short-circuiting on an
	// already-hashed identity.
	//
	// It also keeps the stored record self-describing now that the href is
	// stable: the discriminator is read back off the record's own
	// creationTime, with no separate counter in the href and none in process
	// memory that could drift from the store and re-issue a live identity.
	control.MRID = deriveControlMRID(edevID, dercMRIDKind, creationTime)

	control.EventStatus = &sep2.EventStatus{
		CurrentStatus: sep2.EventStatusActive,
		DateTime:      now,
	}

	// DERControl is an Event, and an Event's interval is not decoration:
	// it is the only thing that tells a client's scheduler WHEN the
	// control applies. A conformant scheduler computes the event's window
	// from interval.start and interval.duration and discards a window that
	// has already ended. Measured on the EPRI reference client:
	// new_block sets eb->start = ev->interval.start and
	// eb->end = eb->start + ev->interval.duration (schedule.c), then
	// update_schedule drops any block whose eb->end <= now (schedule.c).
	// With no interval both are zero, zero is always <= now, and the
	// control is discarded before it can ever activate. EventStatus
	// currentStatus=Active does NOT rescue it: the scheduler branches on
	// the window, not on the status flag.
	//
	// start = now on EVERY generation, including a replacement. Each
	// generation is a distinct event with its own identity, so it gets its
	// own window opening at the instant it was issued; there is no earlier
	// window to preserve, because the generation that had one no longer
	// exists. start must not be in the future (a scheduler treats a future
	// start as pending, not current) and must not be stale, both of which
	// "now" satisfies by construction.
	//
	// duration = defaultControlDurationSeconds measured from that start, so
	// the window stays open well past a client's own polling interval; see
	// that constant. Because start is re-stamped per generation, duration is
	// the plain constant: there is no elapsed time to compensate for.
	control.Interval = &sep2.DateTimeInterval{
		Start:    now,
		Duration: defaultControlDurationSeconds,
	}

	control.DERControlBase = &base

	// Retire-then-replace under the single per-device slot. Update rather
	// than Delete+Create so the slot is never momentarily EMPTY: a client
	// polling the list between the two calls would otherwise read all="0"
	// and conclude the bridge had released the device, reverting it to its
	// DefaultDERControl. Update is atomic with respect to a concurrent
	// reader (core's memory.Store takes its write lock for the whole
	// assignment), so a poll either sees the prior generation or the new
	// one and never neither.
	//
	// The retirement is total: the prior generation's mRID and href are
	// gone from the served list, which is exactly the signal a client acts
	// on (its dep_complete subtracts the vanished href and fires
	// RESOURCE_REMOVE, freeing the old event block by mRID). No superseded
	// twin is left behind; see the doc comment for why.
	if isReplacement {
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

// controlHref returns the served href of the device's single DERControl slot
// under (edev, fsa, derp). It is STABLE across generations: every successive
// command is served at the same URL, and only the mRID and creationTime vary.
//
// This is the correction to GAGO-094's second defect, and it is the opposite
// of what this function did first. A per-generation href
// (".../derc/active-<n>") was self-defeating, because an href is not just a
// list-membership key to a client: it is the URL the client POLLS. Traced
// through the EPRI reference client:
//
//   - activate_block (schedule.c) sets event->poll_rate = active_poll_rate
//     and calls poll_resource on the EVENT's own stub, so once a client
//     actuates a control it begins fast-polling that control's own href.
//   - When the next delta arrived, the old generation's URL stopped existing.
//     That armed poll then GET a path the server no longer had.
//   - process_http (retrieve.c) turns any non-200 on a GET into
//     insert_event(s, RETRIEVE_FAIL, 0), and der_client's RETRIEVE_FAIL arm
//     calls remove_stub, freeing the event outright.
//
// So the client tore down the very event it had just actuated, stopped
// polling, and never observed any later generation. Mounting a
// single-resource DERControl route in core does NOT fix that on its own: with
// a varying href the URL is genuinely gone, so the fast poll 404s correctly.
// The href has to stop moving.
//
// Why a stable href still delivers each new setpoint. The earlier rationale
// for varying it (that a client tracks membership by href, so a reused href
// would leave the retired event block un-freed and the client holding two)
// does not survive reading update_existing (retrieve.c):
//
//   - list_object keys each member by href via get_stub(path), then calls
//     update_existing for that member.
//   - update_existing compares mRIDs. For an event whose mRID DIFFERS it
//     calls replace_se_object, swapping the entire stored event for the
//     incoming one: the new opModTargetW is kept, not discarded. (Only the
//     equal-mRID branch copies EventStatus alone, which is the in-place
//     mutation defect this package already fixed by varying the mRID.)
//   - the dep chain then re-runs schedule_der -> schedule_event, whose
//     hash_get(s->blocks, ev->mRID) MISSES on the new mRID, so a fresh
//     EventBlock is created, insert_active runs, block_supersede wins on the
//     advanced creationTime, and activate_block fires EVENT_START, the hook
//     that pushes the setpoint to the inverter.
//
// There is also no un-freed block to worry about: the list has exactly one
// member, so it never shrinks, list_subtract yields nothing, and there is
// nothing for RESOURCE_REMOVE to free. replace_se_object already replaced the
// event in place, and the old mRID's block is superseded by the new one
// through insert_active rather than by list removal.
//
// So a new event identity (mRID) plus an advancing creationTime at a STABLE,
// fetchable href satisfies every mechanism, and is the only combination that
// also keeps the post-activation fast poll resolving.
func controlHref(edevID, fsaID, derpID string) string {
	return "/edev/" + edevID + "/fsa/" + fsaID + "/derp/" + derpID +
		"/derc/" + activeControlID
}

// nextControlCreationTime returns a creationTime for a replacement control
// that is strictly greater than the creationTime it replaces.
//
// Normally that is simply now: deltas arrive seconds or minutes apart and
// the wall clock has advanced. The clamp matters when it has not:
//
//   - Two deltas inside the same wall-clock second (a burst from the
//     platform, entirely normal) would otherwise carry EQUAL creationTimes.
//     A client breaks an equal-primacy tie with a STRICT comparison
//     (x->creationTime > y->creationTime), so equal means the replacement
//     loses and is discarded as Superseded: the new setpoint would be
//     silently dropped. TimeType is second-resolution on the wire, so there
//     is no sub-second value to fall back on.
//   - A host clock that stepped BACKWARD would produce a now that is less
//     than the stored creationTime, with the same losing outcome and for
//     longer.
//
// Advancing to prior+1 in both cases keeps supersession working. The cost is
// a creationTime up to a few seconds ahead of the true instant during a
// burst, which is the right trade: creationTime is only ever compared
// between this server's own successive events, never used as a clock
// reference, whereas a non-advancing value breaks control delivery outright.
func nextControlCreationTime(prior, now int64) int64 {
	if now > prior {
		return now
	}
	return prior + 1
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
// GAGO-050: the same call also seeds this program's DefaultDERControl
// singleton (into stores.DefaultDERControls, keyed by derControlScope +
// singletonKey, mirroring core's own DefaultDERControlHandler parent-key
// derivation) and points the new program's DefaultDERControlLink at it. A
// client that GETs this DERProgram and follows DefaultDERControlLink must
// find a well-formed DefaultDERControl, not an absent one, so the two
// records are created together and never separately. defaultControl is the
// caller-supplied seed value (sourced from SEP2Policy.DefaultControl, never
// hardcoded here); it is written verbatim except for Href/MRID, which this
// function stamps to match the program's own scope.
//
// The normal caller is seed.go's seedDERProgram, at bulk seed time: see
// that function for why creating the program before any control delta
// arrives is a correctness requirement, not a convenience. This function
// remains get-or-create, and ApplyControlDelta still calls it, because the
// DOWN path must not depend on having been seeded by this process: a
// delta for a device whose program is somehow absent creates it rather than
// failing to write the control.
func ensureDERProgram(ctx context.Context, stores *assembly.Stores, edevID, fsaID, derpID string, defaultControl sep2.DefaultDERControl) error {
	inner := stores.DERPrograms.ForParent(edevID)
	if _, err := inner.Get(ctx, derpID); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("get der program: %w", err)
	}

	dderc := defaultControl.Copy()
	dderc.Href = "/edev/" + edevID + "/fsa/" + fsaID + "/derp/" + derpID + "/dderc"
	dderc.MRID = deriveResourceMRID(edevID, ddercMRIDKind)

	scope := derControlScope(edevID, fsaID, derpID)
	if err := stores.DefaultDERControls.Create(ctx, scope, singletonKey, dderc); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return fmt.Errorf("create default der control: %w", err)
	}

	// mRID is a REQUIRED element on DERProgram (and on DefaultDERControl
	// above), so it is set to a real, wire-legal, per-device value rather
	// than left at the zero value. Previously the program carried no mRID
	// at all, which a lenient parser tolerates but a strict one need not.
	program := sep2.DERProgram{
		Primacy: 1,
		MRID:    deriveResourceMRID(edevID, derpMRIDKind),
	}
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

// defaultControlDurationSeconds is how long a DERControl this bridge
// writes stays valid, measured from its interval start.
//
// Sizing: a client rediscovers the control by polling its DERControlList,
// and the reference client's own active-list poll rate is 300 seconds
// (EPRI's active_poll_rate). The window must therefore comfortably exceed
// one poll interval, or a control could expire in the gap between two
// polls and the device would revert to its DefaultDERControl even though
// the bridge is still commanding it. 900 seconds is three of those
// intervals, and also matches the reference client's DEFAULT_POLL_RATE for
// the lists that carry no explicit rate, so a client polling at its own
// slowest default still never sees an expired window.
//
// It is deliberately NOT unbounded. An event with an effectively infinite
// duration is a control that never releases the device if this bridge
// dies: the expiry is the fail-safe that hands the device back to its
// DefaultDERControl. Every delta issues a new generation whose window opens
// at that instant, so a live bridge keeps the device continuously commanded
// and a dead one lets the last generation lapse within 15 minutes.
//
// It is a plain constant rather than a duration extended by the time already
// elapsed. The elapsed-time compensation this used to carry existed because
// interval.start was preserved across in-place updates, which made the
// window's END stationary while the bridge kept commanding; each generation
// now stamps its own start, so there is no accumulated elapsed time to
// offset and no uint32 overflow surface in computing it.
const defaultControlDurationSeconds uint32 = 900

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
