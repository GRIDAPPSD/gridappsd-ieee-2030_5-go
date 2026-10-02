package gridappsdclient

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-go/fieldbus"
)

func TestSupervisorSettingsDefaultsAndOptions(t *testing.T) {
	t.Parallel()

	def := NewSupervisor(newHealthBus()).Settings()
	wantDef := SupervisorSettings{
		ProbeInterval:      5 * time.Second,
		ProbeTimeout:       10 * time.Second,
		BackoffBase:        500 * time.Millisecond,
		BackoffMax:         10 * time.Second,
		UnsubscribeTimeout: 5 * time.Second,
	}
	if def != wantDef {
		t.Errorf("default settings = %+v, want %+v", def, wantDef)
	}

	got := NewSupervisor(newHealthBus(),
		WithProbeDestination(testLogTopic),
		WithProbeInterval(2*time.Second),
		WithProbeTimeout(3*time.Second),
		WithRecoverBackoff(7*time.Millisecond, 9*time.Second),
		WithUnsubscribeTimeout(11*time.Second),
	).Settings()
	want := SupervisorSettings{
		ProbeDestination:   testLogTopic,
		ProbeInterval:      2 * time.Second,
		ProbeTimeout:       3 * time.Second,
		BackoffBase:        7 * time.Millisecond,
		BackoffMax:         9 * time.Second,
		UnsubscribeTimeout: 11 * time.Second,
	}
	if got != want {
		t.Errorf("settings = %+v, want %+v", got, want)
	}
}

func TestSupervisor_ReconnectBackoffUsesConfiguredBounds(t *testing.T) {
	bus := newHealthBus()
	bus.connectErr = errors.New("connection refused")

	s := NewSupervisor(bus,
		WithRecoverBackoff(10*time.Millisecond, 40*time.Millisecond),
		WithMaxRecoverAttempts(5),
	)
	var (
		mu     sync.Mutex
		delays []time.Duration
	)
	s.sleep = func(_ context.Context, d time.Duration) bool {
		mu.Lock()
		defer mu.Unlock()
		delays = append(delays, d)
		return true
	}

	if err := s.recover(context.Background()); err == nil {
		t.Fatal("recover against a dead broker = nil, want an error")
	}

	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond}
	mu.Lock()
	defer mu.Unlock()
	if len(delays) != len(want) {
		t.Fatalf("backoff delays = %v, want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("backoff delay %d = %s, want %s", i, delays[i], want[i])
		}
	}
}

func TestSupervisor_ProbeTimeoutIsTheConfiguredBound(t *testing.T) {
	s := NewSupervisor(newHealthBus(), WithProbeTimeout(20*time.Millisecond))
	s.probe = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	start := time.Now()
	err := s.runProbe(context.Background())
	if err == nil {
		t.Fatal("runProbe on a probe that never answers = nil, want a timeout")
	}
	if !strings.Contains(err.Error(), "within 20ms") {
		t.Errorf("runProbe error = %q, want it to name the 20ms bound", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("runProbe took %s, want close to 20ms", elapsed)
	}
}

// stuckUnsubBus never returns from Unsubscribe until released, as
// go-stomp's receipt wait does against an unresponsive broker.
type stuckUnsubBus struct {
	*healthBus
	release chan struct{}
}

func (b *stuckUnsubBus) Unsubscribe(_ context.Context, _ string, _ fieldbus.Token) error {
	<-b.release
	return nil
}

func TestSupervisor_UnsubscribeTimeoutBoundsTeardown(t *testing.T) {
	bus := &stuckUnsubBus{healthBus: newHealthBus(), release: make(chan struct{})}
	defer close(bus.release)

	s := NewSupervisor(bus, WithUnsubscribeTimeout(30*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := s.Subscribe(ctx, testOutputTopic)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	start := time.Now()
	cancel()
	select {
	case <-sub.Messages():
	case <-time.After(3 * time.Second):
		t.Fatal("teardown did not finish within 3s of cancel with a 30ms unsubscribe timeout")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("teardown took %s, want close to 30ms (the default is 5s)", elapsed)
	}
	if !errors.Is(sub.Err(), context.DeadlineExceeded) {
		t.Errorf("sub.Err() = %v, want it to wrap context.DeadlineExceeded from the unsubscribe bound", sub.Err())
	}
}
