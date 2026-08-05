package cimstomp

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestPublisherConnect_DoesNotRequestInboundHeartbeats is the Publisher
// counterpart of TestConnect_DoesNotRequestInboundHeartbeats
// (reconnect_test.go). Publisher.Connect (publisher.go) had the identical
// defect Client.dialAndBootstrap had before its fix: a symmetric
// stomp.ConnOpt.HeartBeat(heartbeat, heartbeat) request. It
// must promise outbound heartbeats and request ZERO inbound ones, the
// same wire-level contract as the Client dial site, so this asserts the
// same header shape.
func TestPublisherConnect_DoesNotRequestInboundHeartbeats(t *testing.T) {
	broker := startCountingFakeBroker(t)
	defer broker.Stop()

	p := New(STOMPConfig{Address: broker.Addr(), User: "u", Password: "p"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := p.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = p.Close() }()

	// STOMP encodes heart-beat as "<send>,<receive>" in milliseconds.
	// send is our promise, receive is what we demand of the broker.
	want := fmt.Sprintf("%d,0", heartbeat.Milliseconds())
	if got := broker.LastHeartBeat(); got != want {
		t.Errorf("CONNECT heart-beat header = %q, want %q (a non-zero receive interval arms go-stomp's silent read deadline)", got, want)
	}
}

// TestIdleConnection_ClosesWithDisconnectFrame_BothDialSites is the
// behavioural guard for the symmetric-heartbeat defect across BOTH dial sites, sharing a
// single idle sleep instead of duplicating
// TestIdleConnection_ClosesWithDisconnectFrame (reconnect_test.go) for
// the Publisher alone. That test's idle window already pushed this
// package's suite runtime from ~1.2s to ~19.9s; a second independent
// idle sleep of the same length would roughly double that cost for no
// extra coverage, since both dial sites share the same `heartbeat`
// const and therefore the same deadline math. Each dial site still gets
// its own fake broker so the two connections cannot interact; only the
// time.Sleep is shared.
//
// Before the Publisher fix this failed with pubBroker's
// CONNECT=1, DISCONNECT=0 while Publisher.Close returned nil: the same
// silent-session-leak shape traced on the client side, just on
// the publish leg of the bus instead of the request/reply leg.
func TestIdleConnection_ClosesWithDisconnectFrame_BothDialSites(t *testing.T) {
	idle := heartbeat + 5*time.Second + 2*time.Second

	clientBroker := startCountingFakeBroker(t)
	defer clientBroker.Stop()
	pubBroker := startCountingFakeBroker(t)
	defer pubBroker.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), idle+30*time.Second)
	defer cancel()

	c := NewClient(STOMPConfig{Address: clientBroker.Addr(), User: "u", Password: "p"})
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Client Connect: %v", err)
	}
	if got := clientBroker.ConnectCount(); got != 1 {
		t.Fatalf("client CONNECT count after Connect = %d, want 1", got)
	}

	p := New(STOMPConfig{Address: pubBroker.Addr(), User: "u", Password: "p"})
	if err := p.Connect(ctx); err != nil {
		t.Fatalf("Publisher Connect: %v", err)
	}
	if got := pubBroker.ConnectCount(); got != 1 {
		t.Fatalf("publisher CONNECT count after Connect = %d, want 1", got)
	}

	time.Sleep(idle)

	if err := c.Close(); err != nil {
		t.Fatalf("Client Close after %s idle: %v", idle, err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Publisher Close after %s idle: %v", idle, err)
	}

	// Close returns once go-stomp has the DISCONNECT receipt, but each
	// broker's per-conn goroutine counts the frame on its own schedule.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if clientBroker.DisconnectCount() == clientBroker.ConnectCount() &&
			pubBroker.DisconnectCount() == pubBroker.ConnectCount() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got, want := clientBroker.DisconnectCount(), clientBroker.ConnectCount(); got != want {
		t.Errorf("client: after %s idle: DISCONNECT=%d, CONNECT=%d (want equal); the connection died without a DISCONNECT frame and Close reported success anyway", idle, got, want)
	}
	if got, want := pubBroker.DisconnectCount(), pubBroker.ConnectCount(); got != want {
		t.Errorf("publisher: after %s idle: DISCONNECT=%d, CONNECT=%d (want equal); the connection died without a DISCONNECT frame and Close reported success anyway", idle, got, want)
	}
}
