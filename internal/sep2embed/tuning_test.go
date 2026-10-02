package sep2embed

import (
	"context"
	"strings"
	"testing"
	"time"

	coresub "github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/handlers/subscription"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

func newTuningEmbed(t *testing.T, cfg Config) *Embed {
	t.Helper()
	reg := registry.New()
	if err := reg.AddBatch(fixtureEntries()); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	cfg.Addr = "127.0.0.1:0"
	cfg.CertDir = t.TempDir()
	cfg.ResolveRegistrationPIN = testResolvePIN
	e, err := New(context.Background(), cfg, reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv, ok := e.srv.(*observedMTLSServer); ok {
		t.Cleanup(func() { _ = srv.listener.Close() })
	}
	return e
}

func TestListenerTimeoutsReachHTTPServer(t *testing.T) {
	t.Parallel()

	t.Run("configured values", func(t *testing.T) {
		t.Parallel()
		e := newTuningEmbed(t, Config{
			EnableCCM:         true,
			ReadHeaderTimeout: 2 * time.Second,
			ReadTimeout:       3 * time.Second,
			WriteTimeout:      4 * time.Second,
			IdleTimeout:       5 * time.Second,
			ShutdownTimeout:   6 * time.Second,
		})
		srv, ok := e.srv.(*observedMTLSServer)
		if !ok {
			t.Fatalf("srv is %T, want *observedMTLSServer", e.srv)
		}
		got := [5]time.Duration{srv.httpSrv.ReadHeaderTimeout, srv.httpSrv.ReadTimeout, srv.httpSrv.WriteTimeout, srv.httpSrv.IdleTimeout, srv.shutdownTimeout}
		want := [5]time.Duration{2 * time.Second, 3 * time.Second, 4 * time.Second, 5 * time.Second, 6 * time.Second}
		if got != want {
			t.Errorf("readHeader/read/write/idle/shutdown = %v, want %v", got, want)
		}
	})

	t.Run("zero keeps the defaults", func(t *testing.T) {
		t.Parallel()
		e := newTuningEmbed(t, Config{EnableCCM: true})
		srv := e.srv.(*observedMTLSServer)
		got := [5]time.Duration{srv.httpSrv.ReadHeaderTimeout, srv.httpSrv.ReadTimeout, srv.httpSrv.WriteTimeout, srv.httpSrv.IdleTimeout, srv.shutdownTimeout}
		want := [5]time.Duration{10 * time.Second, 30 * time.Second, 30 * time.Second, 120 * time.Second, 5 * time.Second}
		if got != want {
			t.Errorf("readHeader/read/write/idle/shutdown = %v, want %v", got, want)
		}
	})
}

func TestListenerTimeoutsRefusedOnDefaultListener(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	if err := reg.AddBatch(fixtureEntries()); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	_, err := New(context.Background(), Config{
		Addr: "127.0.0.1:0", CertDir: t.TempDir(), ResolveRegistrationPIN: testResolvePIN,
		ReadTimeout: time.Second,
	}, reg)
	if err == nil || !strings.Contains(err.Error(), "listener timeouts need Observer or EnableCCM") {
		t.Fatalf("New with a listener timeout and no Observer or EnableCCM: err = %v, want the refusal", err)
	}
}

func TestDefaultListenerTimeoutsAcceptedOnDefaultListener(t *testing.T) {
	t.Parallel()

	newTuningEmbed(t, Config{
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	})
}

func TestControlSweepIntervalReachesEmbed(t *testing.T) {
	t.Parallel()

	if got := newTuningEmbed(t, Config{EnableCCM: true, ControlSweepInterval: 7 * time.Second}).controlSweepInterval(); got != 7*time.Second {
		t.Errorf("controlSweepInterval = %s, want 7s", got)
	}
	if got := newTuningEmbed(t, Config{EnableCCM: true}).controlSweepInterval(); got != 10*time.Second {
		t.Errorf("default controlSweepInterval = %s, want 10s", got)
	}
}

func TestNotifySizing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                 string
		cfg                  Config
		wantWorkers, wantLen int
	}{
		{"configured", Config{NotifyWorkers: 9, NotifyQueueSize: 250}, 9, 250},
		{"unset", Config{}, 4, 100},
		{"negative", Config{NotifyWorkers: -1, NotifyQueueSize: -5}, 4, 100},
	} {
		w, q := notifySizing(tc.cfg)
		if w != tc.wantWorkers || q != tc.wantLen {
			t.Errorf("%s: notifySizing = (%d, %d), want (%d, %d)", tc.name, w, q, tc.wantWorkers, tc.wantLen)
		}
	}
}

// These two tests swap package seams, so they are not parallel.
func TestConfiguredSweepIntervalReachesTheTicker(t *testing.T) {
	var got time.Duration
	orig := newSweepTicker
	newSweepTicker = func(d time.Duration) *time.Ticker { got = d; return time.NewTicker(time.Hour) }
	t.Cleanup(func() { newSweepTicker = orig })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	(&Embed{sweepInterval: 7 * time.Second}).runControlSweep(ctx)
	if got != 7*time.Second {
		t.Errorf("ticker period = %s, want 7s", got)
	}
	(&Embed{}).runControlSweep(ctx)
	if got != 10*time.Second {
		t.Errorf("default ticker period = %s, want 10s", got)
	}
}

func TestConfiguredNotifySizingReachesTheNotifier(t *testing.T) {
	var gotWorkers, gotQueue int
	orig := newNotifier
	newNotifier = func(subs coresub.SubscriptionLister, workers, queueSize int, allowLoopback bool) *coresub.Manager {
		gotWorkers, gotQueue = workers, queueSize
		return orig(subs, workers, queueSize, allowLoopback)
	}
	t.Cleanup(func() { newNotifier = orig })

	newTuningEmbed(t, Config{EnableCCM: true, NotifyWorkers: 9, NotifyQueueSize: 250})
	if gotWorkers != 9 || gotQueue != 250 {
		t.Errorf("notifier built with workers=%d queue=%d, want 9 and 250", gotWorkers, gotQueue)
	}
	newTuningEmbed(t, Config{EnableCCM: true})
	if gotWorkers != 4 || gotQueue != 100 {
		t.Errorf("default notifier built with workers=%d queue=%d, want 4 and 100", gotWorkers, gotQueue)
	}
}
