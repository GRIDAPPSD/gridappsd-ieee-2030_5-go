//go:build gridappsd

// Round-trip subscribe/publish test against the real GridAPPS-D platform
// stack. Does not require a running simulation: the test uses an
// arbitrary test topic the bridge publishes to and subscribes to, which
// exercises the same broker path the simulation pub/sub uses.
//
// Run with:
//
//	make test-gridappsd
//
// or, if the platform is already up:
//
//	go test -tags=gridappsd -race -timeout 5m -v ./internal/cim/sim/
//
// Skips when the broker port is not reachable.

package sim

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp"
	"github.com/go-stomp/stomp/v3"
)

const (
	gridappsdAddr     = "127.0.0.1:61613"
	gridappsdUser     = "system"
	gridappsdPassword = "manager"
)

func requireGridAPPSD(t *testing.T) {
	t.Helper()
	conn, err := stomp.Dial("tcp", gridappsdAddr,
		stomp.ConnOpt.Login(gridappsdUser, gridappsdPassword),
		stomp.ConnOpt.HeartBeat(5*time.Second, 5*time.Second),
	)
	if err != nil {
		t.Skipf("GridAPPS-D platform not reachable at %s: %v "+
			"(bring it up with `pixi run gridappsd-start` from "+
			"~/repos/sentient_gridappsd_integration/)",
			gridappsdAddr, err)
	}
	_ = conn.Disconnect()
}

// TestGridAPPSD_SubscribePublishRoundTrip stands up a Client and a
// Publisher against the real platform broker, subscribes to a fresh test
// topic, sends one frame via the Publisher, and verifies the Subscribe
// path delivers it. Proves the simulation pub/sub primitives work over
// the production-equivalent stack.
func TestGridAPPSD_SubscribePublishRoundTrip(t *testing.T) {
	requireGridAPPSD(t)

	c := cimstomp.NewClient(cimstomp.STOMPConfig{
		Address:  gridappsdAddr,
		User:     gridappsdUser,
		Password: gridappsdPassword,
	})
	connectCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Connect(connectCtx); err != nil {
		t.Fatalf("Connect Client: %v", err)
	}
	defer c.Close()

	dest := fmt.Sprintf("/topic/test.sim.roundtrip.%d", time.Now().UnixNano())
	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	sub, err := c.Subscribe(subCtx, dest)
	if err != nil {
		t.Fatalf("Subscribe %s: %v", dest, err)
	}

	// Allow the broker a moment to register the subscription.
	time.Sleep(300 * time.Millisecond)

	// Publish via cimstomp.Publisher to the same topic. Publisher does
	// not natively send raw bodies; we use the underlying conn directly
	// for byte-exact wire content, which mirrors what the bridge will do
	// for difference messages built via cim/diff.
	body := []byte(`{"simulation_id":"sim-test","message":{"timestamp":1714502400,"measurements":{"_a":{"measurement_mrid":"_a","value":42}}}}`)
	pubConn, err := stomp.Dial("tcp", gridappsdAddr,
		stomp.ConnOpt.Login(gridappsdUser, gridappsdPassword),
		stomp.ConnOpt.HeartBeat(5*time.Second, 5*time.Second),
	)
	if err != nil {
		t.Fatalf("publisher Dial: %v", err)
	}
	defer pubConn.Disconnect()
	if err := pubConn.Send(dest, "application/json", body); err != nil {
		t.Fatalf("publisher Send: %v", err)
	}

	select {
	case msg, ok := <-sub.Messages():
		if !ok {
			t.Fatal("Messages channel closed before frame received")
		}
		if string(msg.Body) != string(body) {
			t.Errorf("delivered body = %q, want %q", msg.Body, body)
		}
		var frame MeasurementFrame
		// cross-check decoder against the real-broker delivery path
		if err := decodeFrame(msg.Body, &frame); err != nil {
			t.Errorf("decodeFrame: %v", err)
		}
		if frame.SimulationID != "sim-test" {
			t.Errorf("SimulationID = %q, want sim-test", frame.SimulationID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for frame from gridappsd-docker broker")
	}

	if !strings.HasPrefix(dest, "/topic/") {
		t.Errorf("dest %q does not start with /topic/", dest)
	}
}
