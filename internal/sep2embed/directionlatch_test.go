package sep2embed

import (
	"context"
	"errors"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

func TestDirectionPinkFollowsTheSendPath(t *testing.T) {
	t.Parallel()
	e, reg, clock, _ := newSendEmbed(t, controlClockUnix)
	ctx := context.Background()
	step := int64(0)
	send := func(watts float64) {
		t.Helper()
		step++
		clock.set(controlClockUnix + step*20)
		if _, err := e.ApplyControlFor(ctx, reg, targetWDelta("mrid-a", 0.0, watts), 60); err != nil {
			t.Fatalf("send %v: %v", watts, err)
		}
	}

	steps := []struct {
		name  string
		watts float64
		pink  bool
	}{
		{"first charge", -2000, false},
		{"charge then discharge", 3000, true},
		{"a 0 W send keeps pink", 0, true},
		{"same direction again keeps pink", 1000, true},
		{"a new charge clears it", -500, false},
		{"discharge after charge is pink again", 500, true},
		{"a follow-on sized 0 W changes nothing", 0, true},
	}
	for _, st := range steps {
		send(st.watts)
		if got := e.DirectionPink("mrid-a"); got != st.pink {
			t.Errorf("%s: pink = %v, want %v", st.name, got, st.pink)
		}
	}
	if e.DirectionPink("mrid-b") {
		t.Error("an untouched device must not be pink")
	}
}

// Discharge then charge is blue, never pink.
func TestDirectionPinkNeedsChargeBeforeDischarge(t *testing.T) {
	t.Parallel()
	e, reg, clock, _ := newSendEmbed(t, controlClockUnix)
	for i, w := range []float64{4000, -4000} {
		clock.set(controlClockUnix + int64(i)*20)
		if _, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0.0, w), 60); err != nil {
			t.Fatal(err)
		}
		if e.DirectionPink("mrid-a") {
			t.Errorf("after %v W: pink, want not pink", w)
		}
	}
}

// The bus path records the same latch, and a restated setpoint does not.
func TestDirectionPinkFollowsTheBusPath(t *testing.T) {
	t.Parallel()
	e, reg, clock, _ := newSendEmbed(t, controlClockUnix)
	ctx := context.Background()
	for i, st := range []struct {
		watts float64
		pink  bool
	}{{-1000, false}, {1000, true}, {1000, true}} {
		clock.set(controlClockUnix + int64(i)*10)
		if _, err := e.ApplyControlDeltaOutcome(ctx, reg, targetWDelta("mrid-a", 0.0, st.watts)); err != nil {
			t.Fatalf("bus delta %v: %v", st.watts, err)
		}
		if got := e.DirectionPink("mrid-a"); got != st.pink {
			t.Errorf("after bus %v W: pink = %v, want %v", st.watts, got, st.pink)
		}
	}
}

// A send whose requested control is not written leaves the latch as it was,
// so a refused discharge does not turn the card pink.
func TestDirectionPinkIsNotLatchedByAFailedSend(t *testing.T) {
	t.Parallel()
	e, reg, clock, _ := newSendEmbed(t, controlClockUnix)
	ctx := context.Background()
	if _, err := e.ApplyControlFor(ctx, reg, targetWDelta("mrid-a", 0.0, -2000), 60); err != nil {
		t.Fatalf("charge send: %v", err)
	}

	real := e.stores.DERControls
	e.stores.DERControls = faultyDERControls{
		ScopedStore: real,
		failCreate:  func(string, sep2.DERControl) bool { return true },
	}
	clock.set(controlClockUnix + 20)
	if _, err := e.ApplyControlFor(ctx, reg, targetWDelta("mrid-a", 0.0, 3000), 60); !errors.Is(err, errInjected) {
		t.Fatalf("discharge send error = %v, want the injected failure", err)
	}
	if e.DirectionPink("mrid-a") {
		t.Error("pink after a failed discharge send, want the latch unchanged")
	}

	e.stores.DERControls = real
	clock.set(controlClockUnix + 40)
	if _, err := e.ApplyControlFor(ctx, reg, targetWDelta("mrid-a", 0.0, 3000), 60); err != nil {
		t.Fatalf("retried discharge send: %v", err)
	}
	if !e.DirectionPink("mrid-a") {
		t.Error("not pink after the discharge send succeeded")
	}
}

// Charge on one path followed by discharge on the other is a direction
// change, so the latch is shared between them.
func TestDirectionPinkSpansTheTwoPaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	send := func(e *Embed, reg *registry.Registry, watts float64) error {
		_, err := e.ApplyControlFor(ctx, reg, targetWDelta("mrid-a", 0.0, watts), 60)
		return err
	}
	bus := func(e *Embed, reg *registry.Registry, watts float64) error {
		_, err := e.ApplyControlDeltaOutcome(ctx, reg, targetWDelta("mrid-a", 0.0, watts))
		return err
	}
	for _, tc := range []struct {
		name          string
		first, second func(*Embed, *registry.Registry, float64) error
	}{
		{"charge by send then discharge by bus", send, bus},
		{"charge by bus then discharge by send", bus, send},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, reg, clock, _ := newSendEmbed(t, controlClockUnix)
			if err := tc.first(e, reg, -2000); err != nil {
				t.Fatalf("charge: %v", err)
			}
			if e.DirectionPink("mrid-a") {
				t.Fatal("pink after the first charge")
			}
			clock.set(controlClockUnix + 20)
			if err := tc.second(e, reg, 3000); err != nil {
				t.Fatalf("discharge: %v", err)
			}
			if !e.DirectionPink("mrid-a") {
				t.Error("not pink after a discharge following a charge on the other path")
			}
		})
	}
}
