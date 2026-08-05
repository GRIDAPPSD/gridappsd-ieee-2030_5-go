//go:build integration

// Integration tests for cimstomp.Client.Reconnect. Use the same
// fakeServer harness as client_integration_test.go: a STOMP peer that
// plays the GridAPPS-D role of token responder plus request handler.
//
// Bring the broker up with `docker compose up -d` and run with
// `go test -tags=integration -race ./internal/cimstomp/`.

package cimstomp

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestIntegration_Reconnect_HappyPath connects, reconnects, and verifies
// the Client is usable for Request after the Reconnect.
func TestIntegration_Reconnect_HappyPath(t *testing.T) {
	requireBroker(t)

	const reqQueue = "/queue/goss.gridappsd.process.request.data.reconnect.happy"
	fs := startFakeServer(t, "tok-happy", reqQueue, []byte(`{"ok":true}`))
	defer fs.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	if err := c.Reconnect(ctx); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}

	// Drain any leftover request frames from before the reconnect.
	for {
		select {
		case <-fs.requests:
		default:
			goto drained
		}
	}
drained:

	if _, err := c.Request(ctx, "goss.gridappsd.process.request.data.reconnect.happy", []byte(`{}`)); err != nil {
		t.Fatalf("Request after Reconnect: %v", err)
	}
}

// TestIntegration_Reconnect_RefreshesToken verifies the security
// invariant: every Reconnect refetches the auth
// token; reuse across reconnects is forbidden. The fakeServer rotates
// the token it serves on each token-topic SEND so the assertion is
// straightforward.
func TestIntegration_Reconnect_RefreshesToken(t *testing.T) {
	requireBroker(t)

	const reqQueue = "/queue/goss.gridappsd.process.request.data.reconnect.refresh"
	fs := startRotatingTokenServer(t, "tok-rot", reqQueue, []byte(`{}`))
	defer fs.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	tokenA := c.tokenForTest()
	if tokenA == "" {
		t.Fatalf("token after Connect is empty")
	}

	if err := c.Reconnect(ctx); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}

	tokenB := c.tokenForTest()
	if tokenB == "" {
		t.Fatalf("token after Reconnect is empty")
	}
	if tokenA == tokenB {
		t.Fatalf("token reused across Reconnect: %q (Reconnect must refetch)", tokenA)
	}
}

// TestIntegration_Reconnect_AfterClose verifies the lifecycle terminal
// state: Close is final, Reconnect after Close yields ErrClosed.
func TestIntegration_Reconnect_AfterClose(t *testing.T) {
	requireBroker(t)
	fs := startFakeServer(t, "tok-close", "/queue/goss.gridappsd.process.request.data.reconnect.close", []byte("{}"))
	defer fs.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Reconnect(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("Reconnect after Close: got %v, want ErrClosed", err)
	}
}

// TestIntegration_CloseAfterReconnect verifies that Close cleanly tears
// down the connection installed by Reconnect (no leaked broker session).
func TestIntegration_CloseAfterReconnect(t *testing.T) {
	requireBroker(t)
	fs := startFakeServer(t, "tok-close-after", "/queue/goss.gridappsd.process.request.data.reconnect.closeafter", []byte("{}"))
	defer fs.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := c.Reconnect(ctx); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close after Reconnect: %v", err)
	}
}

// TestIntegration_ConcurrentReconnect_SecondObservesFirst verifies that
// two concurrent Reconnect calls do not stomp each other's state. The
// mutex serializes them; both succeed (the second's bootstrap runs
// against the connection installed by the first plus a fresh dial).
func TestIntegration_ConcurrentReconnect_SecondObservesFirst(t *testing.T) {
	requireBroker(t)
	fs := startRotatingTokenServer(t, "tok-cc", "/queue/goss.gridappsd.process.request.data.reconnect.cc", []byte("{}"))
	defer fs.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	errs := make(chan error, 2)
	go func() { errs <- c.Reconnect(ctx) }()
	go func() { errs <- c.Reconnect(ctx) }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Reconnect[%d]: %v", i, err)
		}
	}
	if c.tokenForTest() == "" {
		t.Fatalf("token empty after concurrent Reconnect")
	}
}

// TestIntegration_RequestAfterReconnect_UsesNewToken verifies that the
// request issued after Reconnect carries the new token in the
// GOSS_SUBJECT header, not the pre-Reconnect one.
func TestIntegration_RequestAfterReconnect_UsesNewToken(t *testing.T) {
	requireBroker(t)
	const reqQueue = "/queue/goss.gridappsd.process.request.data.reconnect.gosshdr"
	fs := startRotatingTokenServer(t, "tok-hdr", reqQueue, []byte(`{}`))
	defer fs.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	if err := c.Reconnect(ctx); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	wantToken := c.tokenForTest()

	// Drain any leftover request frames recorded by fs from before the
	// reconnect; we only care about the post-Reconnect Request.
	for {
		select {
		case <-fs.requests:
		default:
			goto drained
		}
	}
drained:

	if _, err := c.Request(ctx, "goss.gridappsd.process.request.data.reconnect.gosshdr", []byte(`{}`)); err != nil {
		t.Fatalf("Request: %v", err)
	}

	select {
	case rec := <-fs.requests:
		if got := rec.headers["GOSS_SUBJECT"]; got != wantToken {
			t.Fatalf("GOSS_SUBJECT after Reconnect = %q, want refreshed token %q", got, wantToken)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("fakeServer did not record a request within timeout")
	}
}

// rotatingTokenServer is a fakeServer variant that serves a fresh token
// value on every token-topic request. Used to assert Reconnect refetches.
type rotatingTokenServer struct {
	*fakeServer
	tokenSeq atomic.Int64
}

func startRotatingTokenServer(t *testing.T, prefix, requestQueue string, responseBody []byte) *rotatingTokenServer {
	t.Helper()
	fs := startFakeServer(t, prefix+"-0", requestQueue, responseBody)
	rs := &rotatingTokenServer{fakeServer: fs}
	rs.fakeServer.tokenProvider = func() string {
		n := rs.tokenSeq.Add(1)
		return prefixWithSeq(prefix, n)
	}
	return rs
}

func prefixWithSeq(prefix string, n int64) string {
	return prefix + "-" + intToString(n)
}

func intToString(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
