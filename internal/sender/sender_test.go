package sender

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
)

var ctx = context.Background()

func rawBody(mrid, obj string, mult, val int) []byte {
	return []byte(fmt.Sprintf(`{"command":"update","input":{"message":{"difference_mrid":%q,"forward_differences":[{"object":%q,"attribute":%q,"value":{"multiplier":%d,"value":%d}}]}}}`,
		mrid, obj, AttrActivePower, mult, val))
}

func TestNewRequiresItsDependencies(t *testing.T) {
	reg := newRegistry(t)
	tests := []struct {
		name string
		cfg  Config
	}{
		{"no bus", Config{Registry: reg, Destination: destTopic}},
		{"no registry", Config{Bus: &fakeBus{}, Destination: destTopic}},
		{"no destination", Config{Bus: &fakeBus{}, Registry: reg}},
	}
	for _, tc := range tests {
		if s, err := New(tc.cfg); err == nil || s != nil {
			t.Errorf("%s: got sender=%v err=%v, want an error", tc.name, s, err)
		}
	}
}

func TestSwitchIsOffAtStartUnlessOptedIn(t *testing.T) {
	off := newRig(t, false)
	if st := off.s.Publishing(); st.On || st.ChangedBy != "start" || !st.ChangedAt.Equal(t0) {
		t.Errorf("default state = %+v, want off, changed by start at %v", st, t0)
	}
	on := newRig(t, true)
	if st := on.s.Publishing(); !st.On || st.ChangedBy != "start" || !st.ChangedAt.Equal(t0) {
		t.Errorf("opted-in state = %+v, want on, changed by start at %v", st, t0)
	}
}

func TestFlipRecordsTimeAndRemoteAndIsAudited(t *testing.T) {
	r := newRig(t, false)
	r.clk.advance(90 * time.Second)
	st := r.s.SetPublishing(true, "10.0.0.7:5555")
	if !st.On || st.ChangedBy != "10.0.0.7:5555" || !st.ChangedAt.Equal(t0.Add(90*time.Second)) {
		t.Fatalf("state after flip = %+v", st)
	}
	if got := r.s.Publishing(); got != st {
		t.Errorf("Publishing() = %+v, want %+v", got, st)
	}
	lines := r.logs.all()
	if len(lines) != 1 {
		t.Fatalf("%d audit lines, want 1: %q", len(lines), lines)
	}
	for _, want := range []string{"kind=switch", `remote="10.0.0.7:5555"`, "old=off", "new=on", "changed=true", "at=2026-10-05T12:01:30Z"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("audit line %q lacks %q", lines[0], want)
		}
	}

	r.clk.advance(time.Minute)
	st2 := r.s.SetPublishing(false, "10.0.0.8:1")
	if st2.On || st2.ChangedBy != "10.0.0.8:1" || !st2.ChangedAt.Equal(t0.Add(150*time.Second)) {
		t.Errorf("state after second flip = %+v", st2)
	}
	recent := r.s.Recent()
	if len(recent) != 2 || recent[0].Kind != KindSwitch || recent[0].Outcome != "off" || recent[1].Outcome != "on" {
		t.Errorf("recent = %+v, want two switch rows, newest first, off then on", recent)
	}
}

func TestRepeatedSetKeepsTheOriginalFlipTimeAndRemote(t *testing.T) {
	r := newRig(t, false)
	first := r.s.SetPublishing(true, "a")
	r.clk.advance(time.Hour)
	again := r.s.SetPublishing(true, "b")
	if again != first {
		t.Errorf("state = %+v, want unchanged %+v", again, first)
	}
	lines := r.logs.all()
	if len(lines) != 2 || !strings.Contains(lines[1], "changed=false") || !strings.Contains(lines[1], `remote="b"`) {
		t.Errorf("a no-op set must still be audited, with changed=false: %q", lines)
	}
}

func TestSendWhileOffIsRefusedAndNothingIsPublished(t *testing.T) {
	r := newRig(t, false)
	sends := map[string]func() (Result, error){
		"raw":      func() (Result, error) { return r.s.SendRaw(ctx, "ra", rawBody("m", "_dev-a", 0, 1)) },
		"active":   func() (Result, error) { return r.s.SendActivePower(ctx, "ra", "_dev-a", 0, 1) },
		"reactive": func() (Result, error) { return r.s.SendReactivePower(ctx, "ra", "_dev-a", 0, 1) },
		"connect":  func() (Result, error) { return r.s.SendConnect(ctx, "ra", "_dev-a", true) },
		"energize": func() (Result, error) { return r.s.SendEnergize(ctx, "ra", "_dev-a", true) },
	}
	for name, send := range sends {
		res, err := send()
		if !errors.Is(err, ErrPublishingOff) || res != (Result{}) {
			t.Errorf("%s: got %+v, %v; want ErrPublishingOff and an empty result", name, res, err)
		}
	}
	if n := r.bus.count(); n != 0 {
		t.Errorf("%d frames reached the bus while publishing was off", n)
	}
	refusals := r.s.Refusals()
	if len(refusals) != len(sends) {
		t.Fatalf("%d refusal rows, want %d", len(refusals), len(sends))
	}
	if n := len(r.s.Recent()); n != 0 {
		t.Errorf("%d rows in the recent list, want refusals kept apart from it", n)
	}
	for _, e := range refusals {
		if e.Outcome != OutcomeRefused || e.Reason != "publishing is off" || e.Remote != "ra" {
			t.Errorf("row = %+v, want refused with reason %q from ra", e, "publishing is off")
		}
	}
}

func TestOffRefusalsSpendNoRateTokens(t *testing.T) {
	r := newRig(t, false)
	for i := 0; i < 20; i++ {
		_, _ = r.s.SendConnect(ctx, "x", "_dev-a", true)
	}
	r.s.SetPublishing(true, "x")
	for i := 0; i < RateBurst; i++ {
		if _, err := r.s.SendConnect(ctx, "x", "_dev-a", i%2 == 0); err != nil {
			t.Fatalf("send %d after switching on: %v", i, err)
		}
	}
}

func TestRawIsPublishedByteForByteToTheInputTopic(t *testing.T) {
	r := newRig(t, true)
	body := rawBody("keep-me", "_dev-a", -3, 250)
	res, err := r.s.SendRaw(ctx, "9.9.9.9:1", body)
	if err != nil {
		t.Fatal(err)
	}
	if res.DifferenceMRID != "keep-me" || res.Destination != destTopic {
		t.Errorf("result = %+v", res)
	}
	if r.bus.count() != 1 {
		t.Fatalf("%d frames, want 1", r.bus.count())
	}
	f := r.bus.sent[0]
	if f.dest != destTopic || f.contentType != "application/json" || string(f.body) != string(body) {
		t.Errorf("frame = %+v, want %s, application/json and the exact body", f, destTopic)
	}
}

func TestFormsPublishTheirExactMessageWithTheDeviceMRID(t *testing.T) {
	r := newRig(t, true)
	r.clk.advance(0)
	sends := []struct {
		name string
		send func() (Result, error)
		attr string
		val  string
	}{
		{"active", func() (Result, error) { return r.s.SendActivePower(ctx, "ra", "_dev-a", 2, -15) }, AttrActivePower, `{"multiplier":2,"value":-15}`},
		{"reactive", func() (Result, error) { return r.s.SendReactivePower(ctx, "ra", "_dev-b", -1, 7) }, AttrReactivePower, `{"multiplier":-1,"value":7}`},
		{"connect", func() (Result, error) { return r.s.SendConnect(ctx, "ra", "_dev-c", false) }, AttrConnect, `false`},
		{"energize", func() (Result, error) { return r.s.SendEnergize(ctx, "ra", "_dev-u", true) }, AttrEnergize, `true`},
	}
	objs := []string{"_dev-a", "_dev-b", "_dev-c", "_dev-u"}
	for i, tc := range sends {
		res, err := tc.send()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var msg diff.Message
		if err := json.Unmarshal(r.bus.sent[i].body, &msg); err != nil {
			t.Fatalf("%s: published body is not an input message: %v", tc.name, err)
		}
		p := msg.Input.Message
		if res.DifferenceMRID != p.DifferenceMRID || p.Timestamp != t0.Unix() || msg.Command != "update" || msg.Input.SimulationID != nil {
			t.Errorf("%s: result %+v envelope %+v", tc.name, res, msg)
		}
		got, _ := json.Marshal(p.ForwardDifferences[0].Value)
		if p.ForwardDifferences[0].Object != objs[i] || p.ForwardDifferences[0].Attribute != tc.attr || string(got) != tc.val {
			t.Errorf("%s: forward = %+v value %s, want %s %s %s", tc.name, p.ForwardDifferences[0], got, objs[i], tc.attr, tc.val)
		}
		if !strings.Contains(string(r.bus.sent[i].body), `"reverse_differences":[]`) {
			t.Errorf("%s: reverse_differences is not an empty array in %s", tc.name, r.bus.sent[i].body)
		}
	}
}

func TestFormFieldRangeAndUnknownDeviceAreRefusedBeforeTheBus(t *testing.T) {
	r := newRig(t, true)
	if _, err := r.s.SendActivePower(ctx, "ra", "_dev-a", 0, 40000); !errors.Is(err, ErrFieldRange) {
		t.Errorf("out-of-range value: err = %v, want ErrFieldRange", err)
	}
	_, err := r.s.SendConnect(ctx, "ra", "_nobody", true)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Path != "input.message.forward_differences[0].object" {
		t.Errorf("unknown device: err = %v, want a ValidationError at the object path", err)
	}
	if r.bus.count() != 0 {
		t.Errorf("%d frames published for refused forms", r.bus.count())
	}
	recent := r.s.Refusals()
	if len(recent) != 2 || recent[0].Outcome != OutcomeRefused || !strings.Contains(recent[0].Reason, "forward_differences[0].object") || recent[1].Outcome != OutcomeRefused {
		t.Errorf("refusals = %+v, want two refused rows naming the reason", recent)
	}
}

func TestInvalidRawIsRefusedWithItsPathAndNotPublished(t *testing.T) {
	r := newRig(t, true)
	_, err := r.s.SendRaw(ctx, "ra", []byte(`{"command":"update","input":{"message":{"difference_mrid":"m","forward_differences":[{"object":"_dev-a","attribute":"`+AttrActivePower+`","value":{"multiplier":2.5,"value":1}}]}}}`))
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Path != "input.message.forward_differences[0].value.multiplier" {
		t.Fatalf("err = %v, want the multiplier path", err)
	}
	if r.bus.count() != 0 {
		t.Error("an invalid body reached the bus")
	}
	if e := r.s.Refusals()[0]; e.Outcome != OutcomeRefused || !strings.Contains(e.Reason, "forward_differences[0].value.multiplier") {
		t.Errorf("row = %+v", e)
	}
}

func TestRateLimitIsOnePerSecondWithBurstFive(t *testing.T) {
	r := newRig(t, true)
	send := func() error { _, err := r.s.SendConnect(ctx, "ra", "_dev-a", true); return err }
	for i := 0; i < RateBurst; i++ {
		if err := send(); err != nil {
			t.Fatalf("burst send %d: %v", i, err)
		}
	}
	if err := send(); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("send %d: err = %v, want ErrRateLimited", RateBurst+1, err)
	}
	r.clk.advance(999 * time.Millisecond)
	if err := send(); !errors.Is(err, ErrRateLimited) {
		t.Errorf("after 999ms: err = %v, want ErrRateLimited", err)
	}
	r.clk.advance(time.Millisecond)
	if err := send(); err != nil {
		t.Errorf("after 1s: %v", err)
	}
	if err := send(); !errors.Is(err, ErrRateLimited) {
		t.Errorf("second send within the same second: err = %v, want ErrRateLimited", err)
	}
	if n := r.bus.count(); n != RateBurst+1 {
		t.Errorf("%d frames on the bus, want %d", n, RateBurst+1)
	}
}

func TestRateTokensDoNotAccumulateBeyondTheBurst(t *testing.T) {
	r := newRig(t, true)
	r.clk.advance(time.Hour)
	for i := 0; i < RateBurst; i++ {
		if _, err := r.s.SendConnect(ctx, "ra", "_dev-a", true); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if _, err := r.s.SendConnect(ctx, "ra", "_dev-a", true); !errors.Is(err, ErrRateLimited) {
		t.Errorf("send after an idle hour: err = %v, want ErrRateLimited at the burst", err)
	}
}

func TestRateLimitedSendIsRecordedAsRefused(t *testing.T) {
	r := newRig(t, true)
	for i := 0; i < RateBurst+1; i++ {
		_, _ = r.s.SendConnect(ctx, "ra", "_dev-a", true)
	}
	e := r.s.Refusals()[0]
	if e.Outcome != OutcomeRefused || e.Reason != ErrRateLimited.Error() || e.Destination != "" {
		t.Errorf("row = %+v, want refused: %s", e, ErrRateLimited)
	}
}

func TestBusFailureIsRecordedAsFailedAndWrapped(t *testing.T) {
	r := newRig(t, true)
	cause := errors.New("broker gone")
	r.bus.err = cause
	_, err := r.s.SendConnect(ctx, "ra", "_dev-a", true)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), destTopic) {
		t.Errorf("err = %v, want it to wrap the cause and name the destination", err)
	}
	e := r.s.Recent()[0]
	if e.Outcome != OutcomeFailed || e.Destination != destTopic || len(e.Deltas) != 1 || !strings.Contains(e.Reason, "broker gone") {
		t.Errorf("row = %+v, want failed, with the destination, the delta and the cause", e)
	}
}

func TestRecentKeepsTheNewestFiftyNewestFirst(t *testing.T) {
	r := newRig(t, true)
	total := MaxRecent + 7
	for i := 0; i < total; i++ {
		r.clk.advance(RateInterval)
		if _, err := r.s.SendRaw(ctx, "ra", rawBody(fmt.Sprintf("m-%03d", i), "_dev-a", 0, i)); err != nil {
			t.Fatal(err)
		}
	}
	recent := r.s.Recent()
	if len(recent) != MaxRecent {
		t.Fatalf("%d rows, want %d", len(recent), MaxRecent)
	}
	if recent[0].DifferenceMRID != fmt.Sprintf("m-%03d", total-1) || recent[MaxRecent-1].DifferenceMRID != fmt.Sprintf("m-%03d", total-MaxRecent) {
		t.Errorf("newest %q oldest %q, want m-%03d and m-%03d", recent[0].DifferenceMRID, recent[MaxRecent-1].DifferenceMRID, total-1, total-MaxRecent)
	}
}

func TestRecentRowCarriesTheAuditFields(t *testing.T) {
	r := newRig(t, true)
	r.clk.advance(5 * time.Second)
	if _, err := r.s.SendRaw(ctx, "1.2.3.4:99", rawBody("m-7", "_dev-b", 1, -9)); err != nil {
		t.Fatal(err)
	}
	e := r.s.Recent()[0]
	if !e.Time.Equal(t0.Add(5*time.Second)) || e.Kind != KindRaw || e.Remote != "1.2.3.4:99" || e.Destination != destTopic || e.DifferenceMRID != "m-7" {
		t.Errorf("row = %+v", e)
	}
	if len(e.Deltas) != 1 || e.Deltas[0].Object != "_dev-b" || e.Deltas[0].Attribute != AttrActivePower {
		t.Fatalf("deltas = %+v", e.Deltas)
	}
	if v, _ := json.Marshal(e.Deltas[0].Value); string(v) != `{"multiplier":1,"value":-9}` {
		t.Errorf("delta value = %s", v)
	}
	lines := r.logs.all()
	if len(lines) != 1 {
		t.Fatalf("%d audit lines, want 1", len(lines))
	}
	for _, want := range []string{"kind=raw", `remote="1.2.3.4:99"`, `destination="` + destTopic + `"`, `difference_mrid="m-7"`,
		`object="_dev-b"`, `attribute="` + AttrActivePower + `"`, `value="{\"multiplier\":1,\"value\":-9}"`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("audit line %q lacks %q", lines[0], want)
		}
	}
}

func TestAuditLineCannotBeForgedThroughTheRemoteAddress(t *testing.T) {
	r := newRig(t, true)
	r.s.SetPublishing(false, "1.1.1.1\nsender: audit kind=forged")
	if _, err := r.s.SendConnect(ctx, "2.2.2.2\nsender: audit kind=forged", "_dev-a", true); err == nil {
		t.Fatal("send while off succeeded")
	}
	for _, l := range r.logs.all() {
		if strings.Contains(l, "\n") {
			t.Errorf("audit entry spans lines: %q", l)
		}
	}
}

func TestOutcomeIsReadFromTheControlPath(t *testing.T) {
	deltaOf := func(res string) controlobs.DeltaOutcome {
		return controlobs.DeltaOutcome{Object: "_dev-a", Attribute: AttrActivePower, Result: res}
	}
	tests := []struct {
		name    string
		deltas  []controlobs.DeltaOutcome
		skip    bool
		want    string
		wantRes string
	}{
		{"issued", []controlobs.DeltaOutcome{deltaOf(controlobs.ResultIssued)}, false, OutcomeIssued, controlobs.ResultIssued},
		{"restated", []controlobs.DeltaOutcome{deltaOf(controlobs.ResultRestated)}, false, OutcomeRestated, controlobs.ResultRestated},
		{"refused with reason", []controlobs.DeltaOutcome{{Object: "_dev-a", Attribute: AttrActivePower, Result: controlobs.ResultRefused, Reason: "too fast"}}, false, OutcomeRefused, controlobs.ResultRefused},
		{"not recorded yet", nil, true, OutcomePending, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, true)
			if _, err := r.s.SendRaw(ctx, "ra", rawBody("m-o", "_dev-a", 0, 1)); err != nil {
				t.Fatal(err)
			}
			if got := r.s.Recent()[0].Outcome; got != OutcomePending {
				t.Fatalf("before the control path ran: outcome = %q, want pending", got)
			}
			if !tc.skip {
				r.obs.put(controlobs.MessageOutcome{DifferenceMRID: "m-o", At: t0.Add(time.Second), Deltas: tc.deltas})
			}
			e := r.s.Recent()[0]
			if e.Outcome != tc.want || e.Deltas[0].Result != tc.wantRes {
				t.Errorf("outcome = %q delta result = %q, want %q and %q", e.Outcome, e.Deltas[0].Result, tc.want, tc.wantRes)
			}
			if tc.name == "refused with reason" && e.Deltas[0].Reason != "too fast" {
				t.Errorf("delta reason = %q, want %q", e.Deltas[0].Reason, "too fast")
			}
		})
	}
}

func TestMixedOutcomeCountsAsRefusedWhenAnyDifferenceWasRefused(t *testing.T) {
	r := newRig(t, true)
	body := []byte(`{"command":"update","input":{"message":{"difference_mrid":"m-x","forward_differences":[` +
		`{"object":"_dev-a","attribute":"` + AttrConnect + `","value":true},{"object":"_dev-b","attribute":"` + AttrConnect + `","value":true}]}}}`)
	if _, err := r.s.SendRaw(ctx, "ra", body); err != nil {
		t.Fatal(err)
	}
	r.obs.put(controlobs.MessageOutcome{DifferenceMRID: "m-x", At: t0, Deltas: []controlobs.DeltaOutcome{
		{Result: controlobs.ResultIssued}, {Result: controlobs.ResultRefused, Reason: "no"}}})
	e := r.s.Recent()[0]
	if e.Outcome != OutcomeRefused || e.Deltas[0].Result != controlobs.ResultIssued || e.Deltas[1].Result != controlobs.ResultRefused {
		t.Errorf("row = %+v", e)
	}
}

func TestAnOutcomeOlderThanTheSendIsNotTheSendsOutcome(t *testing.T) {
	r := newRig(t, true)
	r.clk.advance(time.Minute)
	if _, err := r.s.SendRaw(ctx, "ra", rawBody("reused", "_dev-a", 0, 1)); err != nil {
		t.Fatal(err)
	}
	r.obs.put(controlobs.MessageOutcome{DifferenceMRID: "reused", At: t0.Add(-time.Hour), Deltas: []controlobs.DeltaOutcome{{Result: controlobs.ResultIssued}}})
	if got := r.s.Recent()[0].Outcome; got != OutcomePending {
		t.Errorf("outcome = %q, want pending: the only record predates the send", got)
	}
}

func TestOutcomeWithADifferentDeltaCountIsNotTrusted(t *testing.T) {
	r := newRig(t, true)
	if _, err := r.s.SendRaw(ctx, "ra", rawBody("m-n", "_dev-a", 0, 1)); err != nil {
		t.Fatal(err)
	}
	r.obs.put(controlobs.MessageOutcome{DifferenceMRID: "m-n", At: t0, Deltas: nil})
	if got := r.s.Recent()[0].Outcome; got != OutcomePending {
		t.Errorf("outcome = %q, want pending for a record with no deltas", got)
	}
}

func TestWithoutAnOutcomeSourceSendsStayPending(t *testing.T) {
	bus := &fakeBus{}
	s, err := New(Config{Bus: bus, Registry: newRegistry(t), Destination: destTopic, PublishAtStart: true, Now: (&clock{t: t0}).now, Logf: (&logSink{}).logf})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendConnect(ctx, "ra", "_dev-a", true); err != nil {
		t.Fatal(err)
	}
	if got := s.Recent()[0].Outcome; got != OutcomePending {
		t.Errorf("outcome = %q, want pending", got)
	}
}

func TestRecentIsACopy(t *testing.T) {
	r := newRig(t, true)
	if _, err := r.s.SendConnect(ctx, "ra", "_dev-a", true); err != nil {
		t.Fatal(err)
	}
	first := r.s.Recent()
	first[0].Remote = "tampered"
	first[0].Deltas[0].Object = "tampered"
	again := r.s.Recent()[0]
	if again.Remote != "ra" || again.Deltas[0].Object != "_dev-a" {
		t.Errorf("a caller changed the stored row: %+v", again)
	}
}

func TestDevicesAreListedByNameThenMRIDWithTheMRIDForUnnamed(t *testing.T) {
	r := newRig(t, false)
	got := r.s.Devices()
	want := []Device{{"Alpha", "_dev-a"}, {"Bravo", "_dev-b"}, {"Charlie", "_dev-c"}, {"_dev-u", "_dev-u"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("devices = %v, want %v", got, want)
	}
}

func TestConcurrentSendsAndFlipsRespectTheBurst(t *testing.T) {
	r := newRig(t, true)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%10 == 9 {
				r.s.SetPublishing(true, "flip")
			}
			if _, err := r.s.SendConnect(ctx, "ra", "_dev-a", true); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
			_ = r.s.Recent()
		}(i)
	}
	wg.Wait()
	if ok != RateBurst || r.bus.count() != RateBurst {
		t.Errorf("%d sends succeeded and %d reached the bus, want exactly %d at a frozen clock", ok, r.bus.count(), RateBurst)
	}
}
