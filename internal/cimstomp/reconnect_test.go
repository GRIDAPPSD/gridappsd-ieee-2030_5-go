package cimstomp

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-stomp/stomp/v3"
)

// These tests cover GAGO-012: explicit Reconnect, TOCTOU fix on the
// Connect/Close lock state machine, the ErrConnectionLost sentinel,
// and the token-bootstrap queue drain. They run without a live broker
// where possible; the few cases that need a STOMP wire-format peer
// stand up an in-process listener.

// TestReconnect_AfterCloseReturnsErrClosed locks in the contract that
// Reconnect on a Closed Client refuses with ErrClosed (the security and
// lifecycle invariant from the GAGO-012 spec).
func TestReconnect_AfterCloseReturnsErrClosed(t *testing.T) {
	c := NewClient(STOMPConfig{Address: "127.0.0.1:1", User: "u", Password: "p"})
	if err := c.Close(); err != nil {
		t.Fatalf("initial Close: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := c.Reconnect(ctx)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Reconnect after Close: got err = %v, want ErrClosed", err)
	}
}

// TestReconnect_ContextAlreadyCancelled verifies that a cancelled context
// short-circuits Reconnect before any I/O.
func TestReconnect_ContextAlreadyCancelled(t *testing.T) {
	c := NewClient(STOMPConfig{Address: "127.0.0.1:1", User: "u", Password: "p"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.Reconnect(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconnect with cancelled ctx: got %v, want context.Canceled", err)
	}
}

// TestErrConnectionLost_Distinct keeps the new sentinel distinct from
// the existing sentinels so callers can errors.Is on each independently.
func TestErrConnectionLost_Distinct(t *testing.T) {
	if errors.Is(ErrConnectionLost, ErrNotConnected) {
		t.Errorf("ErrConnectionLost must not match ErrNotConnected")
	}
	if errors.Is(ErrConnectionLost, ErrRequestTimeout) {
		t.Errorf("ErrConnectionLost must not match ErrRequestTimeout")
	}
	if errors.Is(ErrConnectionLost, ErrClosed) {
		t.Errorf("ErrConnectionLost must not match ErrClosed")
	}
}

// TestWrapTransportErr_StompErrors verifies the helper wraps go-stomp's
// ErrAlreadyClosed and ErrClosedUnexpectedly as ErrConnectionLost so
// callers can detect transport failure with a single sentinel.
func TestWrapTransportErr_StompErrors(t *testing.T) {
	cases := []struct {
		name string
		in   error
	}{
		{"ErrAlreadyClosed", stomp.ErrAlreadyClosed},
		{"ErrClosedUnexpectedly", stomp.ErrClosedUnexpectedly},
		{"io.EOF", io.EOF},
		{"net.ErrClosed", net.ErrClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapTransportErr("test", tc.in)
			if !errors.Is(got, ErrConnectionLost) {
				t.Errorf("wrapTransportErr(%v) = %v, want errors.Is ErrConnectionLost", tc.in, got)
			}
			if !errors.Is(got, tc.in) {
				t.Errorf("wrapTransportErr(%v) = %v, want errors.Is %v", tc.in, got, tc.in)
			}
		})
	}
}

// TestWrapTransportErr_NonTransportPassesThrough verifies that errors
// not matching any transport sentinel are returned with the prefix and
// the cause but without the spurious ErrConnectionLost label.
func TestWrapTransportErr_NonTransportPassesThrough(t *testing.T) {
	other := errors.New("application-layer broker rejection")
	got := wrapTransportErr("test", other)
	if errors.Is(got, ErrConnectionLost) {
		t.Errorf("wrapTransportErr on non-transport error labelled ErrConnectionLost: %v", got)
	}
	if !errors.Is(got, other) {
		t.Errorf("wrapTransportErr did not chain underlying error: %v", got)
	}
}

// TestWrapTransportErr_NilReturnsNil confirms the helper is safe to call
// in the success path without a special-case at the call site.
func TestWrapTransportErr_NilReturnsNil(t *testing.T) {
	if got := wrapTransportErr("test", nil); got != nil {
		t.Errorf("wrapTransportErr(nil) = %v, want nil", got)
	}
}

// stallingListener accepts a single TCP connection, then never responds
// to the STOMP CONNECT frame. stomp.ConnectWithContext blocks reading
// the CONNECTED frame; a concurrent Close races against the ongoing
// handshake. The Connect attempt unblocks only when ctx expires or the
// connection is forcibly closed.
type stallingListener struct {
	ln       net.Listener
	addr     string
	accepted chan struct{}
	conns    []net.Conn
	mu       sync.Mutex
}

func newStallingListener(t *testing.T) *stallingListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	s := &stallingListener{
		ln:       ln,
		addr:     ln.Addr().String(),
		accepted: make(chan struct{}, 4),
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, conn)
			s.mu.Unlock()
			select {
			case s.accepted <- struct{}{}:
			default:
			}
			// Hold the connection open without writing CONNECTED. The
			// caller's stomp.ConnectWithContext blocks until ctx expires
			// or the conn is forcibly closed.
		}
	}()
	return s
}

func (s *stallingListener) Close() {
	_ = s.ln.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
}

// TestConnect_TOCTOU_CloseDuringDial verifies that a Close racing with
// Connect's STOMP handshake leaves the Client in a consistent state:
// either Connect returns ErrClosed and the just-dialed conn is cleaned
// up, or Connect returns its own context error. Either way, Close must
// not allow the Client to settle into "closed=true with a live conn"
// (Leon GAGO-013 review M-1, deferred to GAGO-012).
//
// The test runs the inner scenario many times under -race so the
// scheduler explores both interleavings. A pass under -race is the
// real assertion; the field-level invariant (conn==nil OR closed==true,
// never closed==true with conn live) is checked after each iteration.
func TestConnect_TOCTOU_CloseDuringDial(t *testing.T) {
	const iterations = 64

	for i := 0; i < iterations; i++ {
		s := newStallingListener(t)
		c := NewClient(STOMPConfig{Address: s.addr, User: "u", Password: "p"})

		// Connect on goroutine A; ctx bounds the test if the listener
		// stalls and Close happens to lose the race.
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		connectErr := make(chan error, 1)
		go func() {
			connectErr <- c.Connect(ctx)
		}()

		// Wait for the listener to accept so we know the dial passed
		// the TCP handshake and is now stuck in the STOMP frame layer.
		<-s.accepted

		// Close on goroutine B. This is the racing call.
		closeErr := c.Close()
		if closeErr != nil {
			t.Fatalf("iteration %d: Close: %v", i, closeErr)
		}

		// Force the stalled Connect to unblock by tearing down the
		// listener's accepted conn; otherwise the timeout drives it.
		s.Close()
		<-connectErr
		cancel()

		// Final-state invariant: closed must be true, conn must be nil,
		// connected must be false. The TOCTOU bug let conn outlive
		// closed=true with token still installed.
		c.mu.Lock()
		conn := c.conn
		token := c.token
		c.mu.Unlock()
		if !c.closed.Load() {
			t.Fatalf("iteration %d: closed flag not set after Close", i)
		}
		if c.connected.Load() {
			t.Fatalf("iteration %d: connected flag still true after Close", i)
		}
		if conn != nil {
			t.Fatalf("iteration %d: c.conn still non-nil after Close (TOCTOU regression)", i)
		}
		if token != "" {
			t.Fatalf("iteration %d: c.token = %q after Close, want empty", i, token)
		}
	}
}

// TestConcurrentReconnect_SerializesViaMutex verifies that two goroutines
// calling Reconnect on a Closed Client both observe ErrClosed without
// racing. The mutex contract: Reconnect is serialized with Connect and
// Close. The test exercises the cheapest path (Closed) so it does not
// need a live broker; the lock-acquire/release ordering is what we
// verify here under -race.
func TestConcurrentReconnect_SerializesViaMutex(t *testing.T) {
	c := NewClient(STOMPConfig{Address: "127.0.0.1:1", User: "u", Password: "p"})
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	const goroutines = 16
	var wg sync.WaitGroup
	var errClosedCount atomic.Int32
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := c.Reconnect(ctx)
			if errors.Is(err, ErrClosed) {
				errClosedCount.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := errClosedCount.Load(); int(got) != goroutines {
		t.Fatalf("Reconnect ErrClosed count = %d, want %d", got, goroutines)
	}
}

// TestRequest_BeforeConnect_NotErrConnectionLost verifies the not-connected
// path returns ErrNotConnected, not ErrConnectionLost, so callers can
// distinguish "you forgot to Connect" from "broker dropped".
func TestRequest_BeforeConnect_NotErrConnectionLost(t *testing.T) {
	c := NewClient(STOMPConfig{Address: "127.0.0.1:1"})
	_, err := c.Request(context.Background(), "/queue/foo", []byte("{}"))
	if errors.Is(err, ErrConnectionLost) {
		t.Fatalf("Request before Connect: got ErrConnectionLost, want ErrNotConnected")
	}
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Request before Connect: got %v, want ErrNotConnected", err)
	}
}

// TestDrainAndUnsubscribe_DrainsBufferedFrames verifies the helper
// drains the underlying stomp.Subscription channel before Unsubscribe so
// that any pending frames are consumed instead of left for the broker
// to redeliver. The test wires up a fake subscription (driven from the
// cimstomptest seam) and asserts the drain count.
func TestDrainAndUnsubscribe_DrainsBufferedFrames(t *testing.T) {
	// Use a real *stomp.Subscription channel via a synthetic stand-in
	// is not possible (the type is concrete). Instead, exercise the
	// generic drainStompChan helper which is what drainAndUnsubscribe
	// delegates to.
	ch := make(chan *stomp.Message, 4)
	ch <- &stomp.Message{Body: []byte("a")}
	ch <- &stomp.Message{Body: []byte("b")}
	ch <- &stomp.Message{Body: []byte("c")}

	got := drainStompChan(ch)
	if got != 3 {
		t.Fatalf("drainStompChan returned %d, want 3", got)
	}

	// The channel must still be usable; drain must not close it.
	select {
	case ch <- &stomp.Message{}:
	default:
		t.Fatalf("drainStompChan closed or filled the channel")
	}
}

// TestDrainAndUnsubscribe_EmptyChannel verifies the drain helper is a
// fast no-op on an empty channel; this is the common case post-success.
func TestDrainAndUnsubscribe_EmptyChannel(t *testing.T) {
	ch := make(chan *stomp.Message, 4)
	if got := drainStompChan(ch); got != 0 {
		t.Fatalf("drainStompChan empty channel = %d, want 0", got)
	}
}
