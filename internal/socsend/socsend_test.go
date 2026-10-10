package socsend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/busmonitor"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

const (
	testMRID    = "_pv-1"
	testInput   = "/topic/goss.gridappsd.IEEE_2030_5.input"
	testOutput  = "/topic/goss.gridappsd.IEEE_2030_5.output"
	testLFDI    = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	brokerLeak  = "s3cret-broker-password"
	absentMRID  = "_nobody"
	otherDevice = "_pv-2"
)

type fakeBus struct {
	mu    sync.Mutex
	dests []string
	types []string
	sent  [][]byte
	err   error
}

func (b *fakeBus) Send(_ context.Context, dest, ct string, body []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	b.dests, b.types, b.sent = append(b.dests, dest), append(b.types, ct), append(b.sent, body)
	return nil
}

func (b *fakeBus) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sent)
}

type fakeStatuses struct {
	mu      sync.Mutex
	percent map[string]uint16
	err     error
}

func (f *fakeStatuses) set(mrid string, hundredths uint16) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.percent == nil {
		f.percent = map[string]uint16{}
	}
	f.percent[mrid] = hundredths
}

func (f *fakeStatuses) DERStatusSnapshots(context.Context) ([]sep2embed.DERStatusSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []sep2embed.DERStatusSnapshot
	for m, v := range f.percent {
		out = append(out, sep2embed.DERStatusSnapshot{MRID: m, EDevID: "e", DERID: "1",
			Status: sep2.DERStatus{StateOfChargeStatus: &sep2.StateOfChargeStatusType{Value: v}}})
	}
	return out, nil
}

type fakeFeed struct {
	backlog []busmonitor.Message
	events  chan busmonitor.Event
	closed  bool
}

func (f *fakeFeed) Backlog() []busmonitor.Message   { return f.backlog }
func (f *fakeFeed) Events() <-chan busmonitor.Event { return f.events }
func (f *fakeFeed) Close()                          { f.closed = true }

type fakeWatcher struct {
	mu     sync.Mutex
	feeds  []*fakeFeed
	topics []string
	next   *fakeFeed
}

func (w *fakeWatcher) Watch(name string) (Feed, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f := w.next
	if f == nil {
		f = &fakeFeed{events: make(chan busmonitor.Event, 64)}
	}
	w.next = nil
	w.feeds, w.topics = append(w.feeds, f), append(w.topics, name)
	return f, nil
}

func (w *fakeWatcher) last() *fakeFeed {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.feeds[len(w.feeds)-1]
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type rig struct {
	svc   *Service
	bus   *fakeBus
	stat  *fakeStatuses
	watch *fakeWatcher
	clk   *clock
	logs  []string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	reg := registry.New()
	for m, lfdi := range map[string]string{testMRID: testLFDI, otherDevice: strings.Repeat("B", 40)} {
		if err := reg.Add(registry.Entry{MRID: m, Name: m, LFDI: lfdi}); err != nil {
			t.Fatalf("registry add: %v", err)
		}
	}
	r := &rig{bus: &fakeBus{}, stat: &fakeStatuses{}, watch: &fakeWatcher{},
		clk: &clock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}}
	svc, err := New(Config{Bus: r.bus, Devices: reg, Statuses: r.stat, Watcher: r.watch,
		InputTopic: testInput, OutputTopic: testOutput, Now: r.clk.now,
		Logf: func(f string, a ...any) { r.logs = append(r.logs, fmt.Sprintf(f, a...)) }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.svc = svc
	return r
}

func outputFrame(at time.Time, mrid string, percent float64) busmonitor.Message {
	body := fmt.Sprintf(`{"command":"update","input":{"message":{"timestamp":1,"difference_mrid":"x","reverse_differences":[],"forward_differences":[`+
		`{"object":%q,"attribute":"DERStatus.readingTime","value":5},{"object":%q,"attribute":"DERStatus.stateOfChargeStatus","value":%g}]}}}`,
		mrid, mrid, percent)
	return busmonitor.Message{Destination: testOutput, Received: at, Size: len(body), Body: []byte(body)}
}

func (r *rig) deliver(m busmonitor.Message) {
	r.watch.last().events <- busmonitor.Event{Kind: busmonitor.EventMessage, Message: m}
}

func TestSendPublishesOneDifferenceWithValueAndHold(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	st, err := r.svc.Send(context.Background(), testMRID, 80, 90*time.Second)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if r.bus.count() != 1 || r.bus.dests[0] != testInput || r.bus.types[0] != "application/json" {
		t.Fatalf("published %d frames to %v (%v), want 1 to %s as application/json", r.bus.count(), r.bus.dests, r.bus.types, testInput)
	}
	var env struct {
		Input struct {
			SimulationID *string `json:"simulation_id"`
			Message      struct {
				Timestamp int64  `json:"timestamp"`
				ID        string `json:"difference_mrid"`
				Forward   []struct {
					Object    string `json:"object"`
					Attribute string `json:"attribute"`
					Value     struct {
						Percent *int `json:"percent"`
						Hold    *int `json:"hold_seconds"`
					} `json:"value"`
				} `json:"forward_differences"`
				Reverse json.RawMessage `json:"reverse_differences"`
			} `json:"message"`
		} `json:"input"`
	}
	if err := json.Unmarshal(r.bus.sent[0], &env); err != nil {
		t.Fatalf("frame is not JSON: %v", err)
	}
	m := env.Input.Message
	if len(m.Forward) != 1 {
		t.Fatalf("forward_differences has %d entries, want 1", len(m.Forward))
	}
	d := m.Forward[0]
	if d.Object != testMRID || d.Attribute != "DERStatus.stateOfChargeStatus" || d.Value.Percent == nil || *d.Value.Percent != 80 || d.Value.Hold == nil || *d.Value.Hold != 90 {
		t.Errorf("difference = %+v, want %s DERStatus.stateOfChargeStatus percent 80 hold 90", d, testMRID)
	}
	if string(m.Reverse) != "[]" {
		t.Errorf("reverse_differences = %s, want []", m.Reverse)
	}
	if env.Input.SimulationID != nil {
		t.Errorf("simulation_id set to %q, want absent on the application topic", *env.Input.SimulationID)
	}
	if m.Timestamp != r.clk.now().Unix() {
		t.Errorf("timestamp = %d, want send time %d", m.Timestamp, r.clk.now().Unix())
	}
	if st.ID == "" || st.ID != m.ID {
		t.Errorf("status id %q, want the frame's difference_mrid %q", st.ID, m.ID)
	}
	if st.Kind != KindSend || st.Percent != 80 || st.HoldSeconds != 90 || !st.SentAt.Equal(r.clk.now()) || st.Verdict != VerdictPending {
		t.Errorf("status = %+v", st)
	}
}

func TestClearPublishesHoldZero(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	st, err := r.svc.Clear(context.Background(), testMRID)
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}

	if !strings.Contains(string(r.bus.sent[0]), `"hold_seconds":0`) {
		t.Errorf("clear frame %s lacks hold_seconds 0", r.bus.sent[0])
	}
	if st.Kind != KindClear || st.HoldSeconds != 0 || st.Verdict != VerdictNone {
		t.Errorf("status = %+v, want a clear with hold 0 and verdict none", st)
	}
}

func TestSendRefusesBadInputBeforePublishing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		mrid    string
		percent int
		hold    time.Duration
		want    error
	}{
		{"percent above 100", testMRID, 101, time.Minute, ErrPercentRange},
		{"percent below 0", testMRID, -1, time.Minute, ErrPercentRange},
		{"hold zero", testMRID, 50, 0, ErrHoldRange},
		{"hold under a second", testMRID, 50, 999 * time.Millisecond, ErrHoldRange},
		{"hold over an hour", testMRID, 50, time.Hour + time.Second, ErrHoldRange},
		{"unregistered device", absentMRID, 50, time.Minute, ErrUnknownDevice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			_, err := r.svc.Send(context.Background(), tc.mrid, tc.percent, tc.hold)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if r.bus.count() != 0 {
				t.Errorf("%d frames published, want 0", r.bus.count())
			}
		})
	}
	t.Run("clear of an unregistered device", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		if _, err := r.svc.Clear(context.Background(), absentMRID); !errors.Is(err, ErrUnknownDevice) || r.bus.count() != 0 {
			t.Errorf("err = %v, frames = %d, want ErrUnknownDevice and 0", err, r.bus.count())
		}
	})
	t.Run("boundaries 0 and 100, 1 s and 1 h are accepted", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		for _, c := range []struct {
			p int
			h time.Duration
		}{{0, MinHold}, {100, MaxHold}} {
			if _, err := r.svc.Send(context.Background(), testMRID, c.p, c.h); err != nil {
				t.Errorf("Send(%d, %s): %v", c.p, c.h, err)
			}
		}
	})
}

func TestFailedPublishRecordsNothingAndLeaksNoTransportText(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.bus.err = errors.New("stomp: connect user=bridge password=" + brokerLeak)

	st, err := r.svc.Send(context.Background(), testMRID, 50, time.Minute)

	if !errors.Is(err, ErrPublishFailed) {
		t.Fatalf("err = %v, want ErrPublishFailed", err)
	}
	if strings.Contains(err.Error(), brokerLeak) {
		t.Errorf("error text %q carries the broker credential", err)
	}
	if st.ID != "" {
		t.Errorf("status %+v returned for a failed publish", st)
	}
	if _, ok := r.svc.Status(context.Background(), st.ID); ok {
		t.Errorf("a failed publish left a ledger entry")
	}
	r.svc.mu.Lock()
	n := len(r.svc.ledger)
	r.svc.mu.Unlock()
	if n != 0 {
		t.Errorf("ledger holds %d entries after a failed publish, want 0", n)
	}
}

func TestRoundTripMatchedWithTimesAndRelease(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.stat.set(testMRID, 6500)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)

	// The device still reports its own value: nothing posted yet.
	r.clk.advance(5 * time.Second)
	got, ok := r.svc.Status(ctx, st.ID)
	if !ok || got.PostedAt != nil || got.Verdict != VerdictPending {
		t.Fatalf("before the device posts: %+v ok=%v", got, ok)
	}
	if got.DeviceReported == nil || *got.DeviceReported != 65 {
		t.Errorf("deviceReportedPercent = %v, want 65", got.DeviceReported)
	}

	// The device posts the value; the output topic has not carried it.
	r.clk.advance(10 * time.Second)
	postedAt := r.clk.now()
	r.stat.set(testMRID, 8000)
	got, _ = r.svc.Status(ctx, st.ID)
	if got.PostedAt == nil || !got.PostedAt.Equal(postedAt) || got.SeenAt != nil || got.Verdict != VerdictPending {
		t.Fatalf("after the device posts: %+v", got)
	}

	// An output frame for another device, and an older frame for this one, do not count.
	r.deliver(outputFrame(r.clk.now(), otherDevice, 80))
	r.deliver(outputFrame(st.SentAt.Add(-time.Second), testMRID, 80))
	r.clk.advance(time.Second)
	got, _ = r.svc.Status(ctx, st.ID)
	if got.SeenAt != nil {
		t.Fatalf("seenAt set by a frame for another device or from before the send: %+v", got)
	}

	// The frame for this device with this value is seen at its receipt time.
	seenAt := r.clk.now()
	r.deliver(outputFrame(seenAt, testMRID, 80))
	got, _ = r.svc.Status(ctx, st.ID)
	if got.SeenAt == nil || !got.SeenAt.Equal(seenAt) || got.Verdict != VerdictMatched {
		t.Fatalf("after the frame: %+v", got)
	}
	if got.ReleasedAt != nil {
		t.Errorf("releasedAt = %v while the hold is running", got.ReleasedAt)
	}

	// After the hold the device reports another value: released.
	r.clk.advance(time.Minute)
	releasedAt := r.clk.now()
	r.stat.set(testMRID, 6600)
	got, _ = r.svc.Status(ctx, st.ID)
	if got.ReleasedAt == nil || !got.ReleasedAt.Equal(releasedAt) || got.Verdict != VerdictMatched {
		t.Errorf("after the hold: %+v, want released at %s and still matched", got, releasedAt)
	}
	if got.Percent != 80 || got.HoldSeconds != 60 || got.MRID != testMRID {
		t.Errorf("identity fields changed: %+v", got)
	}
	if r.watch.topics[0] != testOutput {
		t.Errorf("watched %q, want the output topic %q", r.watch.topics[0], testOutput)
	}
}

func TestVerdictNotSeenOnlyAfterHoldPlusGrace(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.stat.set(testMRID, 6500)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)

	r.clk.advance(time.Minute + VerifyGrace - time.Second)
	if got, _ := r.svc.Status(ctx, st.ID); got.Verdict != VerdictPending {
		t.Errorf("verdict %q just before hold + 60 s, want pending", got.Verdict)
	}
	r.clk.advance(time.Second)
	got, _ := r.svc.Status(ctx, st.ID)
	if got.Verdict != VerdictNotSeen {
		t.Errorf("verdict %q at hold + 60 s with nothing returned, want %q", got.Verdict, VerdictNotSeen)
	}

	// A value that arrives after the verdict does not change it.
	r.stat.set(testMRID, 8000)
	r.deliver(outputFrame(r.clk.now(), testMRID, 80))
	if again, _ := r.svc.Status(ctx, st.ID); again.Verdict != VerdictNotSeen || again.PostedAt != nil {
		t.Errorf("a late value changed the final verdict: %+v", again)
	}
}

func TestVerdictMismatchWhenStagesDisagree(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		devicePct uint16
		outputPct float64
	}{
		{"device posted the value, output carried another", 8000, 70},
		{"output carried the value, device reports another", 6500, 80},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			ctx := context.Background()
			r.stat.set(testMRID, tc.devicePct)
			st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
			r.clk.advance(2 * time.Second)
			r.svc.Status(ctx, st.ID) // opens the feed
			r.deliver(outputFrame(r.clk.now(), testMRID, tc.outputPct))
			r.clk.advance(time.Minute + VerifyGrace)

			got, _ := r.svc.Status(ctx, st.ID)

			if got.Verdict != VerdictMismatch {
				t.Errorf("verdict %q, want mismatch: %+v", got.Verdict, got)
			}
		})
	}
}

func TestOutputFrameCutAtTheMonitorLimitIsStillRead(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.stat.set(testMRID, 8000)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.svc.Status(ctx, st.ID)

	full := outputFrame(r.clk.now(), testMRID, 80)
	body := string(full.Body)
	cut := body[:strings.Index(body, `"attribute":"DERStatus.stateOfChargeStatus"`)+len(`"attribute":"DERStatus.stateOfChargeStatus","value":80}`)]
	cut += `,{"object":"_other","attribute":"DERStatus.read`
	r.deliver(busmonitor.Message{Received: r.clk.now(), Body: []byte(cut), Truncated: true, Size: 99999})

	got, _ := r.svc.Status(ctx, st.ID)
	if got.SeenAt == nil || got.Verdict != VerdictMatched {
		t.Errorf("a cut frame holding this device's value was not read: %+v", got)
	}
}

func TestCutFrameWithoutTheDeviceExplainsNotSeen(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.stat.set(testMRID, 8000)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.svc.Status(ctx, st.ID)
	cut := `{"input":{"message":{"forward_differences":[{"object":"_other","attribute":"DERStatus.stateOfChargeStatus","value":1},{"object":"_ot`
	r.deliver(busmonitor.Message{Received: r.clk.now(), Body: []byte(cut), Truncated: true})
	r.clk.advance(time.Minute + VerifyGrace)

	got, _ := r.svc.Status(ctx, st.ID)

	if got.Verdict != VerdictNotSeen || !strings.Contains(got.Note, fmt.Sprint(busmonitor.MaxBodyBytes)) {
		t.Errorf("verdict %q note %q, want not seen with the frame-limit note", got.Verdict, got.Note)
	}
}

func TestBacklogAtOpenIsReadAndEarlierFramesAreNot(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.stat.set(testMRID, 8000)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.clk.advance(time.Second)
	r.watch.next = &fakeFeed{events: make(chan busmonitor.Event, 4), backlog: []busmonitor.Message{
		outputFrame(st.SentAt.Add(-time.Minute), testMRID, 80),
		outputFrame(r.clk.now(), testMRID, 80),
	}}

	got, _ := r.svc.Status(ctx, st.ID)

	if got.SeenAt == nil || !got.SeenAt.Equal(r.clk.now()) {
		t.Errorf("seenAt = %v, want the post-send backlog frame at %s", got.SeenAt, r.clk.now())
	}
}

func TestFeedClosedByTheMonitorIsReopened(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.stat.set(testMRID, 8000)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.svc.Status(ctx, st.ID)
	close(r.watch.last().events)
	r.svc.Status(ctx, st.ID)
	r.svc.Status(ctx, st.ID)

	if len(r.watch.feeds) != 2 {
		t.Fatalf("feeds opened = %d, want 2 (reopened once after the close)", len(r.watch.feeds))
	}
	r.deliver(outputFrame(r.clk.now(), testMRID, 80))
	if got, _ := r.svc.Status(ctx, st.ID); got.SeenAt == nil {
		t.Errorf("frame on the reopened feed not seen: %+v", got)
	}
}

func TestFeedIsClosedWhenNothingIsPending(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.stat.set(testMRID, 6500)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.svc.Status(ctx, st.ID)
	feed := r.watch.last()
	r.clk.advance(time.Minute + VerifyGrace)
	r.svc.Status(ctx, st.ID)
	r.svc.Status(ctx, st.ID)

	if !feed.closed {
		t.Errorf("feed still open after the only send reached its verdict")
	}
}

func TestLedgerKeepsTheNewestSixtyFour(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < MaxLedger+6; i++ {
		st, err := r.svc.Send(ctx, testMRID, i%101, time.Minute)
		if err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
		ids = append(ids, st.ID)
	}

	for i, id := range ids {
		_, ok := r.svc.Status(ctx, id)
		if want := i >= 6; ok != want {
			t.Errorf("send %d: found=%v, want %v", i, ok, want)
		}
	}
}

func TestStatusOfAnUnknownIDIsNotFound(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if _, ok := r.svc.Status(context.Background(), "nope"); ok {
		t.Errorf("found a status for an id never issued")
	}
}

func TestStatusReadFailureLeavesTheSendPendingAndIsLogged(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.stat.err = errors.New("store down")

	got, _ := r.svc.Status(ctx, st.ID)

	if got.Verdict != VerdictPending || len(r.logs) == 0 || !strings.Contains(r.logs[0], "store down") {
		t.Errorf("verdict %q logs %v, want pending and the store error logged", got.Verdict, r.logs)
	}
}

func TestRunFollowsASendWithNobodyAsking(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.svc.cfg.PollInterval = 5 * time.Millisecond
	r.stat.set(testMRID, 6500)
	st, _ := r.svc.Send(context.Background(), testMRID, 80, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.svc.Run(ctx) }()

	r.stat.set(testMRID, 8000)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.svc.mu.Lock()
		posted := r.svc.ledger[0].PostedAt != nil
		r.svc.mu.Unlock()
		if posted {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v on cancel", err)
	}

	r.svc.mu.Lock()
	defer r.svc.mu.Unlock()
	if r.svc.ledger[0].ID != st.ID || r.svc.ledger[0].PostedAt == nil {
		t.Errorf("Run did not record the post: %+v", r.svc.ledger[0].Status)
	}
	if !r.watch.last().closed {
		t.Errorf("Run left the output feed open on cancel")
	}
}

func TestNewRequiresItsDependencies(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}); err == nil {
		t.Errorf("New accepted an empty config")
	}
	if _, err := New(Config{Bus: &fakeBus{}, Devices: registry.New(), Statuses: &fakeStatuses{}}); err == nil {
		t.Errorf("New accepted a config with no input topic")
	}
}
