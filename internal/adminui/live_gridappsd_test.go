//go:build gridappsd

// Live test of the bus monitor panel against the platform broker, through
// an admin listener of its own on a loopback port the kernel picks. It
// publishes only to liveMonitorTopic.
//
//	go test -tags=gridappsd -race -run Live ./internal/adminui/

package adminui

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-stomp/stomp/v3"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/busmonitor"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp"
)

const (
	liveBroker       = "127.0.0.1:61613"
	liveUser         = "system"
	livePassword     = "manager"
	liveMonitorTopic = "/topic/test.bridge-monitor-panel"
)

// liveAdminKey reads the operator's admin key file. The key is never
// logged; a missing file skips the test.
func liveAdminKey(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".config", "gridappsd", "2030.5server", "admin-ui-key"))
	if err != nil {
		t.Skip("admin key file unreadable")
	}
	return strings.TrimSpace(string(b))
}

func TestLiveMonitorPanelStreamsAPublishedMessage(t *testing.T) {
	c, err := net.DialTimeout("tcp", liveBroker, 2*time.Second)
	if err != nil {
		t.Skipf("platform broker %s unreachable: %v", liveBroker, err)
	}
	_ = c.Close()
	key := liveAdminKey(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	mon := busmonitor.New(ctx, busmonitor.Config{
		Dial:  busmonitor.NewDialer(cimstomp.STOMPConfig{Address: liveBroker, User: liveUser, Password: livePassword}),
		Probe: func() string { return "/topic/goss.gridappsd.heartbeat" },
	})
	defer mon.Close()
	src := testSources()
	src.Monitor = mon
	s, err := New(Config{Addr: "127.0.0.1:0", Key: key}, src)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runCtx, stop := context.WithCancel(ctx)
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(runCtx) }()
	defer func() {
		stop()
		if err := <-runErr; err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	sr := openMonitorStreamWithKey(t, "http://"+s.Addr(), liveMonitorTopic, "", key)
	for {
		ev := sr.next()
		if ev.Kind == "status" && ev.Text == "live" {
			break
		}
	}
	// SUBSCRIBE carries no receipt, so give the broker a moment to register it.
	time.Sleep(time.Second)

	pub, err := stomp.Dial("tcp", liveBroker, stomp.ConnOpt.Login(liveUser, livePassword))
	if err != nil {
		t.Fatalf("publisher dial: %v", err)
	}
	body := `{"probe": "monitor panel", "at": "` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`
	if err := pub.Send(liveMonitorTopic, "application/json", []byte(body)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := pub.Disconnect(); err != nil {
		t.Logf("publisher disconnect: %v", err)
	}

	for {
		ev := sr.next()
		if ev.Kind != "message" {
			continue
		}
		want := liveMonitorTopic + " " + strconv.Itoa(len(body)) + ` bytes: {"probe":"monitor panel","at":`
		if !strings.HasPrefix(ev.Text, want) {
			t.Fatalf("message = %q, want prefix %q", ev.Text, want)
		}
		return
	}
}
