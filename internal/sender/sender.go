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
	// OutcomeFailed: the bus refused the publish.
	OutcomeFailed = "failed"
)

var (
	// ErrPublishingOff is returned for every send while the switch is off.
	ErrPublishingOff = errors.New("publishing is off")
	// ErrRateLimited is returned when the send limit is spent.
	ErrRateLimited = errors.New("send rate limit exceeded")
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

// Entry is one row of the recent list.
type Entry struct {
	Time           time.Time
	Kind           string
	Remote         string
	Destination    string
	DifferenceMRID string
	Deltas         []Delta
	// Outcome is on or off for a switch flip, and for a send one of the
	// Outcome constants. Reason explains refused and failed.
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

// Sender is safe for concurrent use.
type Sender struct {
	bus      Bus
	reg      *registry.Registry
	dest     string
	outcomes OutcomeSource
	now      func() time.Time
	logf     func(string, ...any)

	mu     sync.Mutex
	state  State
	tokens float64
	refill time.Time
	recent []Entry
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
		now:      cfg.Now,
		logf:     cfg.Logf,
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
func (s *Sender) SetPublishing(on bool, remote string) State {
	now := s.now()
	s.mu.Lock()
	old := s.state
	if old.On != on {
		s.state = State{On: on, ChangedAt: now, ChangedBy: remote}
	}
	cur := s.state
	outcome := "off"
	if on {
		outcome = "on"
	}
	s.addLocked(Entry{Time: now, Kind: KindSwitch, Remote: remote, Outcome: outcome})
	s.mu.Unlock()

	s.logf("sender: audit kind=switch remote=%q old=%s new=%s changed=%t at=%s",
		remote, onOff(old.On), onOff(cur.On), old.On != on, now.UTC().Format(time.RFC3339))
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
// body is a *ValidationError naming the first failing JSON path.
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
// body does, so a form can never emit what the subscriber would refuse.
func (s *Sender) publish(ctx context.Context, kind, remote string, build func(time.Time) ([]byte, error)) (Result, error) {
	now := s.now()
	if err := s.admit(now); err != nil {
		s.refuse(now, kind, remote, nil, "", err)
		return Result{}, err
	}
	body, err := build(now)
	if err != nil {
		s.refuse(now, kind, remote, nil, "", err)
		return Result{}, err
	}
	parsed, err := validateRaw(s.reg, body)
	if err != nil {
		s.refuse(now, kind, remote, nil, "", err)
		return Result{}, err
	}
	deltas := make([]Delta, len(parsed.Forward))
	for i, d := range parsed.Forward {
		deltas[i] = Delta{Object: d.Object, Attribute: d.Attribute, Value: d.Value}
	}
	if err := s.bus.Send(ctx, s.dest, contentTypeJSON, body); err != nil {
		wrapped := fmt.Errorf("sender: publish to %s: %w", s.dest, err)
		s.fail(now, kind, remote, deltas, parsed.DifferenceMRID, wrapped)
		return Result{}, wrapped
	}
	e := Entry{Time: now, Kind: kind, Remote: remote, Destination: s.dest, DifferenceMRID: parsed.DifferenceMRID, Deltas: deltas}
	s.mu.Lock()
	s.addLocked(e)
	s.mu.Unlock()
	s.audit(e)
	return Result{DifferenceMRID: parsed.DifferenceMRID, Destination: s.dest}, nil
}

// admit refuses while the switch is off, then spends a rate token. A send
// refused for the switch spends nothing.
func (s *Sender) admit(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.On {
		return ErrPublishingOff
	}
	if elapsed := now.Sub(s.refill); elapsed > 0 {
		s.tokens += float64(elapsed) / float64(RateInterval)
		if s.tokens > RateBurst {
			s.tokens = RateBurst
		}
		s.refill = now
	}
	if s.tokens < 1 {
		return ErrRateLimited
	}
	s.tokens--
	return nil
}

func (s *Sender) refuse(now time.Time, kind, remote string, deltas []Delta, mrid string, err error) {
	e := Entry{Time: now, Kind: kind, Remote: remote, DifferenceMRID: mrid, Deltas: deltas, Outcome: OutcomeRefused, Reason: err.Error()}
	s.mu.Lock()
	s.addLocked(e)
	s.mu.Unlock()
	s.audit(e)
}

func (s *Sender) fail(now time.Time, kind, remote string, deltas []Delta, mrid string, err error) {
	e := Entry{Time: now, Kind: kind, Remote: remote, Destination: s.dest, DifferenceMRID: mrid, Deltas: deltas, Outcome: OutcomeFailed, Reason: err.Error()}
	s.mu.Lock()
	s.addLocked(e)
	s.mu.Unlock()
	s.audit(e)
}

func (s *Sender) addLocked(e Entry) {
	s.recent = append(s.recent, e)
	if over := len(s.recent) - MaxRecent; over > 0 {
		s.recent = append([]Entry(nil), s.recent[over:]...)
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

// Recent returns the recent list, newest first, with each published send's
// outcome read from the control path now. A send the control path has not
// recorded since it was published is pending; a record older than the send
// belongs to an earlier message that reused the difference_mrid and is ignored.
func (s *Sender) Recent() []Entry {
	s.mu.Lock()
	entries := make([]Entry, len(s.recent))
	copy(entries, s.recent)
	s.mu.Unlock()

	for i := range entries {
		e := &entries[i]
		e.Deltas = append([]Delta(nil), e.Deltas...)
		if e.Outcome == "" {
			s.resolve(e)
		}
	}
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries
}

func (s *Sender) resolve(e *Entry) {
	e.Outcome = OutcomePending
	if s.outcomes == nil {
		return
	}
	got, ok := s.outcomes.Outcome(e.DifferenceMRID)
	if !ok || got.At.Before(e.Time) || len(got.Deltas) != len(e.Deltas) {
		return
	}
	issued, restated := 0, 0
	for i, d := range got.Deltas {
		e.Deltas[i].Result = d.Result
		e.Deltas[i].Reason = d.Reason
		switch d.Result {
		case controlobs.ResultIssued:
			issued++
		case controlobs.ResultRestated:
			restated++
		}
	}
	switch {
	case issued+restated < len(got.Deltas):
		e.Outcome = OutcomeRefused
	case issued > 0:
		e.Outcome = OutcomeIssued
	default:
		e.Outcome = OutcomeRestated
	}
}
