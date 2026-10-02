package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-go/fieldbus"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/sim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/gridappsdclient"
)

// routerBus models gridappsd-go's router: Subscribe on a destination
// the connection already holds only adds a handler and never touches the
// connection, so it succeeds on a dead bus; Subscribe on a destination
// not yet held needs the connection and fails when it is dead.
type routerBus struct {
	mu       sync.Mutex
	dead     bool
	held     map[string]bool
	tok      fieldbus.Token
	connects int
}

var _ fieldbus.MessageBus = (*routerBus)(nil)

func (b *routerBus) Connect(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dead = false
	b.connects++
	return nil
}

func (b *routerBus) Disconnect() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held = map[string]bool{}
	return nil
}

func (b *routerBus) IsConnected() bool { return true }

func (b *routerBus) Subscribe(_ context.Context, dest string, _ fieldbus.Handler) (fieldbus.Token, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.held[dest] && b.dead {
		return 0, errors.New("connection closed unexpectedly")
	}
	b.held[dest] = true
	b.tok++
	return b.tok, nil
}

func (b *routerBus) Unsubscribe(context.Context, string, fieldbus.Token) error { return nil }

func (b *routerBus) Send(context.Context, string, string, []byte) error { return nil }

func (b *routerBus) GetResponse(context.Context, string, string, []byte) ([]byte, error) {
	return nil, errors.New("routerBus: GetResponse not used")
}

func (b *routerBus) connectCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connects
}

// A dead connection must be noticed whether or not a simulation id is
// set, so the probe destination must not be one the bridge already
// holds.
func TestSupervisorProbeDetectsDeadBusWithoutSimulationID(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config
	}{
		{"no simulation id", config{ApplicationID: "IEEE_2030_5"}},
		{"simulation id", config{ApplicationID: "IEEE_2030_5", SimulationID: "sim-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus := &routerBus{held: map[string]bool{}}
			subs := gridappsdclient.NewSupervisor(bus,
				gridappsdclient.WithProbeDestination(probeDestination(tc.cfg)),
				gridappsdclient.WithProbeInterval(time.Millisecond))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if _, err := subs.Subscribe(ctx, sim.ApplicationInputTopic(tc.cfg.ApplicationID, "")); err != nil {
				t.Fatalf("Subscribe: %v", err)
			}

			bus.mu.Lock()
			bus.dead = true
			bus.mu.Unlock()

			waitFor(3*time.Second, func() bool { return bus.connectCount() >= 1 })
			if got := bus.connectCount(); got < 1 {
				t.Errorf("Connect calls after the bus died = %d, want at least 1: the probe never saw the dead connection", got)
			}
		})
	}
}
