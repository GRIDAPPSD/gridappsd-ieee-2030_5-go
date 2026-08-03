package cimstomp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
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

// countingFakeBroker is an in-process STOMP fake. It speaks just enough
// of STOMP to support Client.Connect and Client.Reconnect: CONNECT,
// SUBSCRIBE (token bootstrap reply queue), SEND (token-topic auth), and
// DISCONNECT. It counts CONNECT frames received and DISCONNECT frames
// received so a test can assert that every CONNECT issued by the Client
// is matched by a DISCONNECT before the broker session leaks.
//
// The fake does NOT speak Request/Reply on real queues. Tests that need
// Request/Reply use the live-broker fakeServer in
// client_integration_test.go. This fake exists for the session-leak
// regression test (Dutch C1 / Leon H1) which only exercises the
// connect-bootstrap-disconnect lifecycle.
type countingFakeBroker struct {
	ln      net.Listener
	addr    string
	stop    chan struct{}
	wg      sync.WaitGroup
	closeMu sync.Mutex
	closed  bool

	connectCount    atomic.Int64
	disconnectCount atomic.Int64

	// lastHeartBeat records the `heart-beat` header of the most recent
	// CONNECT frame received, so a test can assert what the client
	// actually put on the wire rather than inferring it from behaviour.
	lastHeartBeat atomic.Value

	tokenSeq atomic.Int64
}

func startCountingFakeBroker(t *testing.T) *countingFakeBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	b := &countingFakeBroker{
		ln:   ln,
		addr: ln.Addr().String(),
		stop: make(chan struct{}),
	}
	b.wg.Add(1)
	go b.serve()
	return b
}

func (b *countingFakeBroker) Addr() string           { return b.addr }
func (b *countingFakeBroker) ConnectCount() int64    { return b.connectCount.Load() }
func (b *countingFakeBroker) DisconnectCount() int64 { return b.disconnectCount.Load() }

// LastHeartBeat returns the `heart-beat` header carried by the most
// recent CONNECT frame, or "" if the client sent none.
func (b *countingFakeBroker) LastHeartBeat() string {
	v, _ := b.lastHeartBeat.Load().(string)
	return v
}

func (b *countingFakeBroker) Stop() {
	b.closeMu.Lock()
	if b.closed {
		b.closeMu.Unlock()
		return
	}
	b.closed = true
	close(b.stop)
	_ = b.ln.Close()
	b.closeMu.Unlock()
	b.wg.Wait()
}

func (b *countingFakeBroker) serve() {
	defer b.wg.Done()
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			select {
			case <-b.stop:
			default:
			}
			return
		}
		b.wg.Add(1)
		go b.handleConn(conn)
	}
}

func (b *countingFakeBroker) handleConn(conn net.Conn) {
	defer b.wg.Done()
	defer conn.Close()

	br := bufio.NewReader(conn)

	// Read CONNECT (or STOMP) frame.
	cmd, connectHeaders, _, ok := b.readFrame(br)
	if !ok || (cmd != "CONNECT" && cmd != "STOMP") {
		return
	}
	b.lastHeartBeat.Store(connectHeaders["heart-beat"])
	b.connectCount.Add(1)

	// Send CONNECTED declining heartbeats in both directions, which is a
	// legal STOMP 1.2 answer and the one this fake gives so it never has
	// to run a heartbeat ticker. The client must honour that answer and
	// not run an inbound read deadline the broker never agreed to feed;
	// TestConnect_DoesNotRequestInboundHeartbeats and
	// TestIdleConnection_ClosesWithDisconnectFrame guard it.
	if _, err := io.WriteString(conn, "CONNECTED\nversion:1.2\nheart-beat:0,0\nserver:counting-fake\n\n\x00"); err != nil {
		return
	}

	// Drive the frame loop. We need to handle:
	//   - SUBSCRIBE on the token reply queue (record id keyed by destination)
	//   - SEND to /topic/pnnl.goss.token.topic (reply with a fresh token,
	//     dispatched to whichever subscription id matches the reply-to)
	//   - DISCONNECT (count it and exit)
	//   - Anything else: read and ignore so the client does not stall.
	subIDByDest := map[string]string{}
	for {
		cmd, headers, _, ok := b.readFrame(br)
		if !ok {
			return
		}
		switch cmd {
		case "SUBSCRIBE":
			dest := headers["destination"]
			id := headers["id"]
			if dest != "" && id != "" {
				subIDByDest[dest] = id
			}
		case "SEND":
			dest := headers["destination"]
			replyTo := headers["reply-to"]
			if dest == "/topic/pnnl.goss.token.topic" && replyTo != "" {
				token := fmt.Sprintf("counted-tok-%d", b.tokenSeq.Add(1))
				replyDest := replyTo
				if !strings.HasPrefix(replyDest, "/queue/") &&
					!strings.HasPrefix(replyDest, "/topic/") &&
					!strings.HasPrefix(replyDest, "/temp-queue/") {
					replyDest = "/queue/" + replyDest
				}
				// go-stomp's MESSAGE-frame dispatch keys on the
				// `subscription:<id>` header (conn.go:391). Without it,
				// the frame is logged and dropped. Look up the id by
				// destination; if the client subscribed via the bare
				// reply-to (Client.Subscribe normalizes to /queue/...)
				// either form is acceptable.
				subID := subIDByDest[replyDest]
				if subID == "" {
					subID = subIDByDest[replyTo]
				}
				if subID == "" {
					return
				}
				msgID := fmt.Sprintf("%d", b.tokenSeq.Load())
				frame := fmt.Sprintf(
					"MESSAGE\ndestination:%s\nsubscription:%s\nmessage-id:%s\ncontent-type:text/plain\ncontent-length:%d\n\n%s\x00",
					replyDest, subID, msgID, len(token), token,
				)
				if _, err := io.WriteString(conn, frame); err != nil {
					return
				}
			}
		case "UNSUBSCRIBE":
			// go-stomp's Unsubscribe blocks waiting for a RECEIPT
			// (subscription.go: ~30s default). Reply promptly so the
			// token bootstrap's defer returns and the next Reconnect
			// can proceed.
			if receipt := headers["receipt"]; receipt != "" {
				resp := fmt.Sprintf("RECEIPT\nreceipt-id:%s\n\n\x00", receipt)
				_, _ = io.WriteString(conn, resp)
			}
		case "DISCONNECT":
			b.disconnectCount.Add(1)
			receipt := headers["receipt"]
			if receipt != "" {
				resp := fmt.Sprintf("RECEIPT\nreceipt-id:%s\n\n\x00", receipt)
				_, _ = io.WriteString(conn, resp)
			}
			return
		default:
			// Unknown command: ignore but keep reading.
		}
	}
}

// readFrame reads one STOMP frame: command line, headers, body up to NUL.
func (b *countingFakeBroker) readFrame(br *bufio.Reader) (cmd string, headers map[string]string, body []byte, ok bool) {
	headers = map[string]string{}

	// Skip any leading EOL/heartbeat bytes between frames.
	for {
		bb, err := br.Peek(1)
		if err != nil {
			return "", nil, nil, false
		}
		if bb[0] == '\n' || bb[0] == '\r' {
			_, _ = br.ReadByte()
			continue
		}
		break
	}

	line, err := br.ReadString('\n')
	if err != nil {
		return "", nil, nil, false
	}
	cmd = strings.TrimRight(line, "\r\n")

	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return "", nil, nil, false
		}
		if h == "\n" || h == "\r\n" {
			break
		}
		h = strings.TrimRight(h, "\r\n")
		idx := strings.Index(h, ":")
		if idx < 0 {
			continue
		}
		headers[h[:idx]] = h[idx+1:]
	}

	bodyBuf, err := br.ReadBytes('\x00')
	if err != nil {
		return "", nil, nil, false
	}
	if len(bodyBuf) > 0 {
		body = bodyBuf[:len(bodyBuf)-1]
	}
	return cmd, headers, body, true
}

// TestReconnect_ConcurrentNoSessionLeak is the regression test for the
// session-leak race fixed in client.go (Dutch C1 / Leon H1). Two or more
// concurrent Reconnect calls each dial a fresh broker session. Without
// the fix, the goroutines race on the c.conn slot: each captures the
// other's just-installed conn as nil and never sends a DISCONNECT for
// it. The broker session leaks one per losing race.
//
// Assertion: across an entire Connect plus N concurrent Reconnect plus
// Close cycle, the count of CONNECT frames received by the broker must
// equal the count of DISCONNECT frames received. The fix makes this
// invariant hold even under contention; without the fix one or more
// CONNECTs lose their matching DISCONNECT and the count diverges.
func TestReconnect_ConcurrentNoSessionLeak(t *testing.T) {
	const goroutines = 5

	// Run the scenario several times so the scheduler explores
	// interleavings under -race.
	const iterations = 8

	for iter := 0; iter < iterations; iter++ {
		broker := startCountingFakeBroker(t)
		c := NewClient(STOMPConfig{Address: broker.Addr(), User: "u", Password: "p"})

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := c.Connect(ctx); err != nil {
			cancel()
			broker.Stop()
			t.Fatalf("iteration %d: Connect: %v", iter, err)
		}

		var wg sync.WaitGroup
		errs := make(chan error, goroutines)
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := c.Reconnect(ctx); err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			cancel()
			_ = c.Close()
			broker.Stop()
			t.Fatalf("iteration %d: concurrent Reconnect: %v", iter, err)
		}

		if err := c.Close(); err != nil {
			cancel()
			broker.Stop()
			t.Fatalf("iteration %d: Close: %v", iter, err)
		}
		cancel()

		// Drain any in-flight DISCONNECTs by giving the broker a brief
		// window to observe them; the listener goroutines run on accept
		// and process the DISCONNECT frame before the connection
		// closes, but the per-conn goroutine only increments the
		// counter after read returns.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if broker.DisconnectCount() == broker.ConnectCount() {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		gotConnects := broker.ConnectCount()
		gotDisconnects := broker.DisconnectCount()
		broker.Stop()

		// Invariant: every CONNECT must be matched by a DISCONNECT. A
		// session leak shows up as gotConnects > gotDisconnects.
		if gotConnects != gotDisconnects {
			t.Fatalf("iteration %d: session leak: CONNECT=%d, DISCONNECT=%d (want equal)", iter, gotConnects, gotDisconnects)
		}
		// Sanity: at minimum the initial Connect plus every successful
		// Reconnect issued one CONNECT, so the total must be at least
		// 1 + 1 (the final live conn that Close tore down). Some
		// concurrent Reconnects supersede each other but each still
		// dialed; expect goroutines+1 in total at most.
		if gotConnects < 2 {
			t.Fatalf("iteration %d: expected at least 2 CONNECTs (Connect + at least one Reconnect succeeded), got %d", iter, gotConnects)
		}
	}
}

// TestConnect_DoesNotRequestInboundHeartbeats asserts the wire-level
// contract that keeps an idle connection alive: the CONNECT frame must
// promise outbound heartbeats and request ZERO inbound ones.
//
// This is the fast guard for GAGO-112. Requesting a non-zero inbound
// interval makes go-stomp arm a read deadline of that interval plus its
// 5s DefaultHeartBeatError even when the broker answers `heart-beat:0,0`
// to decline heartbeats (conn.go:212-223). When that deadline expires,
// processLoop's `defer c.MustDisconnect()` closes the socket without
// sending DISCONNECT and marks the conn closed, so every later
// Disconnect returns a silent nil (conn.go:477-482): the broker session
// is leaked and our own teardown reports success. Asserting the header
// bytes catches a regression in milliseconds;
// TestIdleConnection_ClosesWithDisconnectFrame proves the consequence.
func TestConnect_DoesNotRequestInboundHeartbeats(t *testing.T) {
	broker := startCountingFakeBroker(t)
	defer broker.Stop()

	c := NewClient(STOMPConfig{Address: broker.Addr(), User: "u", Password: "p"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	// STOMP encodes heart-beat as "<send>,<receive>" in milliseconds.
	// send is our promise, receive is what we demand of the broker.
	want := fmt.Sprintf("%d,0", heartbeat.Milliseconds())
	if got := broker.LastHeartBeat(); got != want {
		t.Errorf("CONNECT heart-beat header = %q, want %q (a non-zero receive interval arms go-stomp's silent read deadline)", got, want)
	}
}

// TestIdleConnection_ClosesWithDisconnectFrame is the behavioural guard
// for the same defect: a connection left idle past the deadline that a
// symmetric heartbeat request used to arm must still be alive, and Close
// must put a real DISCONNECT frame on the wire.
//
// Before the fix this failed with CONNECT=1, DISCONNECT=0 while Close
// returned nil, which is the exact shape of the CI failure in
// TestReconnect_ConcurrentNoSessionLeak: any iteration that ran longer
// than the deadline lost connections silently and the leak assertion
// fired on a leak the production code had not caused.
//
// The idle window must exceed the old deadline (the requested inbound
// interval plus go-stomp's 5s DefaultHeartBeatError), which is what
// makes this test slow. It is kept because the failure it guards is
// silent in production: GAGO-107 is the same 15s read deadline killing
// the bridge's bus with nothing logged.
func TestIdleConnection_ClosesWithDisconnectFrame(t *testing.T) {
	idle := heartbeat + 5*time.Second + 2*time.Second

	broker := startCountingFakeBroker(t)
	defer broker.Stop()

	c := NewClient(STOMPConfig{Address: broker.Addr(), User: "u", Password: "p"})
	ctx, cancel := context.WithTimeout(context.Background(), idle+30*time.Second)
	defer cancel()

	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := broker.ConnectCount(); got != 1 {
		t.Fatalf("CONNECT count after Connect = %d, want 1", got)
	}

	time.Sleep(idle)

	if err := c.Close(); err != nil {
		t.Fatalf("Close after %s idle: %v", idle, err)
	}

	// Close returns once go-stomp has the DISCONNECT receipt, but the
	// broker's per-conn goroutine counts the frame on its own schedule.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if broker.DisconnectCount() == broker.ConnectCount() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got, want := broker.DisconnectCount(), broker.ConnectCount(); got != want {
		t.Errorf("after %s idle: DISCONNECT=%d, CONNECT=%d (want equal); the connection died without a DISCONNECT frame and Close reported success anyway", idle, got, want)
	}
}
