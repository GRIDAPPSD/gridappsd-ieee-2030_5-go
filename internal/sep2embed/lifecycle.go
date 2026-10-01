package sep2embed

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/assembly"
	coresub "github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/handlers/subscription"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/store"
)

// End of life for an issued DERControl.
//
// WHY THIS FILE EXISTS. Every control delta issues its own
// DERControl and no previously issued control is ever rewritten. Nothing then
// took any of them out of service, so a device's DERControlList grew for the
// life of the process and every entry in it kept serving currentStatus 1
// (Active) with an interval that had already elapsed. Observed live on
// 2026-08-03: after the 60-second window closed, GET on the list still
// returned the event marked Active with its now-past interval.
//
// A client that was present when the event started is unharmed: it computes
// the end from interval.start plus duration and reverts on its own. The
// exposure is a client that connects LATER. It fetches an event marked Active
// whose window has already passed, is REQUIRED to discard it (2018 rule l)
// p.90: ignore an Event whose Specified End Time is in the past) and, if
// responseRequired indicates, POSTs status 254 "Rejected: event was received
// after it expired" (Table 27 p.76). The client is conformant; the server is
// presenting as Active something the client is obliged to throw away.
//
// WHAT THE STANDARD REQUIRES, PER EDITION. The obligation is not the same
// across editions, and the difference is the whole reason the behavior below
// is what it is:
//
//   - 2013 and 2018 require NEITHER removal nor a status change. The only
//     clause in play is 2018 rule r) p.91 (2013 rule 18 p.97), "Servers SHOULD
//     maintain and serve Events for the maximum Effective Scheduled Period",
//     which is a SHOULD and is a CEILING as much as a floor: the retention
//     duty ENDS at that period. The 2018 EventStatus enumeration (Annex B
//     p.160; normative at sep.xsd:5598 to 5619) has no value meaning "ended":
//     0 Scheduled, 1 Active, 2 Cancelled, 3 Cancelled with Randomization,
//     4 Superseded, all other values reserved. So serving an expired event
//     violates no 2018 SHALL, and there is no terminal status to move it to.
//   - 2023 requires BOTH. Clause 10.2.2.3 rule p) p.102 scopes the duty to
//     "SCHEDULED AND ACTIVE Events", so a completed event carries no serving
//     obligation at all; and EventStatus 5 Completed (Annex B p.169) says the
//     server "SHALL set the event to this status after the event's maximum
//     Effective Scheduled Period if the event has not been cancelled and is
//     still present on the server". Serving currentStatus 1 with a past
//     interval is a direct 2023 SHALL violation.
//
// WHY REMOVAL, AND WHY THE SAME REMOVAL IN EVERY EDITION. Removal at the end
// of the maximum Effective Scheduled Period is the ONLY single behavior that
// is conformant under all three editions. It satisfies 2023 (which permits
// either removal or status 5); it is permitted under 2013 and 2018 because
// rule r) is a SHOULD bounded at exactly that instant, and 2018 rule s) p.91
// protects the client from misreading it ("When an Event is removed from the
// server ... clients SHALL NOT assume the Event has been cancelled"); and
// 2023 rule p) p.102 only reads removal BEFORE the interval end as a
// cancellation signal, so removal after it is safe in every edition. CSIP
// illustrates the same thing: v2.0 Figure 34 printed p.51 shows the server
// doing "DERC A Completed / Delete from DERC List" at step 5, and section
// 5.3.3 line 712 describes the DERControlList as hosting "the scheduled and
// active DERControl events".
//
// The alternative, keeping the event and restating its status, forces an
// edition branch whose 2018 leg has no defined value to write. That is why it
// is not taken here.
//
// Independently: an ended event does not belong in an "active" list whatever
// its status. 2018 Annex A A.4.10.12 p.151 defines the ActiveDERControlList
// as "List of DERControls that are currently ACTIVE".

// eventRetentionSeconds is how long the server keeps an ended event's
// identity resolvable after the event itself has left the served collection.
//
// THIS NUMBER IS OUR DECISION, NOT THE STANDARD'S. Devi's consultation
// establishes a confirmed silence: no clause in 2013, 2018 or 2023 ties the
// server's acceptance of a Response to whether the event it names is still
// retrievable. Searched: clause 8.8 Response function set (2018 pp.73 to 77),
// clause 8.10 (2023 pp.83 to 88), Table 27 (2018 pp.75 to 76), Table 31 (2023
// pp.84 to 86), and the Response object definition in both Annex Bs. The only
// storage obligation runs the other way (2018 clause 8.8.3.2 p.77: the SERVER
// is expected to provide a mechanism for the service provider to obtain the
// responses, "even in the presence of outages").
//
// WHY A WINDOW IS NEEDED AT ALL. Response.subject "is populated with the mRID
// of the original object", which makes the mRID the sole correlation key
// between a client's Response and the control it reports on. 2018 Table 27
// p.75 places status 3 (Event completed) at EffectiveEndTime. So the
// completion POST arrives at, or just after, the very instant removal
// happens: dropping the mRID together with the resource RACES that POST and
// would leave the server unable to attribute a conformant client's
// EventCompleted. The resource stops being served on time; its identity
// outlives it.
//
// WHY THIS PARTICULAR NUMBER. Twice the poll rate advertised on the
// collection the event was served in. It is the only numeric retention figure
// anywhere in the family: 2023 rule p) p.102 says servers "SHOULD remove
// cancelled Events from the server only after a time of twice the poll rate
// for the EventStatus". That figure governs CANCELLED events, so borrowing it
// for a completed one is an analogy we are choosing, not a rule we are
// following. It is the right order of magnitude for the thing it has to
// cover: a client that polls at the advertised rate has had two full poll
// cycles to observe the end of the event and post its response.
const eventRetentionSeconds int64 = 2 * derControlListPollRate

// derControlListPollRate is the pollRate, in seconds, core advertises on the
// DERControlList this bridge's controls are served in (pkg/sep2srv/assembly,
// the scopedListHandlerDeep mount for
// "GET /edev/{id}/fsa/{fsaId}/derp/{derpId}/derc").
//
// It is duplicated here rather than imported because core does not export it.
// TestDERControlListAdvertisesTheAssumedPollRate reads it back off the served
// bytes, so a change on core's side fails a test here instead of silently
// resizing the retention window this constant feeds.
const derControlListPollRate int64 = 900

// maxEffectiveScheduledEnd returns the instant at which the control's MAXIMUM
// Effective Scheduled Period closes, and whether it could be determined.
//
// "Maximum" is load-bearing. A RandomizableEvent's actual effective window
// varies per device: randomizeStart shifts the start and randomizeDuration
// shifts the end, each by up to the configured magnitude in either direction.
// The instant after which NO device can still be executing the event is
// therefore the nominal end plus the POSITIVE part of each randomization.
// Negative randomization is deliberately ignored: it can only make a device
// finish EARLIER, and sizing the window on the earliest possible finish would
// take an event out of service while a device that randomized the other way
// is still running it.
//
// A control with no interval yields false and is never removed. That is a
// refusal rather than a permissive default, and the direction matters more
// here than anywhere else in this package: under 2023 rule p) p.102 a client
// "SHALL consider Events removed from the server before the end of their
// Effective Scheduled Period as cancelled". Guessing a window for an event
// whose extent we cannot determine risks removing it early, which is not a
// silent no-op but an affirmative cancellation signal on the wire. Every
// control this bridge writes carries an interval (minOccurs=1 on Event,
// sep.xsd:5584), so a false here means a record no path in this package
// produced.
func maxEffectiveScheduledEnd(c sep2.DERControl) (int64, bool) {
	if c.Interval == nil {
		return 0, false
	}
	end := c.Interval.Start + int64(c.Interval.Duration)
	if c.RandomizeStart != nil && *c.RandomizeStart > 0 {
		end += int64(*c.RandomizeStart)
	}
	if c.RandomizeDuration != nil && *c.RandomizeDuration > 0 {
		end += int64(*c.RandomizeDuration)
	}
	return end, true
}

// minEffectiveScheduledEnd returns the EARLIEST instant at which a device
// executing the control could already have finished with it, and whether it
// could be determined. It is maxEffectiveScheduledEnd's mirror and the two are
// deliberately not one function with a flag: they answer opposite questions
// and each is safe only for its own caller.
//
// "Minimum" is load-bearing in the same way "maximum" is over there.
// randomizeStart and randomizeDuration each shift a device's actual window by
// up to the configured magnitude in EITHER direction, so the first instant at
// which SOME device may already have reverted is the nominal end plus the
// NEGATIVE part of each. Positive randomization is ignored here for the
// reason negative randomization is ignored there: it can only make a device
// finish later, and sizing on the latest possible finish would report a
// control as still in force while a device that randomized the other way has
// already dropped it.
//
// The caller is the change bound, which suppresses a delta only while
// the control it restates is certainly still running on every device. Taking
// the earliest end is what makes "certainly" true: between the earliest and
// the latest possible finish the fleet is split, and a restatement in that
// window is a real command to the devices that have already reverted.
//
// A control with no interval yields false, and the change bound then treats it
// as not in force, so a delta is issued rather than suppressed. That is the
// safe direction here: an extra event is a client re-actuating a setpoint it
// is already running, while a wrongly suppressed one is a device left
// uncommanded. (maxEffectiveScheduledEnd's false is safe in ITS direction for
// the opposite reason, which is why the two are separate.)
func minEffectiveScheduledEnd(c sep2.DERControl) (int64, bool) {
	if c.Interval == nil {
		return 0, false
	}
	end := c.Interval.Start + int64(c.Interval.Duration)
	if c.RandomizeStart != nil && *c.RandomizeStart < 0 {
		end += int64(*c.RandomizeStart)
	}
	if c.RandomizeDuration != nil && *c.RandomizeDuration < 0 {
		end += int64(*c.RandomizeDuration)
	}
	return end, true
}

// markEnded records the terminal status of an event whose maximum Effective
// Scheduled Period has closed, per edition. It reports whether it changed
// anything.
//
// 2023: set currentStatus to 5 Completed and stamp the instant. Annex B p.169
// makes it a server duty: "The server SHALL set the event to this status
// after the event's maximum Effective Scheduled Period if the event has not
// been cancelled and is still present on the server." The cancellation
// carve-out is honored below rather than assumed away: a cancelled event
// keeps its cancellation, because 2 and 3 report WHY the event stopped and
// overwriting them with 5 would claim it ran to completion.
//
// 2013 and 2018: nothing. The enumeration has no value meaning ended (Annex B
// p.160; sep.xsd:5598 to 5619, "All other values reserved"), and inventing
// one, or borrowing 2 Cancelled, would put a reserved or a false value on a
// record. A normally ended event under these editions simply carries the last
// status it legitimately held, which is 1 Active, or 4 Superseded if a later
// control replaced it.
//
// This is the transition CTP CORE-022 (CTP v1.2 pp.67 to 69, required for the
// Server profile) describes: "[S] Update the DERControl#N currentStatus
// /dateTime values at each state of the DER event by following its event
// schedule." It is applied to the record that outlives the resource, because
// under the removal policy above the event is out of service at the same
// instant, and 2023's own SHALL is conditioned on the event "still being
// present on the server". A record whose final status still said Active for
// an event that completed would be a falsified audit trail, which is the one
// thing the retained record exists to avoid.
func markEnded(ed eventEdition, es *sep2.EventStatus, atUnix int64) bool {
	if ed != edition2023 || es == nil {
		return false
	}
	switch es.CurrentStatus {
	case sep2.EventStatusComplete, sep2.EventStatusCancelled, eventStatusCancelledWithRandomization:
		return false
	}
	es.CurrentStatus = sep2.EventStatusComplete
	es.DateTime = atUnix
	return true
}

// eventStatusCancelledWithRandomization is EventStatus value 3 (2018 Annex B
// p.160, 2023 p.169). Core names 0, 1, 2, 4 and 5 but not this one, so it is
// spelled out here rather than written as a bare 3 at the comparison site.
const eventStatusCancelledWithRandomization uint8 = 3

// EndedControl is the record the server keeps of one DERControl after that
// control has been removed from service.
//
// It exists so that the event's mRID outlives the resource. Response.subject
// carries the mRID of the object the response reports on, and 2018 Table 27
// p.75 places the status 3 (Event completed) POST at EffectiveEndTime, so a
// server that forgot the mRID at removal time could not attribute a
// conformant client's EventCompleted. See eventRetentionSeconds for how long
// the record is kept and why that number is ours rather than the standard's.
//
// CurrentStatus and DateTime are the event's TERMINAL status, after markEnded
// has run: 5 Completed under 2023, and under 2013/2018 whatever the event
// legitimately last held (1 Active, or 4 Superseded).
type EndedControl struct {
	// MRID is the event's own mRID, the correlation key a Response names.
	MRID string

	// Href is where the event was served before removal. Retained for the
	// audit trail: a Response's subject names the mRID, but an operator
	// reading back a response stream wants the URI the client was fetching.
	Href string

	// CurrentStatus and DateTime are the terminal EventStatus.
	CurrentStatus uint8
	DateTime      int64

	// EndedAt is the close of the event's maximum Effective Scheduled
	// Period: the instant it stopped being served.
	EndedAt int64

	// RetainedUntil is EndedAt plus eventRetentionSeconds. The record is
	// dropped on the first sweep at or after this instant.
	RetainedUntil int64
}

// endedControlLedger holds EndedControl records, keyed by mRID, for the
// retention window.
//
// It is guarded by its own mutex rather than by the caller because its
// writers and its readers are different goroutines by construction: the sweep
// runs on Embed.Run's own timer while a Response arrives on an HTTP handler
// goroutine.
//
// The zero value is not usable; construct with newEndedControlLedger.
type endedControlLedger struct {
	mu     sync.Mutex
	byMRID map[string]EndedControl
}

func newEndedControlLedger() *endedControlLedger {
	return &endedControlLedger{byMRID: make(map[string]EndedControl)}
}

// record stores rec, keyed by its mRID.
//
// An mRID already present is left alone. The mRID is a function of the
// event's content and creation instant (deriveEventMRID), so a second record
// under the same key IS the same event, and the first record carries the
// earlier, correct EndedAt.
func (l *endedControlLedger) record(rec EndedControl) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.byMRID[rec.MRID]; ok {
		return
	}
	l.byMRID[rec.MRID] = rec
}

// lookup returns the retained record for mrid, if the server still holds one.
func (l *endedControlLedger) lookup(mrid string) (EndedControl, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.byMRID[mrid]
	return rec, ok
}

// purge drops every record whose retention window has closed at nowUnix, and
// returns how many it dropped.
//
// Without it the ledger would be the leak the removal it supports exists to
// stop, one map entry per issued event for the life of the process.
func (l *endedControlLedger) purge(nowUnix int64) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	dropped := 0
	for mrid, rec := range l.byMRID {
		if nowUnix >= rec.RetainedUntil {
			delete(l.byMRID, mrid)
			dropped++
		}
	}
	return dropped
}

// len reports how many records the ledger currently holds.
func (l *endedControlLedger) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.byMRID)
}

// expireDeviceControls removes every DERControl under one device's control
// store whose maximum Effective Scheduled Period closed at or before nowUnix,
// recording each into ledger first. It returns how many it removed.
//
// ORDER. The terminal status is stamped, the record is handed to the ledger,
// and only then is the resource deleted. Deleting first would leave a window
// in which the event is neither served nor resolvable, which is exactly the
// race the ledger exists to close.
//
// The stamp lands on the LIST COPY, never on the served record. Core's
// memory store returns deep copies from List (each item goes through
// DERControl.Copy, which copies EventStatus rather than aliasing it), so
// markEnded here cannot edit a served Event: the value it writes reaches only
// the retained record, and nothing is written back through Update. That is
// deliberate, not incidental. Under 2023 the SHALL that would justify writing
// status 5 back is itself conditioned on the event "still being present on
// the server" (Annex B p.169), and at this point it is about not to be.
//
// markEnded's report of whether it changed anything is not consulted, because
// there is no write-back decision to make: the resource is deleted either
// way, and the only consumer of the value is the record handed to the ledger.
//
// A control whose temporal extent cannot be determined is left in place; see
// maxEffectiveScheduledEnd for why that direction is the safe one.
func expireDeviceControls(ctx context.Context, derControls store.ScopedStore[sep2.DERControl], scope string, ledger *endedControlLedger, ed eventEdition, nowUnix int64) (int, error) {
	// Unbounded for the reason ApplyControlDelta's own read is unbounded: a
	// page here would hide events from the sweep, and an event that is not
	// examined is an event that keeps serving a past interval forever.
	list, err := derControls.List(ctx, scope, store.ListOptions{Unbounded: true})
	if err != nil {
		return 0, fmt.Errorf("list controls: %w", err)
	}

	removed := 0
	for _, c := range list.Items {
		end, ok := maxEffectiveScheduledEnd(c)
		if !ok || nowUnix < end {
			continue
		}

		markEnded(ed, c.EventStatus, end)

		rec := EndedControl{
			MRID:          c.MRID,
			Href:          c.Href,
			EndedAt:       end,
			RetainedUntil: end + eventRetentionSeconds,
		}
		if c.EventStatus != nil {
			rec.CurrentStatus = c.EventStatus.CurrentStatus
			rec.DateTime = c.EventStatus.DateTime
		}
		ledger.record(rec)

		// The key is recomputed from the record rather than parsed out of
		// its href, for the reason given on supersedePriorControls:
		// derControlID produced both, and a recomputation cannot drift from
		// a string the way a parse can.
		if err := derControls.Delete(ctx, scope, derControlID(c.CreationTime, c.MRID)); err != nil {
			return removed, fmt.Errorf("remove ended control %s: %w", c.MRID, err)
		}
		removed++
	}
	return removed, nil
}

// expireEndedControls sweeps every seeded device, removing controls whose
// maximum Effective Scheduled Period has closed and purging ledger records
// whose retention window has closed. It returns how many controls it removed.
//
// A device whose control collection actually changed gets a Changed
// notification on its DERProgramList, the same target and the same status
// ApplyControlDelta notifies on. Without it a subscribed client would keep
// holding an event the server no longer serves until its next poll, which
// defeats the point of subscribing.
//
// The sweep continues past a device it cannot process rather than abandoning
// the fleet: one device's malformed record must not leave every other
// device's ended events in service. The errors are joined into the returned
// error so nothing is swallowed.
func expireEndedControls(ctx context.Context, stores *assembly.Stores, notifier *coresub.Manager, ledger *endedControlLedger, ed eventEdition, nowUnix int64) (int, error) {
	devices, err := stores.EndDevices.List(ctx, store.ListOptions{Unbounded: true})
	if err != nil {
		return 0, fmt.Errorf("sep2embed: expire ended controls: list end devices: %w", err)
	}

	removed := 0
	var errs []error
	for _, dev := range devices.Items {
		edevID, err := edevIDFromHref(dev.Href)
		if err != nil {
			errs = append(errs, fmt.Errorf("sep2embed: expire ended controls (lfdi %q): %w", dev.LFDI, err))
			continue
		}

		scope := derControlScope(edevID, controlFSAID, controlDERProgramID)
		n, err := expireDeviceControls(ctx, stores.DERControls, scope, ledger, ed, nowUnix)
		removed += n
		if err != nil {
			errs = append(errs, fmt.Errorf("sep2embed: expire ended controls (edev %q): %w", edevID, err))
			continue
		}
		if n > 0 && notifier != nil {
			notifier.Notify(ctx, derProgramListHref(edevID, controlFSAID), sep2.NotificationStatusDefault)
		}
	}

	ledger.purge(nowUnix)

	if len(errs) > 0 {
		return removed, errors.Join(errs...)
	}
	return removed, nil
}
