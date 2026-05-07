//go:build gridappsd

// Integration tests for cimstomp.Client against the real GridAPPS-D platform
// stack (the gridappsd-docker compose from
// ~/repos/sentient_gridappsd_integration/gridappsd-docker/, brought up via
// `pixi run gridappsd-start`).
//
// These tests are NOT a replacement for the bare-ActiveMQ tests in
// client_integration_test.go. Those run fast against a vanilla ActiveMQ
// Classic and exercise STOMP wire format, header acceptance, and Client
// state transitions in isolation. The tests in this file exercise the
// same Client against the GridAPPS-D platform's real auth-token responder,
// real broker plugins, real request routing, and real credential setup
// (`system / manager`).
//
// Run with:
//
//	make test-gridappsd
//
// or, if the platform is already up,
//
//	go test -tags=gridappsd -race -timeout 5m -v ./internal/cimstomp/
//
// If the broker port is unreachable, every test in this file is skipped.
// That keeps `go test ./...` (and CI without the platform) green.

package cimstomp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-stomp/stomp/v3"
)

const (
	gridappsdAddr     = "127.0.0.1:61613"
	gridappsdUser     = "system"
	gridappsdPassword = "manager"

	// platformStatusQueue is a benign GridAPPS-D request destination that
	// the platform's process manager responds to without side effects on
	// the simulation state. The bare form (no `/queue/` prefix) exercises
	// the Client's destination normalization.
	platformStatusQueue = "goss.gridappsd.process.request.status.platform"
)

// requireGridAPPSD dials the platform broker as a smoke test; if it is not
// running, the gridappsd tests are skipped with a hint at how to bring it
// up. The bare-ActiveMQ tests in client_integration_test.go follow the
// same pattern; gridappsd-docker is a heavier stack so the skip message
// is correspondingly more informative.
func requireGridAPPSD(t *testing.T) {
	t.Helper()
	conn, err := stomp.Dial("tcp", gridappsdAddr,
		stomp.ConnOpt.Login(gridappsdUser, gridappsdPassword),
		stomp.ConnOpt.HeartBeat(5*time.Second, 5*time.Second),
	)
	if err != nil {
		t.Skipf("GridAPPS-D platform not reachable at %s: %v "+
			"(bring it up with `pixi run gridappsd-start` from "+
			"~/repos/sentient_gridappsd_integration/, or run "+
			"`make test-gridappsd` which probes the port and exits "+
			"with a hint if not reachable; the target does not auto-start the stack)",
			gridappsdAddr, err)
	}
	_ = conn.Disconnect()
}

// newGridAPPSDClient is the shared setup for every test in this file: it
// gates on requireGridAPPSD, constructs a Client with the platform's
// dev-default credentials, dials the broker, and registers a Cleanup so
// the connection is closed at test end. Each test gets its own Client
// (no sharing), matching the prior per-test boilerplate exactly.
func newGridAPPSDClient(t *testing.T) *Client {
	t.Helper()
	requireGridAPPSD(t)
	c := NewClient(STOMPConfig{
		Address:  gridappsdAddr,
		User:     gridappsdUser,
		Password: gridappsdPassword,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect against GridAPPS-D: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestGridAPPSD_ConnectFetchesToken verifies that Connect dials the real
// platform broker, sends base64(user:password) to /topic/pnnl.goss.token.topic,
// and caches the token returned by the platform's token responder. The
// Python upstream (gridappsd-python goss.py _make_connection) does the
// same dance; this test is the proof that our Go implementation talks to
// the same responder and gets a non-empty string back.
func TestGridAPPSD_ConnectFetchesToken(t *testing.T) {
	c := newGridAPPSDClient(t)

	tok := c.tokenForTest()
	if tok == "" {
		t.Fatalf("cached token is empty; Connect should have bootstrapped a non-empty token from /topic/pnnl.goss.token.topic")
	}
}

// TestGridAPPSD_RequestRoundTrip verifies that a real benign request to
// the platform yields a non-empty response on the per-request reply-to.
// The platform-status request is chosen because it does not mutate
// simulation state and the platform always responds (even with an empty
// process list before any simulations have started).
//
// This test is the load-bearing proof that the GOSS_HAS_SUBJECT and
// GOSS_SUBJECT headers (set unconditionally by Client.Request) are
// accepted by the real broker. With either header missing or wrong, the
// platform either rejects the SEND or never replies.
func TestGridAPPSD_RequestRoundTrip(t *testing.T) {
	c := newGridAPPSDClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// `get_platform_status` is the canonical benign read. The platform's
	// process manager replies with a JSON document describing currently
	// running processes; the body shape is not part of this test, only
	// that a non-empty response arrives.
	body := []byte(`{"command":"get_platform_status"}`)
	got, err := c.Request(ctx, platformStatusQueue, body)
	if err != nil {
		t.Fatalf("Request to %s: %v", platformStatusQueue, err)
	}
	if len(got) == 0 {
		t.Fatalf("response body is empty; want non-empty platform-status reply")
	}
}

// TestGridAPPSD_CorrelationIDAccepted exercises the Client.Request path
// repeatedly against the real platform to verify the correlation-id
// header (added in GAGO-013, Leon M2) is not rejected by the broker.
//
// The bare-ActiveMQ test (TestIntegration_RequestHappyPath) already
// asserts the header is on the wire. This test asserts the platform's
// real broker accepts it: a request that round-trips successfully proves
// the header is at minimum tolerated. Closes GAGO-016 by direct
// observation against the production-equivalent stack.
func TestGridAPPSD_CorrelationIDAccepted(t *testing.T) {
	c := newGridAPPSDClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Three round-trips: each Request generates a fresh correlation-id
	// internally. If the broker rejected the header on any send, the
	// SEND would error or the request would time out.
	body := []byte(`{"command":"get_platform_status"}`)
	for i := 0; i < 3; i++ {
		resp, err := c.Request(ctx, platformStatusQueue, body)
		if err != nil {
			t.Fatalf("Request[%d] with correlation-id header: %v", i, err)
		}
		if len(resp) == 0 {
			t.Fatalf("Request[%d]: empty response", i)
		}
	}
}

// TestGridAPPSD_GOSSHeadersAccepted asserts that the GOSS_HAS_SUBJECT and
// GOSS_SUBJECT headers Client.Request sets unconditionally are tolerated
// by the real platform broker. A successful round-trip is sufficient
// evidence: the platform's broker plugin filters or rejects SENDs that
// lack the expected GOSS subject framing, so a clean response is proof
// the headers landed correctly.
//
// This is a thin wrapper over TestGridAPPSD_RequestRoundTrip; kept as a
// distinct test so a future change that flips a header off (or sends an
// empty token) shows up as a named failure rather than a generic
// round-trip regression.
func TestGridAPPSD_GOSSHeadersAccepted(t *testing.T) {
	c := newGridAPPSDClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if tok := c.tokenForTest(); strings.TrimSpace(tok) == "" {
		t.Fatalf("token blank before Request; GOSS_SUBJECT would be empty on the wire")
	}

	body := []byte(`{"command":"get_platform_status"}`)
	got, err := c.Request(ctx, platformStatusQueue, body)
	if err != nil {
		t.Fatalf("Request with GOSS headers: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("empty response; the platform did not accept the request frame")
	}
}
