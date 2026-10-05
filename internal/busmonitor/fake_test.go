package busmonitor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp/cimstomptest"
	"github.com/go-stomp/stomp/v3"
	"github.com/go-stomp/stomp/v3/frame"
)

// subRec is one subscription a fake session handed out.
type subRec struct {
	dest string
	sess *fakeSession
	sub  *cimstomp.Subscription
	in   chan<- cimstomp.Message
}

// end finishes the subscription with cause, as the real listener would.
func (r *subRec) end(cause error) {
	cimstomptest.SetErr(r.sub, cause)
	close(r.in)
}

func (r *subRec) send(body []byte) { r.in <- cimstomp.Message{Body: body} }

type fakeBroker struct {
	dials   atomic.Int64
	dialErr atomic.Value // error or nil
	// refuse names destinations whose subscription ends at once with the error.
	refuse sync.Map
	mu     sync.Mutex
	subs   map[string]chan *subRec
	closed []*fakeSession
}

func newBroker() *fakeBroker { return &fakeBroker{subs: map[string]chan *subRec{}} }

func (b *fakeBroker) ch(dest string) chan *subRec {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.subs[dest]
	if !ok {
		c = make(chan *subRec, 64)
		b.subs[dest] = c
	}
	return c
}

func (b *fakeBroker) dial(context.Context) (Session, error) {
	b.dials.Add(1)
	if v := b.dialErr.Load(); v != nil {
		return nil, v.(error)
	}
	return &fakeSession{b: b}, nil
}

// next waits for the next subscription on dest.
func (b *fakeBroker) next(t *testing.T, dest string) *subRec {
	t.Helper()
	select {
	case r := <-b.ch(dest):
		return r
	case <-time.After(3 * time.Second):
		t.Fatalf("no subscription on %s within 3s", dest)
		return nil
	}
}

type fakeSession struct {
	b      *fakeBroker
	closed atomic.Bool
}

func (s *fakeSession) Subscribe(_ context.Context, dest string, _ ...cimstomp.SubscribeOption) (*cimstomp.Subscription, error) {
	sub, in := cimstomptest.NewSubscription()
	rec := &subRec{dest: dest, sess: s, sub: sub, in: in}
	if e, ok := s.b.refuse.Load(dest); ok {
		rec.end(e.(error))
	}
	s.b.ch(dest) <- rec
	return sub, nil
}

func (s *fakeSession) Close() error {
	s.closed.Store(true)
	return nil
}

func refusalErr(reason string) error {
	f := frame.New(frame.ERROR, frame.Message, reason)
	f.Body = []byte("detail\x00\x01body")
	return &stomp.Error{Message: reason, Frame: f}
}

func newTestMonitor(t *testing.T, b *fakeBroker, mod func(*Config)) *Monitor {
	t.Helper()
	cfg := Config{Dial: b.dial, Sleep: func(context.Context, time.Duration) error { return nil }}
	if mod != nil {
		mod(&cfg)
	}
	m := New(context.Background(), cfg)
	t.Cleanup(m.Close)
	return m
}

// waitStatus reads viewer events until a status in want arrives.
func waitStatus(t *testing.T, v *Viewer, want State) Status {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-v.Events():
			if !ok {
				t.Fatalf("events closed while waiting for %s (err %v)", want, v.Err())
			}
			if ev.Kind == EventStatus && ev.Status.State == want {
				return ev.Status
			}
		case <-deadline:
			t.Fatalf("no %s status within 3s", want)
		}
	}
}

// waitMessages reads n message events and returns them.
func waitMessages(t *testing.T, v *Viewer, n int) []Message {
	t.Helper()
	var out []Message
	deadline := time.After(3 * time.Second)
	for len(out) < n {
		select {
		case ev, ok := <-v.Events():
			if !ok {
				t.Fatalf("events closed after %d of %d messages (err %v)", len(out), n, v.Err())
			}
			if ev.Kind == EventMessage {
				out = append(out, ev.Message)
			}
		case <-deadline:
			t.Fatalf("got %d of %d messages within 3s", len(out), n)
		}
	}
	return out
}
