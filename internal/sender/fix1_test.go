package sender

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
)

// holdBus blocks every Send until release is closed. When honorCtx is set it
// gives up as soon as the send's context is cancelled, as a bus that supports
// cancellation would; otherwise it ignores the context, as go-stomp's Send does.
type holdBus struct {
	honorCtx  bool
	release   chan struct{}
	entered   chan struct{}
	mu        sync.Mutex
	delivered int
}

func newHoldBus(honorCtx bool) *holdBus {
	return &holdBus{honorCtx: honorCtx, release: make(chan struct{}), entered: make(chan struct{}, 64)}
}

func (b *holdBus) Send(ctx context.Context, _, _ string, _ []byte) error {
	b.entered <- struct{}{}
	if b.honorCtx {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-b.release:
		}
	} else {
		<-b.release
	}
	b.mu.Lock()
	b.delivered++
	b.mu.Unlock()
	return nil
}

func (b *holdBus) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.delivered
}

func rigWithBus(t *testing.T, bus Bus, flipWait time.Duration) *rig {
	t.Helper()
	r := &rig{clk: &clock{t: t0}, obs: &fakeOutcomes{}, logs: &logSink{}, reg: newRegistry(t)}
	s, err := New(Config{Bus: bus, Registry: r.reg, Destination: destTopic, Outcomes: r.obs,
		PublishAtStart: true, Now: r.clk.now, Logf: r.logs.logf, FlipWait: flipWait})
	if err != nil {
		t.Fatal(err)
	}
	r.s = s
	return r
}

// Item 1: a raw difference_mrid already used is refused.
func TestReusedRawMRIDIsRefused(t *testing.T) {
	r := newRig(t, true)
	if _, err := r.s.SendRaw(ctx, "a", rawBody("dup", "_dev-a", 0, 1)); err != nil {
		t.Fatal(err)
	}
	r.clk.advance(time.Second)
	_, err := r.s.SendRaw(ctx, "b", rawBody("dup", "_dev-a", 0, 2))
	if !errors.Is(err, ErrDuplicateMRID) || !strings.Contains(err.Error(), `"dup"`) {
		t.Fatalf("err = %v, want ErrDuplicateMRID naming the mrid", err)
	}
	if r.bus.count() != 1 {
		t.Errorf("%d frames, want 1: the reused mrid must not be published", r.bus.count())
	}
	r.obs.put(controlobs.MessageOutcome{DifferenceMRID: "dup", At: t0, Deltas: []controlobs.DeltaOutcome{{Result: controlobs.ResultIssued}}})
	for _, e := range r.s.Recent() {
		if e.DifferenceMRID == "dup" && e.Outcome != OutcomeIssued {
			t.Errorf("the one published row = %+v, want issued", e)
		}
	}
	if got := r.s.Refusals(); len(got) != 1 || got[0].Outcome != OutcomeRefused || !strings.Contains(got[0].Reason, "already used") {
		t.Errorf("refusals = %+v, want one row saying the mrid was already used", got)
	}
}

func TestRawMRIDInTheControlPathRingIsRefused(t *testing.T) {
	r := newRig(t, true)
	r.obs.put(controlobs.MessageOutcome{DifferenceMRID: "ring-only", At: t0.Add(-time.Hour), Deltas: []controlobs.DeltaOutcome{{Result: controlobs.ResultIssued}}})
	if _, err := r.s.SendRaw(ctx, "a", rawBody("ring-only", "_dev-a", 0, 1)); !errors.Is(err, ErrDuplicateMRID) {
		t.Errorf("err = %v, want ErrDuplicateMRID for an mrid the control path already holds", err)
	}
	if r.bus.count() != 0 {
		t.Error("frame published")
	}
}

func TestMRIDOfAnEarlierRefusedBodyMayBeReused(t *testing.T) {
	r := newRig(t, true)
	if _, err := r.s.SendRaw(ctx, "a", rawBody("again", "_nobody", 0, 1)); err == nil {
		t.Fatal("body for an unknown device was accepted")
	}
	r.clk.advance(time.Second)
	if _, err := r.s.SendRaw(ctx, "a", rawBody("again", "_dev-a", 0, 1)); err != nil {
		t.Errorf("a corrected resend of a refused body was refused: %v", err)
	}
}

func TestConcurrentRawSendsWithOneMRIDPublishOnce(t *testing.T) {
	r := newRig(t, true)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.s.SendRaw(ctx, "a", rawBody("same", "_dev-a", 0, 1)); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 || r.bus.count() != 1 {
		t.Errorf("%d sends accepted, %d frames; want exactly 1", ok, r.bus.count())
	}
}

// Item 2: sends in flight when the switch goes off.
func TestFlipOffCancelsHeldSendsAndNoneIsDeliveredAfterwards(t *testing.T) {
	bus := newHoldBus(true)
	r := rigWithBus(t, bus, 5*time.Second)
	var wg sync.WaitGroup
	for i := 0; i < RateBurst; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = r.s.SendRaw(ctx, "a", rawBody(fmt.Sprintf("held-%d", i), "_dev-a", 0, i))
		}(i)
	}
	for i := 0; i < RateBurst; i++ {
		<-bus.entered
	}
	r.s.SetPublishing(false, "operator")
	close(bus.release)
	wg.Wait()
	if n := bus.count(); n != 0 {
		t.Errorf("%d frames delivered after the switch went off, want 0", n)
	}
	for _, e := range r.s.Recent() {
		if e.Kind == KindRaw && e.Outcome != OutcomeFailed {
			t.Errorf("held send row = %+v, want failed", e)
		}
	}
}

func TestFlipOffWaitsForASendThatCannotBeInterruptedUpToTheBound(t *testing.T) {
	bus := newHoldBus(false)
	r := rigWithBus(t, bus, 100*time.Millisecond)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.s.SendRaw(ctx, "a", rawBody("stuck", "_dev-a", 0, 1))
	}()
	<-bus.entered
	start := time.Now()
	r.s.SetPublishing(false, "operator")
	if d := time.Since(start); d < 90*time.Millisecond || d > 3*time.Second {
		t.Errorf("SetPublishing(false) took %v, want about the 100ms bound", d)
	}
	var off Entry
	for _, e := range r.s.Recent() {
		if e.Kind == KindSwitch {
			off = e
		}
	}
	if off.Outcome != "off" || !strings.Contains(off.Reason, "1 send") {
		t.Errorf("off row = %+v, want it to say 1 send is still in flight", off)
	}
	close(bus.release)
	<-done
	if n := bus.count(); n != 1 {
		t.Errorf("frames delivered = %d: the uninterruptible send is documented to still go out", n)
	}
}

func TestFlipOffReturnsAtOnceWhenNothingIsInFlightAndRecordsAfterTheSends(t *testing.T) {
	bus := newHoldBus(false)
	r := rigWithBus(t, bus, 5*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.s.SendRaw(ctx, "a", rawBody("slow", "_dev-a", 0, 1))
	}()
	<-bus.entered
	flipped := make(chan struct{})
	go func() {
		r.s.SetPublishing(false, "operator")
		close(flipped)
	}()
	select {
	case <-flipped:
		t.Fatal("SetPublishing(false) returned while a send was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := r.s.SendConnect(ctx, "a", "_dev-a", true); !errors.Is(err, ErrPublishingOff) {
		t.Errorf("a send while the flip waits: err = %v, want ErrPublishingOff at once", err)
	}
	close(bus.release)
	<-done
	<-flipped
	rows := r.s.Recent()
	if len(rows) != 2 || rows[0].Kind != KindSwitch || rows[1].Kind != KindRaw {
		t.Errorf("rows = %+v, want the off row newest, after the send it waited for", rows)
	}
}

// Item 3: a resolved outcome is kept on the row.
func TestResolvedOutcomeSurvivesTheControlPathRingEvictingIt(t *testing.T) {
	r := newRig(t, true)
	if _, err := r.s.SendRaw(ctx, "a", rawBody("keep", "_dev-a", 0, 1)); err != nil {
		t.Fatal(err)
	}
	r.obs.put(controlobs.MessageOutcome{DifferenceMRID: "keep", At: t0, Deltas: []controlobs.DeltaOutcome{{Result: controlobs.ResultIssued}}})
	if got := r.s.Recent()[0]; got.Outcome != OutcomeIssued || got.Deltas[0].Result != controlobs.ResultIssued {
		t.Fatalf("row = %+v, want issued", got)
	}
	r.obs.mu.Lock()
	r.obs.m = map[string]controlobs.MessageOutcome{}
	r.obs.mu.Unlock()
	r.clk.advance(time.Hour)
	if got := r.s.Recent()[0]; got.Outcome != OutcomeIssued || got.Deltas[0].Result != controlobs.ResultIssued {
		t.Errorf("after eviction row = %+v, want issued to stay", got)
	}
}

// Item 4: refusals spend no token and cannot evict sends.
func TestRefusedBodiesSpendNoRateTokens(t *testing.T) {
	r := newRig(t, true)
	for i := 0; i < 3*RateBurst; i++ {
		if _, err := r.s.SendRaw(ctx, "a", []byte(`{}`)); err == nil {
			t.Fatal("invalid body accepted")
		}
	}
	for i := 0; i < RateBurst; i++ {
		if _, err := r.s.SendConnect(ctx, "a", "_dev-a", true); err != nil {
			t.Fatalf("send %d after the refusals: %v", i, err)
		}
	}
}

func TestRefusalsCannotEvictSendsFromTheRecentList(t *testing.T) {
	r := newRig(t, true)
	if _, err := r.s.SendRaw(ctx, "a", rawBody("real", "_dev-a", 0, 1)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3*MaxRecent; i++ {
		_, _ = r.s.SendRaw(ctx, "a", []byte(`{}`))
	}
	found := false
	for _, e := range r.s.Recent() {
		if e.DifferenceMRID == "real" {
			found = true
		}
	}
	if !found {
		t.Error("a real send was pushed out of the recent list by refusals")
	}
	if n := len(r.s.Refusals()); n != MaxRecent {
		t.Errorf("%d refusals kept, want the newest %d", n, MaxRecent)
	}
}

// Item 5: duplicate keys at any level.
func TestDuplicateKeysAreRefusedWithTheirPath(t *testing.T) {
	const fwd = `{"object":"_dev-a","attribute":"` + AttrConnect + `","value":true}`
	tests := []struct {
		name, body, path string
	}{
		{"input twice", `{"command":"update","input":{"simulation_id":"1","message":{"difference_mrid":"m","forward_differences":[` + fwd + `]}},"input":{"message":{"difference_mrid":"m","forward_differences":[` + fwd + `]}}}`, "input"},
		{"message twice", `{"command":"update","input":{"message":{"difference_mrid":"m","forward_differences":[` + fwd + `]},"message":{"difference_mrid":"m","forward_differences":[` + fwd + `]}}}`, "input.message"},
		{"command twice", `{"command":"update","command":"update","input":{}}`, "command"},
		{"inside a forward difference", `{"command":"update","input":{"message":{"difference_mrid":"m","forward_differences":[{"object":"_dev-a","attribute":"` + AttrConnect + `","value":true,"value":false}]}}}`, "input.message.forward_differences[0].value"},
		{"inside a power value", `{"command":"update","input":{"message":{"difference_mrid":"m","forward_differences":[{"object":"_dev-a","attribute":"` + AttrActivePower + `","value":{"multiplier":0,"multiplier":1,"value":1}}]}}}`, "input.message.forward_differences[0].value.multiplier"},
		{"in the second entry", `{"command":"update","input":{"message":{"difference_mrid":"m","forward_differences":[` + fwd + `,{"object":"_dev-b","object":"_dev-b","attribute":"` + AttrConnect + `","value":true}]}}}`, "input.message.forward_differences[1].object"},
	}
	reg := newRegistry(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateRaw(reg, []byte(tc.body))
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Path != tc.path || !strings.Contains(ve.Msg, "duplicate") {
				t.Errorf("err = %v, want a duplicate-field refusal at %q", err, tc.path)
			}
		})
	}
}

// Item 6: pending expires.
func TestPendingBecomesNoOutcomeAfterTheBound(t *testing.T) {
	r := newRig(t, true)
	if _, err := r.s.SendRaw(ctx, "a", rawBody("lost", "_dev-a", 0, 1)); err != nil {
		t.Fatal(err)
	}
	r.clk.advance(PendingExpiry - time.Second)
	if got := r.s.Recent()[0].Outcome; got != OutcomePending {
		t.Errorf("just inside the bound: outcome = %q, want pending", got)
	}
	r.clk.advance(2 * time.Second)
	if got := r.s.Recent()[0].Outcome; got != OutcomeNone || OutcomeNone != "no outcome" {
		t.Errorf("past the bound: outcome = %q, want %q", got, "no outcome")
	}
	r.obs.put(controlobs.MessageOutcome{DifferenceMRID: "lost", At: r.clk.now(), Deltas: []controlobs.DeltaOutcome{{Result: controlobs.ResultIssued}}})
	if got := r.s.Recent()[0].Outcome; got != OutcomeIssued {
		t.Errorf("a late record: outcome = %q, want issued", got)
	}
}

// Mixed result text.
func TestRefusedRowSaysHowManyDifferencesIssued(t *testing.T) {
	r := newRig(t, true)
	body := []byte(`{"command":"update","input":{"message":{"difference_mrid":"m-mix","forward_differences":[` +
		`{"object":"_dev-a","attribute":"` + AttrConnect + `","value":true},{"object":"_dev-b","attribute":"` + AttrConnect + `","value":true}]}}}`)
	if _, err := r.s.SendRaw(ctx, "a", body); err != nil {
		t.Fatal(err)
	}
	r.obs.put(controlobs.MessageOutcome{DifferenceMRID: "m-mix", At: t0, Deltas: []controlobs.DeltaOutcome{
		{Result: controlobs.ResultIssued}, {Result: controlobs.ResultRefused, Reason: "no"}}})
	e := r.s.Recent()[0]
	if e.Outcome != OutcomeRefused || e.Reason != "1 of 2 differences issued, 0 restated, 1 refused" {
		t.Errorf("row = %+v", e)
	}
}

func TestRawMRIDOfASendStillInFlightIsRefused(t *testing.T) {
	bus := newHoldBus(false)
	r := rigWithBus(t, bus, time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.s.SendRaw(ctx, "a", rawBody("flying", "_dev-a", 0, 1))
	}()
	<-bus.entered
	if _, err := r.s.SendRaw(ctx, "b", rawBody("flying", "_dev-a", 0, 2)); !errors.Is(err, ErrDuplicateMRID) {
		t.Errorf("err = %v, want ErrDuplicateMRID while the first send is in flight", err)
	}
	close(bus.release)
	<-done
	if n := bus.count(); n != 1 {
		t.Errorf("%d frames delivered, want 1", n)
	}
}
