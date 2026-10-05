package busmonitor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp"
	"github.com/go-stomp/stomp/v3"
	"github.com/go-stomp/stomp/v3/frame"
)

// Session is one STOMP connection. *cimstomp.Client satisfies it.
type Session interface {
	Subscribe(ctx context.Context, destination string, opts ...cimstomp.SubscribeOption) (*cimstomp.Subscription, error)
	Close() error
}

// Dialer opens a new, dedicated connection. The monitor calls it once per
// topic attempt and never shares the result.
type Dialer func(ctx context.Context) (Session, error)

// NewDialer returns a Dialer that opens a fresh cimstomp.Client per call, so
// each watched topic authenticates and connects on its own.
func NewDialer(cfg cimstomp.STOMPConfig) Dialer {
	return func(ctx context.Context) (Session, error) {
		c := cimstomp.NewClient(cfg)
		if err := c.Connect(ctx); err != nil {
			return nil, err
		}
		return c, nil
	}
}

// Config configures a Monitor. Zero limits take the design values.
type Config struct {
	Dial Dialer
	// Probe returns the health-probe destination, or "" when there is none.
	Probe          func() string
	ConnectTimeout time.Duration
	// IdleClose, BackoffMin, BackoffMax and MaxTries default to the package
	// constants; tests shorten them.
	IdleClose  time.Duration
	BackoffMin time.Duration
	BackoffMax time.Duration
	MaxTries   int
	// StableAfter is how long a connection must stay live before the retry
	// count resets, so a flapping connection cannot retry forever.
	StableAfter time.Duration
	Now         func() time.Time
	// Sleep waits d or returns ctx's error. Tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (c Config) withDefaults() Config {
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 10 * time.Second
	}
	if c.IdleClose <= 0 {
		c.IdleClose = IdleClose
	}
	if c.BackoffMin <= 0 {
		c.BackoffMin = BackoffMin
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = BackoffMax
	}
	if c.MaxTries <= 0 {
		c.MaxTries = MaxReconnectTries
	}
	if c.StableAfter <= 0 {
		c.StableAfter = c.BackoffMax
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Sleep == nil {
		c.Sleep = sleepCtx
	}
	return c
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// State is where a topic's connection stands.
type State string

const (
	StateConnecting   State = "connecting"
	StateLive         State = "live"
	StateReconnecting State = "reconnecting"
	// StateRefused: the broker refused the topic; no retry until a viewer asks again.
	StateRefused State = "refused"
	// StateFailed: reconnect tries ran out; no retry until a viewer asks again.
	StateFailed State = "failed"
	StateClosed State = "closed"
)

// Message is one buffered frame. Body holds at most MaxBodyBytes; Size is the
// true body length.
type Message struct {
	Seq uint64
	// Destination is the topic the frame arrived on, which differs from the
	// watched name when that name is a wildcard.
	Destination string
	Received    time.Time
	Size        int
	Body        []byte
	Truncated   bool
}

// Status is a connection state change with the reason a viewer can show.
type Status struct {
	State   State
	Reason  string
	Attempt int
	Retry   time.Duration
}

// EventKind separates frames from state changes on a viewer's channel.
type EventKind string

const (
	EventMessage EventKind = "message"
	EventStatus  EventKind = "status"
)

// Event is one item of a viewer's live feed.
type Event struct {
	Kind    EventKind
	Time    time.Time
	Message Message
	Status  Status
}

// TopicInfo describes one watched topic.
type TopicInfo struct {
	Name     string
	State    State
	Viewers  int
	Buffered int
}

const viewerBuffer = 256

// MaxViewersPerTopic bounds the memory a topic can pin: each viewer holds a
// 256-event buffer.
const MaxViewersPerTopic = 16

var ErrTooManyViewers = fmt.Errorf("busmonitor: at most %d viewers per topic", MaxViewersPerTopic)

// Monitor owns the watched topics. All state sits under one mutex; viewer
// sends never block under it.
type Monitor struct {
	cfg Config

	// base is the lifetime owner for topic workers; Close cancels it.
	base   context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	topics map[string]*topic
	closed bool
	wg     sync.WaitGroup
}

type topic struct {
	name    string
	ring    []Message
	seq     uint64
	viewers map[*Viewer]struct{}
	status  Status
	cancel  context.CancelFunc
	running bool
	idle    *time.Timer
	idleGen uint64
}

// New starts an empty Monitor whose workers end when ctx is cancelled or Close is called.
func New(ctx context.Context, cfg Config) *Monitor {
	base, cancel := context.WithCancel(ctx)
	return &Monitor{cfg: cfg.withDefaults(), base: base, cancel: cancel, topics: make(map[string]*topic)}
}

// Viewer is one watcher of one topic.
type Viewer struct {
	m       *Monitor
	t       *topic
	events  chan Event
	backlog []Message
	status  Status
	closed  bool
	err     error
}

// Backlog is the buffered messages at the moment the viewer joined, oldest first.
func (v *Viewer) Backlog() []Message { return v.backlog }

// Status is the topic's state at the moment the viewer joined.
func (v *Viewer) Status() Status { return v.status }

// Events delivers live messages and status changes. It is closed by Close,
// Monitor.Close, or when the viewer falls behind (see Err).
func (v *Viewer) Events() <-chan Event { return v.events }

// Err says why Events was closed by the monitor, or nil.
func (v *Viewer) Err() error {
	v.m.mu.Lock()
	defer v.m.mu.Unlock()
	return v.err
}

// Close detaches the viewer. The topic's connection closes IdleClose after its last viewer leaves.
func (v *Viewer) Close() {
	m := v.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if v.closed {
		return
	}
	v.closed = true
	close(v.events)
	delete(v.t.viewers, v)
	m.armIdleLocked(v.t)
}

// Watch validates name and attaches a viewer, opening the topic's dedicated
// connection if this is its first viewer.
func (m *Monitor) Watch(name string) (*Viewer, error) {
	probe := ""
	if m.cfg.Probe != nil {
		probe = m.cfg.Probe()
	}
	if err := ValidateTopic(name, probe); err != nil {
		return nil, err
	}
	return m.attach(name)
}

// attach is Watch after validation. It is separate so the live test can hand
// the broker a name the validator would refuse and observe the isolation.
func (m *Monitor) attach(name string) (*Viewer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	t, ok := m.topics[name]
	if !ok {
		if len(m.topics) >= MaxTopics {
			return nil, ErrTooManyTopics
		}
		t = &topic{name: name, viewers: make(map[*Viewer]struct{}), status: Status{State: StateConnecting}}
		m.topics[name] = t
	}
	if len(t.viewers) >= MaxViewersPerTopic {
		return nil, ErrTooManyViewers
	}
	if t.idle != nil {
		t.idle.Stop()
		t.idle = nil
		t.idleGen++
	}
	if !t.running {
		t.status = Status{State: StateConnecting}
		m.startLocked(t)
	}
	v := &Viewer{
		m:       m,
		t:       t,
		events:  make(chan Event, viewerBuffer),
		backlog: append([]Message(nil), t.ring...),
		status:  t.status,
	}
	t.viewers[v] = struct{}{}
	return v, nil
}

// Topics lists the watched topics sorted by name.
func (m *Monitor) Topics() []TopicInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TopicInfo, 0, len(m.topics))
	for _, t := range m.topics {
		out = append(out, TopicInfo{Name: t.name, State: t.status.State, Viewers: len(t.viewers), Buffered: len(t.ring)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Close stops every worker, closes every connection and every viewer, and waits.
func (m *Monitor) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	for _, t := range m.topics {
		if t.idle != nil {
			t.idle.Stop()
			t.idleGen++
		}
		for v := range t.viewers {
			v.closed = true
			v.err = ErrClosed
			close(v.events)
		}
		t.viewers = map[*Viewer]struct{}{}
	}
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
}

func (m *Monitor) startLocked(t *topic) {
	ctx, cancel := context.WithCancel(m.base)
	t.cancel = cancel
	t.running = true
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.run(ctx, t)
		cancel()
	}()
}

// armIdleLocked starts the close timer when the last viewer has left.
func (m *Monitor) armIdleLocked(t *topic) {
	if len(t.viewers) > 0 || m.closed || t.idle != nil {
		return
	}
	t.idleGen++
	gen := t.idleGen
	t.idle = time.AfterFunc(m.cfg.IdleClose, func() { m.idleExpired(t, gen) })
}

func (m *Monitor) idleExpired(t *topic, gen uint64) {
	m.mu.Lock()
	if t.idleGen != gen || len(t.viewers) > 0 || m.closed {
		m.mu.Unlock()
		return
	}
	t.idle = nil
	if m.topics[t.name] == t {
		delete(m.topics, t.name)
	}
	cancel := t.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// finish records the terminal status and marks the worker stopped in one step,
// so a Watch that sees the terminal state also sees that a restart is due.
func (m *Monitor) finish(t *topic, st Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t.status = st
	t.running = false
	m.broadcastLocked(t, Event{Kind: EventStatus, Time: m.cfg.Now(), Status: st})
}

// setStatus records st and tells every viewer.
func (m *Monitor) setStatus(t *topic, st Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t.status = st
	m.broadcastLocked(t, Event{Kind: EventStatus, Time: m.cfg.Now(), Status: st})
}

func (m *Monitor) record(t *topic, dest string, body []byte) {
	msg := Message{Received: m.cfg.Now(), Destination: cleanReason(dest), Size: len(body)}
	if len(body) > MaxBodyBytes {
		msg.Body = append([]byte(nil), body[:MaxBodyBytes]...)
		msg.Truncated = true
	} else {
		msg.Body = append([]byte(nil), body...)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t.seq++
	msg.Seq = t.seq
	if len(t.ring) >= RingSize {
		copy(t.ring, t.ring[1:])
		t.ring = t.ring[:RingSize-1]
	}
	t.ring = append(t.ring, msg)
	m.broadcastLocked(t, Event{Kind: EventMessage, Time: msg.Received, Message: msg})
}

// broadcastLocked never blocks: a viewer whose buffer is full is closed with
// ErrSlowViewer rather than stalling the topic's reader.
func (m *Monitor) broadcastLocked(t *topic, ev Event) {
	for v := range t.viewers {
		select {
		case v.events <- ev:
		default:
			v.closed = true
			v.err = ErrSlowViewer
			close(v.events)
			delete(t.viewers, v)
			m.armIdleLocked(t)
		}
	}
}

// run is one topic's worker: connect, subscribe, pump, and reconnect with
// backoff until refused, out of tries, or cancelled.
func (m *Monitor) run(ctx context.Context, t *topic) {
	attempt := 0
	delay := m.cfg.BackoffMin
	for {
		liveSince, err := m.session(ctx, t)
		if ctx.Err() != nil {
			m.finish(t, Status{State: StateClosed})
			return
		}
		if reason, refused := refusal(err); refused {
			m.finish(t, Status{State: StateRefused, Reason: reason})
			return
		}
		if !liveSince.IsZero() && m.cfg.Now().Sub(liveSince) >= m.cfg.StableAfter {
			attempt = 0
			delay = m.cfg.BackoffMin
		}
		attempt++
		reason := cleanReason(err.Error())
		if attempt > m.cfg.MaxTries {
			m.finish(t, Status{State: StateFailed, Reason: fmt.Sprintf("gave up after %d reconnect tries: %s", m.cfg.MaxTries, reason), Attempt: attempt - 1})
			return
		}
		m.setStatus(t, Status{State: StateReconnecting, Reason: reason, Attempt: attempt, Retry: delay})
		if m.cfg.Sleep(ctx, delay) != nil {
			m.finish(t, Status{State: StateClosed})
			return
		}
		delay = min(delay*2, m.cfg.BackoffMax)
	}
}

// session runs one connection to its end and returns when it went live (zero
// if it never did) and why it ended. The connection is closed before return.
func (m *Monitor) session(ctx context.Context, t *topic) (time.Time, error) {
	dctx, cancel := context.WithTimeout(ctx, m.cfg.ConnectTimeout)
	sess, err := m.cfg.Dial(dctx)
	cancel()
	if err != nil {
		return time.Time{}, fmt.Errorf("connect: %w", err)
	}
	defer func() {
		if cerr := sess.Close(); cerr != nil {
			log.Printf("busmonitor: close connection for %s: %v", t.name, cerr)
		}
	}()

	sub, err := sess.Subscribe(ctx, t.name)
	if err != nil {
		return time.Time{}, err
	}
	liveSince := m.cfg.Now()
	m.setStatus(t, Status{State: StateLive})
	for {
		select {
		case <-ctx.Done():
			return liveSince, ctx.Err()
		case msg, ok := <-sub.Messages():
			if !ok {
				if serr := sub.Err(); serr != nil {
					return liveSince, serr
				}
				return liveSince, errors.New("subscription ended without a reason")
			}
			m.record(t, msg.Destination, msg.Body)
		}
	}
}

// refusal reports whether err is a broker ERROR frame, and the broker's reason.
// go-stomp builds that error as *stomp.Error carrying the frame; every
// transport drop is a different shape.
func refusal(err error) (string, bool) {
	var se *stomp.Error
	if !errors.As(err, &se) || se == nil || se.Frame == nil || se.Frame.Command != frame.ERROR {
		return "", false
	}
	// go-stomp also builds ERROR frames itself for local failures ("connection
	// closed", "write channel closed"): one message header, no body. A frame
	// from the broker carries more, so the bare shape is a drop, not a refusal.
	if len(se.Frame.Body) == 0 && se.Frame.Header.Len() <= 1 {
		return "", false
	}
	reason := se.Message
	if body := strings.TrimSpace(string(se.Frame.Body)); body != "" {
		reason += ": " + body
	}
	return cleanReason(reason), true
}

// cleanReason makes broker text safe to show: control bytes become spaces, non-ASCII bytes become '?', and
// the length is capped.
func cleanReason(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s) && len(b) < maxReasonBytes; i++ {
		c := s[i]
		switch {
		case c < 0x20 || c == 0x7f:
			c = ' '
		case c >= 0x80:
			c = '?'
		}
		b = append(b, c)
	}
	return strings.TrimSpace(string(b))
}
