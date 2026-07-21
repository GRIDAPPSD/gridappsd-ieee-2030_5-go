package cimstomp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// These tests cover the deterministic, broker-free parts of cimstomp.Client:
// queue-prefix normalization, header construction, sentinel errors, the
// not-connected guard, and Close idempotency. Integration with a live STOMP
// broker is covered in client_integration_test.go behind the `integration`
// build tag.

func TestNormalizeDestination(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare goss form gets queue prefix",
			"goss.gridappsd.process.request.data.powergridmodel",
			"/queue/goss.gridappsd.process.request.data.powergridmodel"},
		{"already prefixed queue form is preserved",
			"/queue/goss.gridappsd.process.request.config",
			"/queue/goss.gridappsd.process.request.config"},
		{"topic destination is preserved",
			"/topic/pnnl.goss.token.topic",
			"/topic/pnnl.goss.token.topic"},
		{"temp-queue destination is preserved",
			"/temp-queue/response.123",
			"/temp-queue/response.123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeDestination(tc.in)
			if got != tc.want {
				t.Errorf("normalizeDestination(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewTempReplyDest_Format(t *testing.T) {
	dest := newTempReplyDest()
	if !strings.HasPrefix(dest, "/temp-queue/response.") {
		t.Fatalf("temp reply dest %q does not start with /temp-queue/response.", dest)
	}
	suffix := strings.TrimPrefix(dest, "/temp-queue/response.")
	if suffix == "" {
		t.Fatalf("temp reply dest %q has empty suffix", dest)
	}
}

func TestNewTempReplyDest_Unique(t *testing.T) {
	// Two consecutive calls must produce different destinations so the broker
	// gives each request its own correlation queue.
	a := newTempReplyDest()
	b := newTempReplyDest()
	if a == b {
		t.Fatalf("two calls to newTempReplyDest returned the same value: %q", a)
	}
}

func TestRequest_NotConnected(t *testing.T) {
	c := NewClient(STOMPConfig{Address: "tcp://127.0.0.1:1", User: "u", Password: "p"})

	_, err := c.Request(context.Background(), "/queue/foo", []byte("{}"))
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Request before Connect: got err = %v, want ErrNotConnected", err)
	}
}

func TestRequest_ContextAlreadyCancelled(t *testing.T) {
	// A cancelled context must short-circuit Request without touching the wire,
	// surfacing ctx.Err().
	c := NewClient(STOMPConfig{Address: "tcp://127.0.0.1:1"})
	c.markConnectedForTest()
	defer c.unmarkConnectedForTest()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Request(ctx, "/queue/foo", []byte("{}"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Request with cancelled ctx: got err = %v, want context.Canceled", err)
	}
}

func TestRequest_DeadlineExceededReturnsTimeout(t *testing.T) {
	// A deadline that has already passed should yield ErrRequestTimeout
	// without dialing.
	c := NewClient(STOMPConfig{Address: "tcp://127.0.0.1:1"})
	c.markConnectedForTest()
	defer c.unmarkConnectedForTest()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := c.Request(ctx, "/queue/foo", []byte("{}"))
	if !errors.Is(err, ErrRequestTimeout) {
		t.Fatalf("Request with expired deadline: got err = %v, want ErrRequestTimeout", err)
	}
}

func TestClose_IdempotentBeforeConnect(t *testing.T) {
	c := NewClient(STOMPConfig{Address: "tcp://127.0.0.1:1"})
	if err := c.Close(); err != nil {
		t.Fatalf("first Close on never-connected client: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close on never-connected client: %v", err)
	}
}

// TestClose_ZeroesCachedPassword locks in the GAGO-015 hardening: Close is
// terminal (Connect/Reconnect cannot run afterward), so it is safe to zero
// c.cfg.Password symmetrically with the existing c.token clearing. Both
// zeroings are best-effort defense-in-depth per the Close doc comment, not
// a live-exposure fix.
func TestClose_ZeroesCachedPassword(t *testing.T) {
	c := NewClient(STOMPConfig{Address: "tcp://127.0.0.1:1", User: "u", Password: "s3cr3t"})
	if err := c.Close(); err != nil {
		t.Fatalf("Close on never-connected client: %v", err)
	}
	if c.cfg.Password != "" {
		t.Errorf("Close: cfg.Password = %q, want empty after Close", c.cfg.Password)
	}
}

func TestSentinelErrors_AreDistinct(t *testing.T) {
	if errors.Is(ErrRequestTimeout, ErrNotConnected) {
		t.Errorf("ErrRequestTimeout must not match ErrNotConnected")
	}
	if errors.Is(ErrNotConnected, ErrRequestTimeout) {
		t.Errorf("ErrNotConnected must not match ErrRequestTimeout")
	}
	if errors.Is(ErrClosed, ErrNotConnected) {
		t.Errorf("ErrClosed must not match ErrNotConnected")
	}
	if errors.Is(ErrClosed, ErrRequestTimeout) {
		t.Errorf("ErrClosed must not match ErrRequestTimeout")
	}
}

// TestConnect_AfterCloseReturnsErrClosed locks in the contract that a Client
// is single-shot: once Close has been called, Connect must refuse and return
// ErrClosed rather than silently re-arming the connected flag (review item D).
func TestConnect_AfterCloseReturnsErrClosed(t *testing.T) {
	c := NewClient(STOMPConfig{Address: "tcp://127.0.0.1:1", User: "u", Password: "p"})
	if err := c.Close(); err != nil {
		t.Fatalf("initial Close on never-connected client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := c.Connect(ctx)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Connect after Close: got err = %v, want ErrClosed", err)
	}
}

// TestPublisher_PublishNotConnectedSharesSentinel verifies item A: Publisher
// reuses the package-level ErrNotConnected sentinel rather than its own
// fmt.Errorf string.
func TestPublisher_PublishNotConnectedSharesSentinel(t *testing.T) {
	p := New(STOMPConfig{Address: "tcp://127.0.0.1:1", User: "u", Password: "p"})
	err := p.Publish(&PointMessage{Topic: "/topic/x", MRID: "m"})
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Publish before Connect: got err = %v, want ErrNotConnected", err)
	}
}

// TestPublisher_CloseBeforeConnectReturnsNil verifies that Close on a never-
// connected Publisher is a no-op, matching Client.Close's contract.
func TestPublisher_CloseBeforeConnectReturnsNil(t *testing.T) {
	p := New(STOMPConfig{Address: "tcp://127.0.0.1:1"})
	if err := p.Close(); err != nil {
		t.Fatalf("Close on never-connected Publisher: %v", err)
	}
}

// TestPublisher_ConnectContextAlreadyCancelled verifies that Connect on a
// Publisher with a cancelled context returns the ctx error before any
// network I/O.
func TestPublisher_ConnectContextAlreadyCancelled(t *testing.T) {
	p := New(STOMPConfig{Address: "127.0.0.1:1"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := p.Connect(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect with cancelled ctx: got %v, want context.Canceled", err)
	}
}

// TestConnect_ContextAlreadyCancelled verifies that Client.Connect with a
// cancelled context surfaces ctx.Err() before any TCP dial.
func TestConnect_ContextAlreadyCancelled(t *testing.T) {
	c := NewClient(STOMPConfig{Address: "127.0.0.1:1"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.Connect(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect with cancelled ctx: got %v, want context.Canceled", err)
	}
}

// TestNewCorrelationID_FormatAndUnique verifies that newCorrelationID
// returns a 32-char hex string and that two calls do not collide. The
// header is defense-in-depth (Leon M2 / GAGO-013 item G).
func TestNewCorrelationID_FormatAndUnique(t *testing.T) {
	a, err := newCorrelationID()
	if err != nil {
		t.Fatalf("newCorrelationID: %v", err)
	}
	if len(a) != 32 {
		t.Errorf("correlation id length = %d, want 32", len(a))
	}
	for _, ch := range a {
		if !(ch >= '0' && ch <= '9') && !(ch >= 'a' && ch <= 'f') {
			t.Errorf("correlation id %q contains non-hex char %q", a, ch)
			break
		}
	}
	b, err := newCorrelationID()
	if err != nil {
		t.Fatalf("newCorrelationID #2: %v", err)
	}
	if a == b {
		t.Errorf("two newCorrelationID calls returned the same value: %q", a)
	}
}

// TestLogDisconnectErr_NilIsNoOp verifies the helper does not panic when
// called with a nil error.
func TestLogDisconnectErr_NilIsNoOp(t *testing.T) {
	logDisconnectErr(nil, "TestLogDisconnectErr_NilIsNoOp")
}

// TestNewTokenReplyDest_FormatAndUnique covers the bootstrap reply-queue
// constructor; the integration tests exercise it but the unit tests did
// not, leaving 0% coverage post-cleanup.
func TestNewTokenReplyDest_FormatAndUnique(t *testing.T) {
	a := newTokenReplyDest("admin")
	if !strings.HasPrefix(a, "/queue/temp.token_resp.admin.") {
		t.Errorf("token reply dest %q missing expected prefix", a)
	}
	b := newTokenReplyDest("admin")
	if a == b {
		t.Errorf("two calls returned the same dest: %q", a)
	}
}
