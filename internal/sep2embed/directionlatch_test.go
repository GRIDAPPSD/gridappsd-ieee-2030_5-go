package sep2embed

import (
	"context"
	"testing"
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
