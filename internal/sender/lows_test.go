package sender

import (
	"errors"
	"testing"
	"time"
)

// An on flip made while an off flip waits for a send must stay the newest
// switch row, so the list never shows off above a switch that is on.
func TestOnFlipDuringAnOffFlipsWaitStaysNewest(t *testing.T) {
	bus := newHoldBus(false)
	r := rigWithBus(t, bus, 5*time.Second)
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		_, _ = r.s.SendRaw(ctx, "a", rawBody("slow", "_dev-a", 0, 1))
	}()
	<-bus.entered
	offDone := make(chan State)
	go func() { offDone <- r.s.SetPublishing(false, "off-flipper") }()
	// The off flip is waiting for the held send.
	deadline := time.Now().Add(3 * time.Second)
	for r.s.Publishing().On {
		if time.Now().After(deadline) {
			t.Fatal("switch never went off")
		}
		time.Sleep(time.Millisecond)
	}
	r.s.SetPublishing(true, "on-flipper")
	close(bus.release)
	<-sent
	<-offDone

	if st := r.s.Publishing(); !st.On || st.ChangedBy != "on-flipper" || st.StillInFlight != 0 {
		t.Fatalf("state = %+v, want on, changed by on-flipper, nothing in flight", st)
	}
	var switches []Entry
	for _, e := range r.s.Recent() {
		if e.Kind == KindSwitch {
			switches = append(switches, e)
		}
	}
	if len(switches) != 2 || switches[0].Outcome != "on" || switches[0].Remote != "on-flipper" ||
		switches[1].Outcome != "off" || switches[1].Remote != "off-flipper" {
		t.Errorf("switch rows newest first = %+v, want on by on-flipper above off by off-flipper", switches)
	}
}

// A send that failed, or was cancelled by the switch, did not reach the
// control path, so its difference_mrid may be used again.
func TestFailedRawSendMayBeRetriedWithItsMRID(t *testing.T) {
	r := newRig(t, true)
	r.bus.err = errors.New("broker gone")
	if _, err := r.s.SendRaw(ctx, "a", rawBody("retry-me", "_dev-a", 0, 1)); err == nil {
		t.Fatal("send over a failing bus succeeded")
	}
	r.bus.err = nil
	r.clk.advance(time.Second)
	res, err := r.s.SendRaw(ctx, "a", rawBody("retry-me", "_dev-a", 0, 1))
	if err != nil || res.DifferenceMRID != "retry-me" {
		t.Fatalf("retry = %+v, %v; want published under retry-me", res, err)
	}
	if r.bus.count() != 1 {
		t.Errorf("frames = %d, want 1", r.bus.count())
	}
	rows := r.s.Recent()
	if len(rows) != 2 || rows[0].Outcome != OutcomePending || rows[1].Outcome != OutcomeFailed {
		t.Errorf("rows = %+v, want the retry pending above the failed attempt", rows)
	}
	// Once the retry succeeded the mrid is held again.
	r.clk.advance(time.Second)
	if _, err := r.s.SendRaw(ctx, "a", rawBody("retry-me", "_dev-a", 0, 1)); !errors.Is(err, ErrDuplicateMRID) {
		t.Errorf("third use: err = %v, want ErrDuplicateMRID", err)
	}
}

// The off flip's in-flight count reaches the state the UI reads, not only
// the audit log, and clears when publishing goes on again.
func TestOffFlipReportsSendsStillInFlight(t *testing.T) {
	bus := newHoldBus(false)
	r := rigWithBus(t, bus, 50*time.Millisecond)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.s.SendRaw(ctx, "a", rawBody("stuck", "_dev-a", 0, 1))
	}()
	<-bus.entered
	got := r.s.SetPublishing(false, "operator")
	if got.On || got.StillInFlight != 1 {
		t.Errorf("returned state = %+v, want off with 1 still in flight", got)
	}
	if st := r.s.Publishing(); st.StillInFlight != 1 {
		t.Errorf("Publishing() = %+v, want StillInFlight 1", st)
	}
	close(bus.release)
	<-done
	if st := r.s.SetPublishing(true, "operator"); st.StillInFlight != 0 {
		t.Errorf("after on: %+v, want StillInFlight 0", st)
	}
}
