// Package socsend publishes a state of charge to one device on the
// application input topic and follows the value back: into the device's
// stored DERStatus and onto the application output topic.
//
// The package holds no credential and applies no authorization; who may
// call it is decided by the listener in front of it. It does not read the
// admin UI bus sender's switch.
package socsend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/busmonitor"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// Attribute is the difference attribute a send carries. The bridge's
// control subscriber shares the input topic and passes over it.
const Attribute = "DERStatus.stateOfChargeStatus"

const (
	// DefaultHold is the hold a send gets when the caller names none.
	DefaultHold = 60 * time.Second
	MinHold     = time.Second
	MaxHold     = time.Hour
	// MaxLedger is how many sends are kept. Past it, the oldest finished
	// send is dropped; a send still awaiting its verdict is never dropped.
	MaxLedger = 64
	// VerifyGrace is how long after the hold ends a send may still be
	// matched before its verdict is final.
	VerifyGrace = 60 * time.Second
	// DefaultPollInterval is how often Run looks for the value coming back.
	DefaultPollInterval = 2 * time.Second
	// FeedRetryMin and FeedRetryMax bound the wait before the output feed is
	// reopened after the monitor reports it refused, failed or closed.
	FeedRetryMin = 2 * time.Second
	FeedRetryMax = time.Minute

	contentTypeJSON = "application/json"

	feedDownNote         = "the output topic feed is unavailable and is being reopened, so a value on it may not be seen"
	feedReconnectingNote = "the output topic feed is reconnecting, so a value on it may not be seen"
	storeNote            = "the device status store could not be read, so the device stage was not checked"
	baseNote             = "the device's value before the send could not be read, so its post cannot be told from the earlier value"
)

// Kinds of ledger entry.
const (
	KindSend  = "send"
	KindClear = "clear"
)

// Verdicts. A clear has no round trip to judge and reads VerdictNone.
const (
	VerdictPending  = "pending"
	VerdictMatched  = "matched"
	VerdictNotSeen  = "not seen"
	VerdictMismatch = "mismatch"
	VerdictNone     = "none"
)

var (
	ErrPercentRange  = errors.New("percent must be an integer from 0 to 100")
	ErrHoldRange     = errors.New("hold must be from 1 second to 1 hour")
	ErrUnknownDevice = errors.New("device is not registered")
	// ErrPublishFailed carries no transport text: the bus error can name
	// the broker and its user.
	ErrPublishFailed = errors.New("publish failed")
)

// Bus is the one operation a send needs from the message bus.
type Bus interface {
	Send(ctx context.Context, destination, contentType string, body []byte) error
}

// Devices is the registry read a send needs.
type Devices interface {
	Get(mrid string) (registry.Entry, bool)
}

// Statuses reads the DERStatus each device last posted.
type Statuses interface {
	DERStatusSnapshots(ctx context.Context) ([]sep2embed.DERStatusSnapshot, error)
}

// Feed is one viewer of a watched topic.
type Feed interface {
	Backlog() []busmonitor.Message
	Events() <-chan busmonitor.Event
	Close()
}

// Watcher opens a Feed on a topic.
type Watcher interface {
	Watch(name string) (Feed, error)
}

type monitorWatcher struct{ m *busmonitor.Monitor }

func (w monitorWatcher) Watch(name string) (Feed, error) {
	v, err := w.m.Watch(name)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// MonitorWatcher adapts a bus monitor to Watcher.
func MonitorWatcher(m *busmonitor.Monitor) Watcher { return monitorWatcher{m} }

// Config configures a Service.
type Config struct {
	Bus      Bus
	Devices  Devices
	Statuses Statuses
	// Watcher is optional. Without it the output topic is never seen.
	Watcher     Watcher
	InputTopic  string
	OutputTopic string
	// PollInterval, Now and Logf default to DefaultPollInterval, time.Now
	// and log.Printf.
	PollInterval time.Duration
	Now          func() time.Time
	Logf         func(format string, args ...any)
}

// Status is one send and how far its value has come back. Times are set
// when the stage was first observed, which for the device's DERStatus is
// when this service next read the store, not when the device posted.
//
// Verdict is matched when the device posted the value and the output topic
// carried it. After the hold and VerifyGrace it is mismatch when the value
// reached one of the two and a different value reached the other, and not
// seen otherwise.
type Status struct {
	ID          string     `json:"id"`
	MRID        string     `json:"mrid"`
	Kind        string     `json:"kind"`
	Percent     int        `json:"percent"`
	HoldSeconds int        `json:"holdSeconds"`
	SentAt      time.Time  `json:"sentAt"`
	PostedAt    *time.Time `json:"postedAt,omitempty"`
	SeenAt      *time.Time `json:"seenAt,omitempty"`
	ReleasedAt  *time.Time `json:"releasedAt,omitempty"`
	// DeviceReported and OutputReported are a different percent the device
	// or the output topic carried for this device since the send.
	DeviceReported *float64 `json:"deviceReportedPercent,omitempty"`
	OutputReported *float64 `json:"outputReportedPercent,omitempty"`
	Verdict        string   `json:"verdict"`
	Note           string   `json:"note,omitempty"`
}

// derKey names one DER of one end device inside a status snapshot.
type derKey struct{ edev, der string }

// point is a stored state of charge: the value in hundredths of a percent
// and the dateTime the device stamped on it.
type point struct {
	value    uint16
	dateTime int64
}

type record struct {
	Status
	hold      time.Duration
	deadline  time.Time
	cutFrames bool
	// base is what the device had stored when the send was made. A stored
	// value counts as the device's reply only if it differs from base or
	// carries a newer dateTime. baseUnknown is set when the store could not
	// be read at send, so nothing can be told apart from the earlier value.
	base        map[derKey]point
	baseUnknown bool
	// storeErr is set when the store could not be read during the window.
	storeErr bool
	// feedNote is the output feed's trouble as of the last refresh.
	feedNote string
	// done is set by the settle that ends the follow: the deadline passed,
	// or the send matched and was released.
	done bool
}

// Service sends and follows state-of-charge values. It is safe for
// concurrent use.
type Service struct {
	cfg Config

	// refreshMu serializes refresh and owns feed.
	refreshMu sync.Mutex
	feed      Feed
	// feedNote, backoff and reopenAt describe the output feed's trouble: a
	// terminal status or a failed Watch closes the feed and holds off the
	// next open, doubling the wait up to FeedRetryMax.
	feedNote string
	backoff  time.Duration
	reopenAt time.Time

	mu     sync.Mutex
	ledger []*record
}

// New validates cfg and returns a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Bus == nil || cfg.Devices == nil || cfg.Statuses == nil {
		return nil, errors.New("socsend: Bus, Devices and Statuses are required")
	}
	if cfg.InputTopic == "" {
		return nil, errors.New("socsend: InputTopic is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	return &Service{cfg: cfg}, nil
}

// Send publishes percent for the device mrid, to be held for hold.
func (s *Service) Send(ctx context.Context, mrid string, percent int, hold time.Duration) (Status, error) {
	if percent < 0 || percent > 100 {
		return Status{}, ErrPercentRange
	}
	if hold < MinHold || hold > MaxHold {
		return Status{}, ErrHoldRange
	}
	return s.publish(ctx, KindSend, mrid, percent, hold)
}

// Clear publishes a hold of zero, which ends a held value at once.
func (s *Service) Clear(ctx context.Context, mrid string) (Status, error) {
	return s.publish(ctx, KindClear, mrid, 0, 0)
}

func (s *Service) publish(ctx context.Context, kind, mrid string, percent int, hold time.Duration) (Status, error) {
	if _, ok := s.cfg.Devices.Get(mrid); !ok {
		return Status{}, ErrUnknownDevice
	}
	now := s.cfg.Now()
	var base map[derKey]point
	baseUnknown := false
	if kind == KindSend {
		base, baseUnknown = s.readBaseline(ctx, mrid)
	}
	msg, err := diff.NewBuilder("").Message(now.UTC().Unix())
	if err != nil {
		s.cfg.Logf("socsend: build message: %v", err)
		return Status{}, ErrPublishFailed
	}
	msg.Input.Message.ForwardDifferences = []diff.Difference{{
		Object:    mrid,
		Attribute: Attribute,
		Value:     map[string]any{"percent": percent, "hold_seconds": int(hold / time.Second)},
	}}
	msg.Input.Message.ReverseDifferences = []diff.Difference{}
	body, err := json.Marshal(msg)
	if err != nil {
		s.cfg.Logf("socsend: encode message: %v", err)
		return Status{}, ErrPublishFailed
	}
	if err := s.cfg.Bus.Send(ctx, s.cfg.InputTopic, contentTypeJSON, body); err != nil {
		s.cfg.Logf("socsend: publish to %s: %v", s.cfg.InputTopic, err)
		return Status{}, ErrPublishFailed
	}

	rec := &record{
		Status: Status{
			ID: msg.Input.Message.DifferenceMRID, MRID: mrid, Kind: kind,
			Percent: percent, HoldSeconds: int(hold / time.Second),
			SentAt: now, Verdict: VerdictPending,
		},
		hold:        hold,
		deadline:    now.Add(hold + VerifyGrace),
		base:        base,
		baseUnknown: baseUnknown,
	}
	if kind == KindClear {
		rec.Verdict = VerdictNone
	}
	s.mu.Lock()
	s.ledger = append(s.ledger, rec)
	s.trimLedgerLocked()
	s.mu.Unlock()
	return rec.Status, nil
}

// trimLedgerLocked drops the oldest finished entries until the ledger is
// within MaxLedger. A send whose verdict is still pending is kept even if
// that leaves the ledger over the limit: the callers' rate limit bounds how
// many can be pending, and a flushed pending send reads as "no such send".
func (s *Service) trimLedgerLocked() {
	for len(s.ledger) > MaxLedger {
		drop := -1
		for i, r := range s.ledger {
			if !active(r) {
				drop = i
				break
			}
		}
		if drop < 0 {
			return
		}
		s.ledger = append(s.ledger[:drop:drop], s.ledger[drop+1:]...)
	}
}

// readBaseline records what the device has stored for mrid before the send.
func (s *Service) readBaseline(ctx context.Context, mrid string) (map[derKey]point, bool) {
	snaps, err := s.cfg.Statuses.DERStatusSnapshots(ctx)
	if err != nil {
		s.cfg.Logf("socsend: read device status before send: %v", err)
		return nil, true
	}
	base := map[derKey]point{}
	for _, sn := range snaps {
		if sn.MRID != mrid || sn.Status.StateOfChargeStatus == nil {
			continue
		}
		soc := sn.Status.StateOfChargeStatus
		base[derKey{sn.EDevID, sn.DERID}] = point{soc.Value, soc.DateTime}
	}
	return base, false
}

// fresh reports whether sn is a post made after the send: its value or its
// dateTime moved from the baseline. A DER the baseline never saw is fresh.
func (r *record) fresh(sn sep2embed.DERStatusSnapshot) bool {
	b, ok := r.base[derKey{sn.EDevID, sn.DERID}]
	if !ok {
		return true
	}
	soc := sn.Status.StateOfChargeStatus
	return soc.Value != b.value || soc.DateTime > b.dateTime
}

// Status returns the send with the given id after looking for its value
// once more.
func (s *Service) Status(ctx context.Context, id string) (Status, bool) {
	s.refresh(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.ledger {
		if r.ID == id {
			return r.Status, true
		}
	}
	return Status{}, false
}

// Run follows pending sends until ctx is cancelled, so a held value is
// seen even when nobody is asking.
func (s *Service) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()
	defer s.closeFeed()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.refresh(ctx)
		}
	}
}

func (s *Service) closeFeed() {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if s.feed != nil {
		s.feed.Close()
		s.feed = nil
	}
}

// active reports whether r is still being followed.
func active(r *record) bool { return r.Kind == KindSend && !r.done }

type sighting struct {
	at    time.Time
	diffs []diff.Difference
	cut   bool
}

func (s *Service) anyActive(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.ledger {
		if active(r) {
			return true
		}
	}
	return false
}

// refresh reads the device statuses and the output feed once and applies
// what they show to every active send.
func (s *Service) refresh(ctx context.Context) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	now := s.cfg.Now()
	if !s.anyActive(now) {
		if s.feed != nil {
			s.feed.Close()
			s.feed = nil
		}
		s.feedNote, s.backoff, s.reopenAt = "", 0, time.Time{}
		return
	}

	snaps, serr := s.cfg.Statuses.DERStatusSnapshots(ctx)
	if serr != nil {
		s.cfg.Logf("socsend: read device status: %v", serr)
	}
	sights := s.drainFeed(now)

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.ledger {
		if !active(r) {
			continue
		}
		r.feedNote = s.feedNote
		if now.Before(r.deadline) {
			if serr != nil {
				r.storeErr = true
			} else {
				observeDevice(r, snaps, now)
			}
		}
		for _, sg := range sights {
			observeOutput(r, sg)
		}
		s.settle(r, now)
	}
}

// drainFeed returns the frames that arrived on the output topic since the
// last call, opening the feed first if it is not open and its retry wait
// has passed.
func (s *Service) drainFeed(now time.Time) []sighting {
	if s.cfg.Watcher == nil || s.cfg.OutputTopic == "" {
		return nil
	}
	var out []sighting
	if s.feed == nil {
		if now.Before(s.reopenAt) {
			return nil
		}
		f, err := s.cfg.Watcher.Watch(s.cfg.OutputTopic)
		if err != nil {
			s.cfg.Logf("socsend: watch %s: %v", s.cfg.OutputTopic, err)
			s.feedDown(now)
			return nil
		}
		s.feed = f
		for _, m := range f.Backlog() {
			out = append(out, parseFrame(m))
		}
	}
	for {
		select {
		case ev, ok := <-s.feed.Events():
			if !ok {
				// The monitor closed a viewer that fell behind or the
				// topic went away; the next refresh opens a new one.
				s.feed.Close()
				s.feed = nil
				return out
			}
			switch ev.Kind {
			case busmonitor.EventMessage:
				out = append(out, parseFrame(ev.Message))
			case busmonitor.EventStatus:
				if s.onFeedStatus(ev.Status, now) {
					return out
				}
			}
		default:
			return out
		}
	}
}

// onFeedStatus logs one connection state change of the output topic and
// keeps the note sends carry. It reports true when it closed the feed.
func (s *Service) onFeedStatus(st busmonitor.Status, now time.Time) bool {
	s.cfg.Logf("socsend: output topic %s is %s: %s", s.cfg.OutputTopic, st.State, st.Reason)
	switch st.State {
	case busmonitor.StateLive:
		s.feedNote, s.backoff = "", 0
	case busmonitor.StateRefused, busmonitor.StateFailed, busmonitor.StateClosed:
		s.feed.Close()
		s.feed = nil
		s.feedDown(now)
		return true
	default:
		s.feedNote = feedReconnectingNote
	}
	return false
}

// feedDown notes that the output feed is unavailable and holds off the next
// open, doubling the wait each time up to FeedRetryMax.
func (s *Service) feedDown(now time.Time) {
	s.feedNote = feedDownNote
	switch {
	case s.backoff == 0:
		s.backoff = FeedRetryMin
	case s.backoff < FeedRetryMax:
		s.backoff = min(2*s.backoff, FeedRetryMax)
	}
	s.reopenAt = now.Add(s.backoff)
}

func parseFrame(m busmonitor.Message) sighting {
	sg := sighting{at: m.Received, cut: m.Truncated}
	if !m.Truncated {
		var env diff.Message
		if err := json.Unmarshal(m.Body, &env); err == nil {
			sg.diffs = env.Input.Message.ForwardDifferences
		}
		return sg
	}
	// A frame cut at the monitor's size limit is still read as far as it
	// goes, one difference at a time.
	const key = `"forward_differences"`
	i := bytes.Index(m.Body, []byte(key))
	if i < 0 {
		return sg
	}
	rest := m.Body[i+len(key):]
	j := bytes.IndexByte(rest, '[')
	if j < 0 {
		return sg
	}
	dec := json.NewDecoder(bytes.NewReader(rest[j:]))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return sg
	}
	for dec.More() {
		var d diff.Difference
		if dec.Decode(&d) != nil {
			break
		}
		sg.diffs = append(sg.diffs, d)
	}
	return sg
}

func observeDevice(r *record, snaps []sep2embed.DERStatusSnapshot, now time.Time) {
	var equal bool
	var otherAny, otherFresh *float64
	for _, sn := range snaps {
		if sn.MRID != r.MRID || sn.Status.StateOfChargeStatus == nil {
			continue
		}
		v := sn.Status.StateOfChargeStatus.Value
		if int(v) == r.Percent*100 {
			// The value the device held before the send is not its reply.
			if r.fresh(sn) {
				equal = true
			}
			continue
		}
		p := float64(v) / 100
		otherAny = &p
		if r.fresh(sn) {
			otherFresh = &p
		}
	}
	if r.PostedAt == nil {
		if equal {
			r.PostedAt = &now
		} else {
			r.DeviceReported = otherFresh
		}
		return
	}
	// Released: the held value has gone and the device reports another.
	if !equal && otherAny != nil && r.ReleasedAt == nil && !now.Before(r.SentAt.Add(r.hold)) {
		r.ReleasedAt = &now
	}
}

// observeOutput counts a frame only if it arrived after the send and
// before the verdict was due, so draining late cannot add what the
// verdict never saw.
func observeOutput(r *record, sg sighting) {
	if sg.at.Before(r.SentAt) || !sg.at.Before(r.deadline) {
		return
	}
	found := false
	for _, d := range sg.diffs {
		if d.Object != r.MRID || d.Attribute != Attribute {
			continue
		}
		v, ok := d.Value.(float64)
		if !ok {
			continue
		}
		found = true
		if v == float64(r.Percent) {
			if r.SeenAt == nil {
				at := sg.at
				r.SeenAt = &at
			}
			continue
		}
		r.OutputReported = &v
	}
	if !found && sg.cut {
		r.cutFrames = true
	}
}

// settle sets r's verdict and note from what has been observed.
func (s *Service) settle(r *record, now time.Time) {
	expired := !now.Before(r.deadline)
	if r.Verdict == VerdictMatched {
		r.done = r.ReleasedAt != nil || expired
		return
	}
	r.done = expired
	switch {
	case r.PostedAt != nil && r.SeenAt != nil:
		r.Verdict = VerdictMatched
		r.done = r.ReleasedAt != nil || expired
	case !expired:
		r.Verdict = VerdictPending
	case (r.PostedAt != nil && r.OutputReported != nil) || (r.SeenAt != nil && r.PostedAt == nil && r.DeviceReported != nil):
		r.Verdict = VerdictMismatch
	default:
		r.Verdict = VerdictNotSeen
	}
	r.Note = r.notes()
}

// notes joins what the reader needs to know about how far the verdict can be
// trusted: the feed's trouble while it can still matter, and for a verdict
// of not seen the stage that could not be checked.
func (r *record) notes() string {
	var parts []string
	if r.Verdict != VerdictMatched && r.feedNote != "" {
		parts = append(parts, r.feedNote)
	}
	if r.baseUnknown && r.PostedAt != nil {
		parts = append(parts, baseNote)
	}
	if r.Verdict == VerdictNotSeen {
		if r.storeErr && r.PostedAt == nil {
			parts = append(parts, storeNote)
		}
		if r.cutFrames && r.SeenAt == nil {
			parts = append(parts, fmt.Sprintf("output frames larger than %d bytes are cut by the bus monitor, so a device late in a frame may not be seen", busmonitor.MaxBodyBytes))
		}
	}
	return strings.Join(parts, "; ")
}
