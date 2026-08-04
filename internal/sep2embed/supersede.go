package sep2embed

import (
	"slices"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// Supersession of issued DERControls (GAGO-133).
//
// WHY THIS FILE EXISTS. The bridge used to keep exactly one DERControl per
// device, at a fixed store key, whose mRID was a pure function of (kind,
// LFDI). A second control delta rewrote that record in place. Two clauses
// make that non-conformant and one makes it useless:
//
//   - 2018 rules q)2) p.91 and t)3) p.92: a server "SHALL NOT edit the
//     original Event but SHALL maintain all Events in their entirety."
//     Rewriting a served DERControl's DERControlBase is exactly the edit
//     those sub-rules forbid. (2023 carries the same text at o)2) and q)3)
//     p.102.)
//   - 2018 rule c) p.90: editing Events is not allowed except for updating
//     status; providers "SHALL cancel Events ... and/or provide new
//     superseding Events."
//   - 2018 clause 10.2.5.6 p.95: clients "SHALL detect duplicate Events by
//     comparing the mRIDs of the Events." A constant mRID therefore makes a
//     conformant client discard every update as a duplicate of the event it
//     already holds. That was the observed live behavior: one control served
//     and fetched 22 times, zero re-actuations.
//
// The remedy CSIP names is a new event, not a mutation: CSIP v2.0 section
// 4.4.1 lines 282 to 283, "To change the DERControl setting, a new DERControl
// is issued to supersede or cancel the existing DERControl." CSIP adds no
// server-side supersession procedure of its own; requirement P30 (CSIP v2.0
// printed p.55) delegates to the IEEE 2030.5 event rules.
//
// The shape implemented here is CTP BASIC-019's (CTP v1.2 printed p.119):
// two distinct DERControl instances inside ONE DERProgram, each with its own
// identity and its own response cycle.

// eventEdition names the IEEE 2030.5 edition whose EventStatus semantics the
// server presents on the wire.
//
// It exists because the correct status of a superseded event is NOT the same
// across editions, and the difference is not a detail:
//
//   - 2013 and 2018 (2018 Annex B printed p.160): value 4 Superseded, which
//     the server SHALL set on an event replaced by a newer event "from the
//     same program that target the exact same set of deviceCategory's (if
//     applicable) AND DERControl controls (e.g., opModTargetW) (if
//     applicable) and overlap for a given period of time." Our 5000 W to
//     7500 W case is precisely that.
//   - 2023 (Annex B printed p.169): value 4 is DEPRECATED and "SHALL NOT be
//     used by servers"; value 1 Active is reworded to mean currently active
//     "even if the event is known to be overlapped". Under 2023 supersession
//     is not a server-represented state at all: it is wholly a client
//     computation from primacy plus creationTime.
//
// potentiallySuperseded splits the same way. Under 2018 it is the PARTIAL
// supersession flag (rules q)3) p.91 and t)4) p.92): true only when a newer
// overlapping event covers SOME BUT NOT ALL of this event's controls. Under
// 2023 (printed pp.169 to 170) it is deprecated to an unconditional true and
// potentiallySupersededTime "SHALL NOT be included by servers".
//
// Making the edition an explicit parameter rather than an implicit 2018
// assumption is the point. Every branch below is written and tested; only the
// selection is pinned.
type eventEdition int

const (
	// edition2018 covers IEEE 2030.5-2013 and -2018, which are identical on
	// this surface (2013 EventStatus printed pp.158 to 159 defines value 4
	// with a broader scope; 2018 p.160 tightens the scope to same-program,
	// exact-same-control-set, which is the scope implemented here).
	edition2018 eventEdition = iota

	// edition2023 covers IEEE 2030.5-2023.
	edition2023
)

// servedEventEdition is the edition this bridge presents today.
//
// 2018 is pinned because it is the edition the EPRI reference client this
// bridge is verified against implements, and because the workspace targets
// all editions with 2018 primary. This is the ONLY place the choice is made:
// every behavioral difference flows from the eventEdition argument threaded
// through the functions below, so a future 2023 mode is a matter of sourcing
// this value from operator configuration (sep2config.SEP2Policy) and plumbing
// it to ApplyControlDelta, not of finding and rewriting scattered 2018
// assumptions. No such configuration seam exists today and none is invented
// here: see GAGO-133's report for what a 2023 mode would additionally need
// beyond this constant (chiefly the ActiveDERControlListLink deprecation and
// the EventStatus 5 Completed lifecycle, which belongs to GAGO-134).
const servedEventEdition = edition2018

// derControlModeName is one entry in the table of DERControlBase control
// modes: the wire element name, and a test for whether a given base carries
// it.
type derControlModeName struct {
	name  string
	isSet func(*sep2.DERControlBase) bool
}

// derControlModes enumerates every DERControlBase control mode, in schema
// order.
//
// The table is the operative definition of "the DERControl controls (e.g.,
// opModTargetW)" that 2018 Annex B p.160 scopes supersession to, and of the
// "differing controls ... are independent and are allowed to overlap or nest
// without superseding" of 2018 rule t) p.91.
//
// Every opMod* field is listed, not only the four this bridge's DOWN path can
// currently write. A control mode that a future delta learns to set would
// otherwise be invisible to the classifier, and an invisible mode reads as
// "no controls in common", which silently turns a superseding event into an
// independent one and leaves the device running two conflicting setpoints.
// TestDERControlModeTableCoversEveryOpModField enforces the coverage by
// reflection so a core field addition fails a test rather than degrading
// behavior.
//
// rampTms is deliberately EXCLUDED. It is a ramp-rate modifier applied to
// whatever mode is present, not a control mode of its own, so two events that
// differ only in rampTms are not "differing controls" in rule t)'s sense.
var derControlModes = []derControlModeName{
	{"opModConnect", func(b *sep2.DERControlBase) bool { return b.OpModConnect != nil }},
	{"opModEnergize", func(b *sep2.DERControlBase) bool { return b.OpModEnergize != nil }},
	{"opModFixedPFAbsorbW", func(b *sep2.DERControlBase) bool { return b.OpModFixedPFAbsorbW != nil }},
	{"opModFixedPFInjectW", func(b *sep2.DERControlBase) bool { return b.OpModFixedPFInjectW != nil }},
	{"opModFixedVar", func(b *sep2.DERControlBase) bool { return b.OpModFixedVar != nil }},
	{"opModFixedW", func(b *sep2.DERControlBase) bool { return b.OpModFixedW != nil }},
	{"opModFreqDroop", func(b *sep2.DERControlBase) bool { return b.OpModFreqDroop != nil }},
	{"opModFreqWatt", func(b *sep2.DERControlBase) bool { return b.OpModFreqWatt != nil }},
	{"opModHFRTMustTrip", func(b *sep2.DERControlBase) bool { return b.OpModHFRTMustTrip != nil }},
	{"opModHVRTMomentaryCessation", func(b *sep2.DERControlBase) bool { return b.OpModHVRTMomentaryCessation != nil }},
	{"opModHVRTMustTrip", func(b *sep2.DERControlBase) bool { return b.OpModHVRTMustTrip != nil }},
	{"opModLFRTMustTrip", func(b *sep2.DERControlBase) bool { return b.OpModLFRTMustTrip != nil }},
	{"opModLVRTMomentaryCessation", func(b *sep2.DERControlBase) bool { return b.OpModLVRTMomentaryCessation != nil }},
	{"opModLVRTMustTrip", func(b *sep2.DERControlBase) bool { return b.OpModLVRTMustTrip != nil }},
	{"opModMaxLimW", func(b *sep2.DERControlBase) bool { return b.OpModMaxLimW != nil }},
	{"opModTargetVar", func(b *sep2.DERControlBase) bool { return b.OpModTargetVar != nil }},
	{"opModTargetW", func(b *sep2.DERControlBase) bool { return b.OpModTargetW != nil }},
	{"opModVoltVar", func(b *sep2.DERControlBase) bool { return b.OpModVoltVar != nil }},
	{"opModVoltWatt", func(b *sep2.DERControlBase) bool { return b.OpModVoltWatt != nil }},
}

// controlModesOf returns the names of the control modes a DERControlBase
// carries, in the table's (schema) order. A nil base carries none.
func controlModesOf(base *sep2.DERControlBase) []string {
	if base == nil {
		return nil
	}
	modes := make([]string, 0, len(derControlModes))
	for _, m := range derControlModes {
		if m.isSet(base) {
			modes = append(modes, m.name)
		}
	}
	return modes
}

// modeRelation classifies how one event's control-mode set relates to
// another's. The three cases are the three distinct spec outcomes, not an
// arbitrary partition.
type modeRelation int

const (
	// modesIndependent: no control mode in common. 2018 rule t) p.91,
	// "differing controls ... within DERControl Events are independent and
	// are allowed to overlap or nest without superseding". Nothing happens to
	// the earlier event; both run. CTP BASIC-024 through BASIC-026 (CTP v1.2
	// printed pp.130 to 138) require exactly this: the client executes BOTH.
	modesIndependent modeRelation = iota

	// modesIdentical: the exact same set of control modes. This is the scope
	// 2018 Annex B p.160 attaches to EventStatus 4 Superseded, "the exact
	// same set of deviceCategory's (if applicable) AND DERControl controls".
	modesIdentical

	// modesPartial: some modes in common, but not all. 2018 rules q)3) p.91
	// and t)4) p.92 route this to the potentiallySuperseded flag rather than
	// to status 4, because the earlier event is still partly in force.
	modesPartial
)

// classifyModes reports how incoming relates to existing.
//
// Two empty sets classify as independent rather than identical: an event that
// commands no control mode supersedes nothing, and treating "nothing in
// common" as "the same nothing" would mark such an event superseded on the
// strength of an empty intersection.
func classifyModes(existing, incoming []string) modeRelation {
	shared := 0
	for _, m := range existing {
		if slices.Contains(incoming, m) {
			shared++
		}
	}
	if shared == 0 {
		return modesIndependent
	}
	if shared == len(existing) && shared == len(incoming) {
		return modesIdentical
	}
	return modesPartial
}

// intervalsOverlap reports whether two DateTimeIntervals cover any instant in
// common, treating each as the half-open window [start, start+duration).
//
// A nil interval yields false. That is a deliberate refusal rather than a
// permissive default: supersession is the one edit rule c) p.90 permits on a
// served Event, and an event whose temporal extent cannot be determined is
// not one this server should be editing. Every control this bridge writes
// carries an interval (it is minOccurs=1 on Event, sep.xsd:5584), so a nil
// here means a record no path in this package produced.
func intervalsOverlap(a, b *sep2.DateTimeInterval) bool {
	if a == nil || b == nil {
		return false
	}
	aEnd := a.Start + int64(a.Duration)
	bEnd := b.Start + int64(b.Duration)
	return a.Start < bEnd && b.Start < aEnd
}

// supersedes reports how the incoming event acts on the existing one.
//
// The full precedence chain in the standard is primacy first (2018 clause
// 10.2.5.6 rule b) p.95), then creationTime at equal primacy (rule f) p.90,
// "the Event with the larger creationTime is newer"). Primacy does not appear
// here because both events are always in the SAME DERProgram: this bridge
// maintains exactly one DERProgram per device (see controlDERProgramID), so
// primacy is equal by construction and rule f) is the whole test.
//
// The comparison on creationTime is strictly greater, matching the client
// side it exists to drive: the EPRI reference client's block_supersede
// compares `x->creationTime > y->creationTime`, so two events sharing a
// creationTime compare false in both directions and neither can win. That is
// why the write path guarantees a strictly increasing creationTime per device
// rather than relying on the wall clock (see nextEventCreationTime).
func supersedes(existing, incoming sep2.DERControl) modeRelation {
	if incoming.CreationTime <= existing.CreationTime {
		return modesIndependent
	}
	if !intervalsOverlap(existing.Interval, incoming.Interval) {
		return modesIndependent
	}
	return classifyModes(controlModesOf(existing.DERControlBase), controlModesOf(incoming.DERControlBase))
}

// newEventStatus builds the EventStatus stamped on a freshly issued
// DERControl, per edition, for an event starting at startUnix and issued at
// wall-clock instant wallUnix.
//
// currentStatus is EVALUATED against the two instants rather than assumed
// (GAGO-137). sep.xsd:5603 fixes both directions:
//
//   - start at or before now: "this status SHALL never be indicated, the event
//     SHALL start with a status of Active". A server that stamped 0 Scheduled
//     on an event already in force would violate that SHALL outright.
//   - start after now: the event "has been scheduled and ... has not yet
//     started", which is what value 0 means. Stamping 1 Active on it tells
//     every client the event is running when it is not, and a client acting on
//     it actuates early.
//
// The write path in control.go stamps interval.start from the wall clock, so
// the Scheduled branch is not reachable from today's ApplyControlDelta. It is
// written and tested anyway, because the alternative is a hardcoded Active
// whose correctness rests on a property of a DIFFERENT file. That is precisely
// how the defect arrived: the tie-break moved interval.start forward without
// the status following, and a hardcoded Active does not fail when its
// precondition stops holding. Note that reaching the Scheduled branch would
// bring a second duty with it, since sep.xsd:5606 requires the server to move
// the event to Active "when the event reaches its earliest Effective Start
// Time" and nothing in this package does that today.
//
// dateTime is the WALL clock, never a bumped or otherwise adjusted stamp.
// sep.xsd:5623: it "MUST be set to the time at which the status change
// occurred, not a time in the future or past".
//
// potentiallySuperseded is where the editions part. Under 2013 and 2018 it
// carries PARTIAL supersession and a fresh event has nothing to be partly
// superseded by, so false is the correct wire value (2018 Annex B p.160).
// Under 2023 it is deprecated to a constant: "DEPRECATED. SHALL be set to
// true" (2023 Annex B p.169), so the element carries no information and is
// served true unconditionally. potentiallySupersededTime is absent in both:
// under 2018 because nothing has set the flag, under 2023 because it "SHALL
// NOT be included by servers" (2023 printed p.170).
func newEventStatus(ed eventEdition, startUnix, wallUnix int64) *sep2.EventStatus {
	currentStatus := sep2.EventStatusActive
	if startUnix > wallUnix {
		currentStatus = sep2.EventStatusScheduled
	}
	return &sep2.EventStatus{
		CurrentStatus:         currentStatus,
		DateTime:              wallUnix,
		PotentiallySuperseded: ed == edition2023,
	}
}

// markSuperseded records full supersession on an already-served event's
// status, per edition. It reports whether it changed anything.
//
// 2018 (and 2013): set currentStatus to 4 Superseded. Annex B p.160 makes
// this a server duty and fixes the instant: the server SHALL mark the event
// Superseded "at the earliest Effective Start Time of the overlapping event".
// EARLIEST is why an event already at status 4 is left alone rather than
// restamped: a third, later control must not push the recorded instant
// forward past the first superseding control's start.
//
// 2023: nothing at all. Value 4 "SHALL NOT be used by servers" and value 1 is
// reworded to mean currently active "even if the event is known to be
// overlapped" (2023 Annex B p.169), so a superseded event under 2023 is
// indistinguishable, by design, from any other active event. Supersession
// becomes wholly the client's computation, which is what the CTP already
// tests (the observable in every CTP supersession procedure is the client's
// Response status 7 POST, never a server status field).
//
// Editing the status is the one modification rule c) p.90 permits on a served
// Event ("SHALL NOT be allowed except for updating status"). Nothing else on
// the event is touched, which is what rules q)2) p.91 and t)3) p.92 require.
func markSuperseded(ed eventEdition, es *sep2.EventStatus, atUnix int64) bool {
	if ed == edition2023 || es == nil {
		return false
	}
	if es.CurrentStatus == sep2.EventStatusSuperseded {
		return false
	}
	es.CurrentStatus = sep2.EventStatusSuperseded
	es.DateTime = atUnix
	return true
}

// markPotentiallySuperseded records PARTIAL supersession on an already-served
// event's status, per edition. It reports whether it changed anything.
//
// 2018 (and 2013): 2018 rule q)3) p.91 and rule t)4) p.92 both say a server
// "SHALL set the potentiallySuperseded flag when the Event is superseded for
// any of the device categories [rule t)4): controls] and update the
// potentiallySupersededTime". The flag, not status 4, is the correct signal
// here because the older event remains partly in force: 2018 Annex B p.160
// scopes the flag to events overlapped in "SOME, BUT NOT ALL" controls.
// currentStatus is left at Active for exactly that reason.
//
// Like markSuperseded this is set-once. potentiallySupersededTime "indicates
// the time that the potentiallySuperseded flag was set" (2018 p.161), so
// restamping it on a later partial overlap would report the wrong instant.
//
// 2023: nothing. The flag is already unconditionally true on every event and
// potentiallySupersededTime "SHALL NOT be included by servers" (2023 printed
// pp.169 to 170), so there is no state left to record.
func markPotentiallySuperseded(ed eventEdition, es *sep2.EventStatus, atUnix int64) bool {
	if ed == edition2023 || es == nil {
		return false
	}
	if es.PotentiallySuperseded {
		return false
	}
	es.PotentiallySuperseded = true
	at := atUnix
	es.PotentiallySupersededTime = &at
	return true
}
