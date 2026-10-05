// Package sender publishes control messages to the bridge's application input
// topic on behalf of the admin UI. Publishing is gated by a runtime switch that
// is off after every start unless the operator opted in, and every flip and
// every send leaves an audit line and a row in a bounded recent-sends list.
//
// The package holds no credential: who may call it is decided by the admin
// plane in front of it, and a remote address is all it records about a caller.
package sender

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

const (
	// MaxRecent is how many sends and flips the recent list keeps.
	MaxRecent = 50
	// RateBurst and RateInterval set the send limit: one per interval, with a
	// burst of RateBurst.
	RateBurst    = 5
	RateInterval = time.Second

	// PendingExpiry is how long a published send may stay pending before its
	// row reads "outcome unknown": the frame may have been lost after a
	// fire-and-forget publish, or the control path's record of it may have
	// been evicted before anyone read it.
	PendingExpiry = 30 * time.Second
	// DefaultFlipWait bounds how long switching off waits for sends in flight.
	// go-stomp's Send ignores its context and can block for 10 seconds.
	DefaultFlipWait = 12 * time.Second

	contentTypeJSON = "application/json"
)

// Kinds of recent entry.
const (
	KindSwitch = "switch"
	KindForm   = "form"
	KindRaw    = "raw"
)

// Outcomes of a send. Issued, restated and refused are the control path's own
// words (controlobs); the other two are the sender's.
const (
	OutcomeIssued   = controlobs.ResultIssued
	OutcomeRestated = controlobs.ResultRestated
	OutcomeRefused  = controlobs.ResultRefused
	// OutcomePending: published, and the control path has not recorded it.
	OutcomePending = "pending"
	// OutcomeFailed: the bus refused the publish, or the switch went off
	// while it was in flight and cancelled it.
	OutcomeFailed = "failed"
	// OutcomeNone: published, and no record of it could be read from the
	// control path within PendingExpiry. That is not proof nothing was
	// recorded, so the row says the outcome is unknown.
	OutcomeNone = "outcome unknown"
)

// unknownReason explains an OutcomeNone row.
var unknownReason = fmt.Sprintf("no control path record after %s: the frame may have been lost, or its record evicted before it was read", PendingExpiry)

var (
	// ErrPublishingOff is returned for every send while the switch is off.
	ErrPublishingOff = errors.New("publishing is off")
	// ErrRateLimited is returned when the send limit is spent.
	ErrRateLimited = errors.New("send rate limit exceeded")
	// ErrDuplicateMRID is returned for a raw send whose difference_mrid was
	// already used: the control path keys outcomes by it, so a reuse would
	// show one message's outcome on another's row.
	ErrDuplicateMRID = errors.New("difference_mrid already used")
)

// Bus is the one operation the sender needs from the message bus.
type Bus interface {
	Send(ctx context.Context, destination, contentType string, body []byte) error
}

// OutcomeSource reads what the control path did with a message, by its
// difference_mrid. *controlobs.Hook satisfies it.
type OutcomeSource interface {
	Outcome(mrid string) (controlobs.MessageOutcome, bool)
}

// Config configures a Sender.
type Config struct {
	Bus         Bus
	Registry    *registry.Registry
	Destination string
	// Outcomes is optional; without it a published send stays pending.
	Outcomes OutcomeSource
	// PublishAtStart sets the switch on at construction. The default, off, is
	// deliberate: a restart must not silently re-arm writes to the simulation.
	PublishAtStart bool
	// FlipWait bounds how long SetPublishing(false) waits for sends already in
	// flight; zero means DefaultFlipWait.
	FlipWait time.Duration
	// Now and Logf default to time.Now and log.Printf.
	Now  func() time.Time
	Logf func(format string, args ...any)
}

// State is the switch as the UI shows it.
type State struct {
	On        bool
	ChangedAt time.Time
	// ChangedBy is the remote address of the last flip, or "start" for the
	// state the process began in.
	ChangedBy string
	// StillInFlight is, after an off flip, how many sends were still in
	// flight when its wait ended. Any of them may still reach the bus.
	StillInFlight int
}

// Delta is one forward difference of a recorded send. Result and Reason come
// from the control path once it has handled the message.
type Delta struct {
	Object    string
	Attribute string
	Value     any
	Result    string
	Reason    string
}

// Entry is one row of the recent or refusals list.
type Entry struct {
	Time           time.Time
	Kind           string
	Remote         string
	Destination    string
	DifferenceMRID string
	Deltas         []Delta
	// Outcome is on or off for a switch flip, and for a send one of the
	// Outcome constants. Reason explains refused and failed, and on an off
	// row says how many sends were still in flight when the wait ended.
	Outcome string
	Reason  string
}

// Result is what a successful publish returns.
type Result struct {
	DifferenceMRID string
	Destination    string
}

// Device is a registered device as a form's choice list offers it.
type Device struct {
	Name string
	MRID string
}

// record is a stored row. final is set once the outcome came from the control
// path, so a later eviction from that path's ring cannot change the row. flip
// orders switch rows by when the flip was made, not when its row was added.
type record struct {
	Entry
	final bool
	flip  uint64
}

// flight is one send between admission and the end of its Send.
type flight struct {
	mrid   string
	cancel context.CancelFunc
}

// Sender is safe for concurrent use.
//
// Sends and switch rows live in recent. Refusals (a send turned away before
// anything was published) live in their own list, so a flood of bad requests
// cannot push real sends out of view.
type Sender struct {
	bus      Bus
	reg      *registry.Registry
	dest     string
	outcomes OutcomeSource
	flipWait time.Duration
	now      func() time.Time
	logf     func(string, ...any)

	mu       sync.Mutex
	state    State
	flips    uint64
	tokens   float64
	refill   time.Time
	recent   []*record
	refusals []Entry
	inflight map[*flight]struct{}
	idle     []chan struct{}
}

// New returns a Sender. Bus, Registry and Destination are required.
func New(cfg Config) (*Sender, error) {
	switch {
	case cfg.Bus == nil:
		return nil, errors.New("sender: Bus is required")
	case cfg.Registry == nil:
		return nil, errors.New("sender: Registry is required")
	case cfg.Destination == "":
		return nil, errors.New("sender: Destination is required")
	}
	s := &Sender{
		bus:      cfg.Bus,
		reg:      cfg.Registry,
		dest:     cfg.Destination,
		outcomes: cfg.Outcomes,
		flipWait: cfg.FlipWait,
		now:      cfg.Now,
		logf:     cfg.Logf,
		inflight: map[*flight]struct{}{},
	}
	if s.flipWait <= 0 {
		s.flipWait = DefaultFlipWait
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logf == nil {
		s.logf = log.Printf
	}
	start := s.now()
	s.state = State{On: cfg.PublishAtStart, ChangedAt: start, ChangedBy: "start"}
	s.tokens = RateBurst
	s.refill = start
	return s, nil
}

// Publishing returns the switch state.
func (s *Sender) Publishing() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// SetPublishing sets the switch on behalf of remote and audits the call. The
// state's time and remote change only when the value does.
//
// Switching off refuses new sends at once, cancels the context of every send
// in flight, and waits up to the flip wait for them to end before recording
// the off row, so the row follows the sends it waited for. A bus whose Send
// ignores its context (go-stomp's does) can still deliver a send that outlasts
// the wait; the off row's Reason and the returned StillInFlight then say how
// many were still in flight. A flip made during that wait keeps its row
// newest.
func (s *Sender) SetPublishing(on bool, remote string) State {
	now := s.now()
	s.mu.Lock()
	old := s.state
	s.flips++
	flip := s.flips
	if old.On != on {
		s.state = State{On: on, ChangedAt: now, ChangedBy: remote}
	}
	cur := s.state
	var wait chan struct{}
	if !on && len(s.inflight) > 0 {
		for f := range s.inflight {
			f.cancel()
		}
		wait = make(chan struct{})
		s.idle = append(s.idle, wait)
	}
	s.mu.Unlock()

	reason := ""
	if wait != nil {
		timer := time.NewTimer(s.flipWait)
		select {
		case <-wait:
		case <-timer.C:
		}
		timer.Stop()
	}

	outcome := "off"
	if on {
		outcome = "on"
	}
	s.mu.Lock()
	if wait != nil {
		if n := len(s.inflight); n > 0 {
			reason = fmt.Sprintf("%d send(s) still in flight after %s; they may still be delivered", n, s.flipWait)
			cur.StillInFlight = n
			if s.flips == flip {
				s.state.StillInFlight = n
			}
		}
	}
	s.addFlipLocked(Entry{Time: now, Kind: KindSwitch, Remote: remote, Outcome: outcome, Reason: reason}, flip)
	s.mu.Unlock()
	s.logf("sender: audit kind=switch remote=%q old=%s new=%s changed=%t at=%s reason=%q",
		remote, onOff(old.On), onOff(cur.On), old.On != on, now.UTC().Format(time.RFC3339), reason)
	return cur
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// Devices lists the registered devices by name, then mRID. A device with no
// name is listed under its mRID.
func (s *Sender) Devices() []Device {
	entries := s.reg.Snapshot()
	out := make([]Device, 0, len(entries))
	for _, e := range entries {
		name := e.Name
		if name == "" {
			name = e.MRID
		}
		out = append(out, Device{Name: name, MRID: e.MRID})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].MRID < out[j].MRID
	})
	return out
}

// SendActivePower publishes an active power target for the device.
func (s *Sender) SendActivePower(ctx context.Context, remote, deviceMRID string, multiplier, value int) (Result, error) {
	return s.publish(ctx, KindForm, remote, func(now time.Time) ([]byte, error) {
		return marshalForm(ActivePowerMessage(now, deviceMRID, multiplier, value))
	})
}

// SendReactivePower publishes a reactive power target for the device.
func (s *Sender) SendReactivePower(ctx context.Context, remote, deviceMRID string, multiplier, value int) (Result, error) {
	return s.publish(ctx, KindForm, remote, func(now time.Time) ([]byte, error) {
		return marshalForm(ReactivePowerMessage(now, deviceMRID, multiplier, value))
	})
}

// SendConnect publishes a connect or disconnect for the device.
func (s *Sender) SendConnect(ctx context.Context, remote, deviceMRID string, connect bool) (Result, error) {
	return s.publish(ctx, KindForm, remote, func(now time.Time) ([]byte, error) {
		return marshalForm(ConnectMessage(now, deviceMRID, connect))
	})
}

// SendEnergize publishes an energize or de-energize for the device.
func (s *Sender) SendEnergize(ctx context.Context, remote, deviceMRID string, energize bool) (Result, error) {
	return s.publish(ctx, KindForm, remote, func(now time.Time) ([]byte, error) {
		return marshalForm(EnergizeMessage(now, deviceMRID, energize))
	})
}

// SendRaw validates body and publishes it byte for byte. A refusal for a bad
// body is a *ValidationError naming the first failing JSON path, and a reused
// difference_mrid is ErrDuplicateMRID.
func (s *Sender) SendRaw(ctx context.Context, remote string, body []byte) (Result, error) {
	return s.publish(ctx, KindRaw, remote, func(time.Time) ([]byte, error) { return body, nil })
}

func marshalForm(msg *diff.Message, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	return json.Marshal(msg)
}

// publish is the one send path. A form passes through validateRaw like a raw
// body does, so a form can never emit what the subscriber would refuse. The
// body is validated before a rate token is spent, so refused bodies cost no
// burst.
func (s *Sender) publish(ctx context.Context, kind, remote string, build func(time.Time) ([]byte, error)) (Result, error) {
	now := s.now()
	if err := s.checkOn(); err != nil {
		s.refuse(now, kind, remote, "", err)
		return Result{}, err
	}
	body, err := build(now)
	if err != nil {
		s.refuse(now, kind, remote, "", err)
		return Result{}, err
	}
	parsed, err := validateRaw(s.reg, body)
	if err != nil {
		s.refuse(now, kind, remote, "", err)
		return Result{}, err
	}
	sendCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	f, err := s.admit(now, kind == KindRaw, parsed.DifferenceMRID, cancel)
	if err != nil {
		s.refuse(now, kind, remote, parsed.DifferenceMRID, err)
		return Result{}, err
	}
	deltas := make([]Delta, len(parsed.Forward))
	for i, d := range parsed.Forward {
		deltas[i] = Delta{Object: d.Object, Attribute: d.Attribute, Value: d.Value}
	}
	sendErr := s.bus.Send(sendCtx, s.dest, contentTypeJSON, body)

	e := Entry{Time: now, Kind: kind, Remote: remote, Destination: s.dest, DifferenceMRID: parsed.DifferenceMRID, Deltas: deltas}
	var result Result
	if sendErr != nil {
		sendErr = fmt.Errorf("sender: publish to %s: %w", s.dest, sendErr)
		e.Outcome, e.Reason = OutcomeFailed, sendErr.Error()
	} else {
		result = Result{DifferenceMRID: parsed.DifferenceMRID, Destination: s.dest}
	}
	s.finish(f, e)
	s.audit(e)
	return result, sendErr
}

func (s *Sender) checkOn() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.On {
		return ErrPublishingOff
	}
	return nil
}

// admit is the one atomic gate before a publish: the switch, the difference_mrid
// reuse check (raw only: a form's is freshly minted), and a rate token. A send
// that passes is registered in flight, which also reserves its mrid against a
// concurrent send of the same one.
func (s *Sender) admit(now time.Time, checkMRID bool, mrid string, cancel context.CancelFunc) (*flight, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.On {
		return nil, ErrPublishingOff
	}
	if checkMRID && s.mridUsedLocked(mrid) {
		return nil, fmt.Errorf("%w: %q was already used by a recent send or is held by the control path; use a new one", ErrDuplicateMRID, mrid)
	}
	if elapsed := now.Sub(s.refill); elapsed > 0 {
		s.tokens += float64(elapsed) / float64(RateInterval)
		if s.tokens > RateBurst {
			s.tokens = RateBurst
		}
		s.refill = now
	}
	if s.tokens < 1 {
		return nil, ErrRateLimited
	}
	s.tokens--
	f := &flight{mrid: mrid, cancel: cancel}
	s.inflight[f] = struct{}{}
	return f, nil
}

// mridUsedLocked reports whether mrid belongs to a send in flight, a send still
// in the recent list, or a message the control path still holds. Refusals are
// not in these lists, and a failed or cancelled send does not hold its mrid,
// so either may be sent again with the same one; the control path check still
// refuses it if the first attempt reached the bus after all.
func (s *Sender) mridUsedLocked(mrid string) bool {
	for f := range s.inflight {
		if f.mrid == mrid {
			return true
		}
	}
	for _, r := range s.recent {
		if r.Kind != KindSwitch && r.Outcome != OutcomeFailed && r.DifferenceMRID == mrid {
			return true
		}
	}
	if s.outcomes != nil {
		if _, ok := s.outcomes.Outcome(mrid); ok {
			return true
		}
	}
	return false
}

// finish ends a flight and records its row in the same critical section, so a
// waiting flip sees the row before it records its own.
func (s *Sender) finish(f *flight, e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, f)
	s.addLocked(e)
	if len(s.inflight) == 0 {
		for _, c := range s.idle {
			close(c)
		}
		s.idle = nil
	}
}

func (s *Sender) refuse(now time.Time, kind, remote, mrid string, err error) {
	e := Entry{Time: now, Kind: kind, Remote: remote, DifferenceMRID: mrid, Outcome: OutcomeRefused, Reason: err.Error()}
	s.mu.Lock()
	s.refusals = append(s.refusals, e)
	if over := len(s.refusals) - MaxRecent; over > 0 {
		s.refusals = append([]Entry(nil), s.refusals[over:]...)
	}
	s.mu.Unlock()
	s.audit(e)
}

func (s *Sender) addLocked(e Entry) {
	s.recent = append(s.recent, &record{Entry: e})
	s.trimLocked()
}

// addFlipLocked adds a switch row ahead of any switch row of a later flip,
// which an off flip's wait can let in first.
func (s *Sender) addFlipLocked(e Entry, flip uint64) {
	rec := &record{Entry: e, flip: flip}
	at := len(s.recent)
	for i, r := range s.recent {
		if r.Kind == KindSwitch && r.flip > flip {
			at = i
			break
		}
	}
	s.recent = append(s.recent[:at], append([]*record{rec}, s.recent[at:]...)...)
	s.trimLocked()
}

func (s *Sender) trimLocked() {
	if over := len(s.recent) - MaxRecent; over > 0 {
		s.recent = append([]*record(nil), s.recent[over:]...)
	}
}

// audit writes one line per send. Every string that may carry caller input is
// quoted, so a newline in it cannot forge a second line.
func (s *Sender) audit(e Entry) {
	var b strings.Builder
	for i, d := range e.Deltas {
		if i > 0 {
			b.WriteString(" ")
		}
		val, err := json.Marshal(d.Value)
		if err != nil {
			val = []byte("null")
		}
		fmt.Fprintf(&b, "[object=%q attribute=%q value=%q]", d.Object, d.Attribute, val)
	}
	outcome := e.Outcome
	if outcome == "" {
		outcome = "published"
	}
	s.logf("sender: audit kind=%s remote=%q destination=%q difference_mrid=%q outcome=%s reason=%q deltas=%s",
		e.Kind, e.Remote, e.Destination, e.DifferenceMRID, outcome, e.Reason, b.String())
}

// Refusals returns the newest MaxRecent sends that were turned away before
// anything was published, newest first.
func (s *Sender) Refusals() []Entry {
	s.mu.Lock()
	out := make([]Entry, len(s.refusals))
	copy(out, s.refusals)
	s.mu.Unlock()
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Recent returns the sends and switch rows, newest first, with each published
// send's outcome read from the control path. Once the control path has
// recorded a send the answer is kept on the row, so the shared outcome ring
// evicting it later changes nothing. A send not yet recorded is pending, and
// reads "outcome unknown" once PendingExpiry has passed; a record that arrives
// later still resolves it. A record older than the send belongs to an earlier
// message that reused the difference_mrid and is ignored.
func (s *Sender) Recent() []Entry {
	s.mu.Lock()
	recs := make([]*record, len(s.recent))
	copy(recs, s.recent)
	entries := make([]Entry, len(recs))
	finals := make([]bool, len(recs))
	for i, r := range recs {
		entries[i] = r.Entry
		entries[i].Deltas = append([]Delta(nil), r.Deltas...)
		finals[i] = r.final
	}
	s.mu.Unlock()

	now := s.now()
	for i := range entries {
		e := &entries[i]
		if e.Kind == KindSwitch || finals[i] || e.Outcome == OutcomeFailed {
			continue
		}
		if s.resolve(e) {
			s.mu.Lock()
			recs[i].Entry = *e
			recs[i].Deltas = append([]Delta(nil), e.Deltas...)
			recs[i].final = true
			s.mu.Unlock()
		} else if now.Sub(e.Time) > PendingExpiry {
			e.Outcome, e.Reason = OutcomeNone, unknownReason
		}
	}
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries
}

// resolve fills e from the control path and reports whether the path had
// recorded it; otherwise e is left pending.
func (s *Sender) resolve(e *Entry) bool {
	e.Outcome = OutcomePending
	if s.outcomes == nil {
		return false
	}
	got, ok := s.outcomes.Outcome(e.DifferenceMRID)
	if !ok || got.At.Before(e.Time) || len(got.Deltas) != len(e.Deltas) {
		return false
	}
	issued, restated, refused := 0, 0, 0
	for i, d := range got.Deltas {
		e.Deltas[i].Result = d.Result
		e.Deltas[i].Reason = d.Reason
		switch d.Result {
		case controlobs.ResultIssued:
			issued++
		case controlobs.ResultRestated:
			restated++
		default:
			refused++
		}
	}
	switch {
	case refused > 0:
		e.Outcome = OutcomeRefused
		e.Reason = fmt.Sprintf("%d of %d differences issued, %d restated, %d refused", issued, len(got.Deltas), restated, refused)
	case issued > 0:
		e.Outcome = OutcomeIssued
	default:
		e.Outcome = OutcomeRestated
	}
	return true
}
