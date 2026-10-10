package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/sim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sender"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/socsend"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

func socDelta(mrid string) diff.Difference {
	return diff.Difference{Object: mrid, Attribute: socsend.Attribute,
		Value: map[string]any{"percent": 80.0, "hold_seconds": 60.0}}
}

// A state-of-charge send shares the input topic. The subscriber must pass
// over it: no refused count, no log line, no outcome row, no charted
// setpoint, and a control in the same frame is still applied.
// Not parallel: it swaps the process-wide log writer.
func TestSubscriberPassesOverStateOfChargeWithoutLoggingOrCounting(t *testing.T) {
	logs := captureHistoryLog(t)
	const mrid = "mrid-soc-1"
	var history telemetryhistory.Store
	bus, hook := startSeededControlHarness(t, mrid, &history)

	bus.deliver(controlMessage(t, "soc-only", frameEpoch, socDelta(mrid)))
	bus.deliver(controlMessage(t, "soc-unregistered", frameEpoch+1, socDelta("not-in-the-registry")))
	bus.deliver(controlMessage(t, "mixed", frameEpoch+2, socDelta(mrid), targetW(mrid, 5000)))
	waitProcessed(t, hook, 1)

	snap := hook.Snapshot()
	if snap.Skipped != 0 || snap.Applied != 1 || snap.Restated != 0 || snap.EmptyFrames != 0 {
		t.Errorf("skipped=%d applied=%d restated=%d empty=%d, want 0 1 0 0", snap.Skipped, snap.Applied, snap.Restated, snap.EmptyFrames)
	}
	if out := logs.String(); strings.Contains(out, "skip delta") || strings.Contains(out, socsend.Attribute) {
		t.Errorf("the subscriber logged a state-of-charge delta: %q", out)
	}
	for _, id := range []string{"soc-only", "soc-unregistered"} {
		if m, ok := hook.Outcome(id); ok {
			t.Errorf("Outcome(%s) = %+v, want no row for a pass-over frame", id, m)
		}
	}
	mixed, ok := hook.Outcome("mixed")
	if !ok || len(mixed.Deltas) != 1 || mixed.Deltas[0].Attribute != controlAttr {
		t.Errorf("Outcome(mixed) = %+v found=%v, want only the control delta", mixed, ok)
	}
	for _, s := range history.Snapshot() {
		if s.Key.Attribute == socsend.Attribute {
			t.Errorf("history charted %+v for a state-of-charge send", s.Key)
		}
	}
}

type noStatuses struct{}

func (noStatuses) DERStatusSnapshots(context.Context) ([]sep2embed.DERStatusSnapshot, error) {
	return nil, nil
}

// The state-of-charge service publishes where the control subscriber
// listens and does not read the bus sender's switch: with the switch off
// the sender refuses and the service still publishes.
func TestSoCServicePublishesToTheInputTopicWhateverTheSenderSwitchSays(t *testing.T) {
	t.Parallel()
	reg := registry.New()
	if err := reg.Add(registry.Entry{MRID: "_dev-a", Name: "A", LFDI: strings.Repeat("A", 40)}); err != nil {
		t.Fatal(err)
	}
	var hook controlobs.Hook

	for _, tc := range []struct{ appID, want string }{
		{"IEEE_2030_5", "/topic/goss.gridappsd.IEEE_2030_5.input"},
		{"app-x", sim.ApplicationInputTopic("app-x", "")},
	} {
		bus := &destBus{}
		cfg := config{ApplicationID: tc.appID, SEP2AdminUIBusPublishAtStart: false}
		snd, err := newBusSender(cfg, bus, reg, &hook)
		if err != nil || snd == nil {
			t.Fatalf("newBusSender: %v, %v", snd, err)
		}
		if _, err := snd.SendConnect(context.Background(), "test", "_dev-a", true); !errors.Is(err, sender.ErrPublishingOff) {
			t.Fatalf("sender with the switch off: err = %v, want ErrPublishingOff", err)
		}

		svc, err := newSoCService(cfg, bus, reg, noStatuses{}, nil)
		if err != nil || svc == nil {
			t.Fatalf("newSoCService: %v, %v", svc, err)
		}
		if _, err := svc.Send(context.Background(), "_dev-a", 80, time.Minute); err != nil {
			t.Fatalf("Send with the sender switch off: %v", err)
		}
		if len(bus.dests) != 1 || bus.dests[0] != tc.want {
			t.Errorf("app %q published to %q, want exactly %q", tc.appID, bus.dests, tc.want)
		}
	}

	if svc, err := newSoCService(config{}, &destBus{}, reg, noStatuses{}, nil); svc != nil || err != nil {
		t.Errorf("no application id: %v, %v, want no service and no error", svc, err)
	}
}
