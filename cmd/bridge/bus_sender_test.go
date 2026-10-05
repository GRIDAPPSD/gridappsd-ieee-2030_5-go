package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/sim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

type destBus struct {
	mu    sync.Mutex
	dests []string
}

func (b *destBus) Send(_ context.Context, dest, _ string, _ []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dests = append(b.dests, dest)
	return nil
}

// TestNewBusSenderPublishesToTheInputTopicTheSubscriberReads: the sender
// writes where the control subscriber listens, starts in the configured
// switch state, and is absent with no application id.
func TestNewBusSenderPublishesToTheInputTopicTheSubscriberReads(t *testing.T) {
	t.Parallel()
	reg := registry.New()
	if err := reg.Add(registry.Entry{MRID: "_dev-a", Name: "A", LFDI: strings.Repeat("A", 40)}); err != nil {
		t.Fatal(err)
	}
	var hook controlobs.Hook

	for _, atStart := range []bool{false, true} {
		bus := &destBus{}
		s, err := newBusSender(config{ApplicationID: "app-x", SEP2AdminUIBusPublishAtStart: atStart}, bus, reg, &hook)
		if err != nil || s == nil {
			t.Fatalf("newBusSender: %v, %v", s, err)
		}
		if st := s.Publishing(); st.On != atStart || st.ChangedBy != "start" {
			t.Errorf("at start %v: state %+v", atStart, st)
		}
		s.SetPublishing(true, "test")
		if _, err := s.SendConnect(context.Background(), "test", "_dev-a", true); err != nil {
			t.Fatal(err)
		}
		if want := sim.ApplicationInputTopic("app-x", ""); len(bus.dests) != 1 || bus.dests[0] != want {
			t.Errorf("published to %q, want %q", bus.dests, want)
		}
	}

	if s, err := newBusSender(config{}, &destBus{}, reg, &hook); s != nil || err != nil {
		t.Errorf("no application id: %v, %v, want no sender and no error", s, err)
	}
}
