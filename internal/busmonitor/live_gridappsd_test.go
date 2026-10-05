//go:build gridappsd

// Live test against the platform broker. It uses only its own connections
// and publishes only under liveTopicBase.
//
//	go test -tags=gridappsd -race ./internal/busmonitor/

package busmonitor

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp"
	"github.com/go-stomp/stomp/v3"
)

const (
	liveAddr      = "127.0.0.1:61613"
	liveUser      = "system"
	livePassword  = "manager"
	liveTopicBase = "test.bridge-monitor-live-199"
)

func requireLiveBroker(t *testing.T) {
	t.Helper()
	c, err := net.DialTimeout("tcp", liveAddr, 2*time.Second)
	if err != nil {
		t.Skipf("platform broker %s unreachable: %v", liveAddr, err)
	}
	if err := c.Close(); err != nil {
		t.Logf("close probe connection: %v", err)
	}
}

func livePublish(t *testing.T, dest, body string) {
	t.Helper()
	c, err := stomp.Dial("tcp", liveAddr, stomp.ConnOpt.Login(liveUser, livePassword))
	if err != nil {
		t.Fatalf("publisher dial: %v", err)
	}
	// Disconnect waits for the broker's receipt, so the SEND is flushed first.
	defer func() {
		if err := c.Disconnect(); err != nil {
			t.Logf("publisher disconnect: %v", err)
		}
	}()
	if err := c.Send(dest, "text/plain", []byte(body)); err != nil {
		t.Fatalf("publish to %s: %v", dest, err)
	}
}

func TestLiveWatchedTopicsAreIsolatedFromAnotherConnection(t *testing.T) {
	requireLiveBroker(t)
	cfg := cimstomp.STOMPConfig{Address: liveAddr, User: liveUser, Password: livePassword}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Stand-in for the bridge's main connection: its own subscription must
	// keep delivering after a watched topic is closed by the broker.
	main := cimstomp.NewClient(cfg)
	if err := main.Connect(ctx); err != nil {
		t.Fatalf("main connect: %v", err)
	}
	defer func() {
		if err := main.Close(); err != nil {
			t.Logf("main close: %v", err)
		}
	}()
	mainTopic := "/topic/" + liveTopicBase + ".main"
	mainSub, err := main.Subscribe(ctx, mainTopic)
	if err != nil {
		t.Fatalf("main subscribe: %v", err)
	}
	time.Sleep(time.Second)

	m := New(ctx, Config{Dial: NewDialer(cfg), BackoffMin: 50 * time.Millisecond, BackoffMax: 100 * time.Millisecond})
	defer m.Close()

	exact := "/topic/" + liveTopicBase + ".exact"
	vExact, err := m.Watch(exact)
	if err != nil {
		t.Fatal(err)
	}
	vWild, err := m.Watch("/topic/" + liveTopicBase + ".>")
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, vExact, StateLive)
	waitStatus(t, vWild, StateLive)
	// STOMP SUBSCRIBE carries no receipt here, so "live" means the frame was
	// written, not that the broker has registered it.
	time.Sleep(time.Second)

	// A trailing wildcard receives a message published to the exact topic,
	// and the frame says which topic it came from.
	livePublish(t, exact, "hello")
	for name, v := range map[string]*Viewer{"exact": vExact, "wildcard": vWild} {
		got := waitMessages(t, v, 1)[0]
		if string(got.Body) != "hello" || got.Size != 5 || got.Destination != exact {
			t.Fatalf("%s viewer: body %q size %d destination %q", name, got.Body, got.Size, got.Destination)
		}
	}

	// A name the broker answers by closing the connection. attach skips the
	// validator on purpose: the validator would never let this name through.
	bad, err := m.attach("/topic/")
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, bad, StateReconnecting)
	waitStatus(t, bad, StateFailed)

	livePublish(t, mainTopic, "main still works")
	select {
	case msg := <-mainSub.Messages():
		if string(msg.Body) != "main still works" {
			t.Fatalf("main connection got %q", msg.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("main connection stopped delivering after the watched topic was closed (err %v)", mainSub.Err())
	}

	// The healthy watched topics kept their connections too.
	livePublish(t, exact, "after")
	if got := string(waitMessages(t, vExact, 1)[0].Body); got != "after" {
		t.Fatalf("exact viewer after the bad topic got %q", got)
	}
}
