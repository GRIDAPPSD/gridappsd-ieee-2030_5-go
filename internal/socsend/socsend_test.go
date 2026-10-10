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
	mu       sync.Mutex
	percent  map[string]uint16
	dateTime map[string]int64
	posts    int64
	err      error
}

// set stores hundredths for mrid as a device post: every call stamps a newer
// dateTime, so setting the value a device already holds is a re-post. A
// value that is not set again is not posted again.
func (f *fakeStatuses) set(mrid string, hundredths uint16) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.percent == nil {
		f.percent, f.dateTime = map[string]uint16{}, map[string]int64{}
	}
	f.posts++
	f.percent[mrid], f.dateTime[mrid] = hundredths, f.posts
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
			Status: sep2.DERStatus{StateOfChargeStatus: &sep2.StateOfChargeStatusType{Value: v, DateTime: f.dateTime[m]}}})
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
	if got.DeviceReported != nil {
		t.Errorf("deviceReportedPercent = %v, want none: 65 was the value before the send", *got.DeviceReported)
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
		{"output carried the value, device posted another", 7000, 80},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			ctx := context.Background()
			r.stat.set(testMRID, 6500)
			st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
			r.clk.advance(2 * time.Second)
			r.stat.set(testMRID, tc.devicePct)
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
	r.stat.set(testMRID, 6500)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.stat.set(testMRID, 8000)
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
	r.stat.set(testMRID, 6500)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.stat.set(testMRID, 8000)
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
	r.stat.set(testMRID, 6500)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.stat.set(testMRID, 8000)
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
	r.stat.set(testMRID, 6500)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.stat.set(testMRID, 8000)
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

func TestLedgerKeepsTheNewestSixtyFourFinishedEntries(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < MaxLedger+6; i++ {
		st, err := r.svc.Clear(ctx, testMRID)
		if err != nil {
			t.Fatalf("Clear %d: %v", i, err)
		}
		ids = append(ids, st.ID)
	}

	for i, id := range ids {
		_, ok := r.svc.Status(ctx, id)
		if want := i >= 6; ok != want {
			t.Errorf("clear %d: found=%v, want %v", i, ok, want)
		}
	}
}

func TestLedgerNeverEvictsASendStillAwaitingItsVerdict(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	first, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	var clears []string
	for i := 0; i < MaxLedger+6; i++ {
		st, _ := r.svc.Clear(ctx, testMRID)
		clears = append(clears, st.ID)
	}

	if got, ok := r.svc.Status(ctx, first.ID); !ok || got.Verdict != VerdictPending {
		t.Fatalf("the pending send was evicted by later entries: found=%v %+v", ok, got)
	}
	// The pending send takes one place; the oldest finished entries make room.
	for i, id := range clears {
		_, ok := r.svc.Status(ctx, id)
		if want := i >= 7; ok != want {
			t.Errorf("clear %d: found=%v, want %v", i, ok, want)
		}
	}

	var sends []string
	for i := 0; i < MaxLedger+6; i++ {
		st, _ := r.svc.Send(ctx, otherDevice, i%101, time.Minute)
		sends = append(sends, st.ID)
	}
	for i, id := range sends {
		if _, ok := r.svc.Status(ctx, id); !ok {
			t.Errorf("pending send %d was evicted", i)
		}
	}
}

func TestSendFinishedByItsVerdictBecomesEvictable(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	first, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.clk.advance(time.Minute + VerifyGrace)
	if got, _ := r.svc.Status(ctx, first.ID); got.Verdict != VerdictNotSeen {
		t.Fatalf("verdict %q, want not seen", got.Verdict)
	}
	for i := 0; i < MaxLedger; i++ {
		r.svc.Clear(ctx, testMRID)
	}

	if _, ok := r.svc.Status(ctx, first.ID); ok {
		t.Errorf("a send with its final verdict was kept past the ledger limit")
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

func TestDeviceAlreadyAtTheSentValueIsNotAPost(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.stat.set(testMRID, 8000)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.clk.advance(time.Second)
	r.svc.Status(ctx, st.ID)
	r.deliver(outputFrame(r.clk.now(), testMRID, 80))

	got, _ := r.svc.Status(ctx, st.ID)
	if got.PostedAt != nil || got.Verdict != VerdictPending {
		t.Fatalf("a value held before the send was taken as the post: %+v", got)
	}
	r.clk.advance(time.Minute + VerifyGrace)
	got, _ = r.svc.Status(ctx, st.ID)
	if got.PostedAt != nil || got.SeenAt == nil || got.Verdict != VerdictNotSeen {
		t.Errorf("final: %+v, want seen on the output, never posted, and not seen", got)
	}
}

func TestSameValuePostedAgainWithANewerDateTimeIsAPost(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.stat.set(testMRID, 8000)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.clk.advance(time.Second)
	r.stat.set(testMRID, 8000)

	got, _ := r.svc.Status(ctx, st.ID)

	if got.PostedAt == nil || !got.PostedAt.Equal(r.clk.now()) {
		t.Errorf("postedAt = %v, want %s for a re-post of the same value", got.PostedAt, r.clk.now())
	}
}

func TestValueHeldBeforeTheSendNeverCountsAsMismatch(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.stat.set(testMRID, 3000)
	st, _ := r.svc.Send(ctx, testMRID, 50, time.Minute)
	r.clk.advance(time.Second)
	r.svc.Status(ctx, st.ID)
	r.deliver(outputFrame(r.clk.now(), testMRID, 50))
	r.clk.advance(time.Minute + VerifyGrace)

	got, _ := r.svc.Status(ctx, st.ID)

	if got.Verdict != VerdictNotSeen || got.DeviceReported != nil {
		t.Errorf("verdict %q deviceReported %v, want not seen and no reported value: 30 was there before the send", got.Verdict, got.DeviceReported)
	}
}

func TestDeviceThatPostsAnotherValueAfterTheSendIsReported(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.stat.set(testMRID, 3000)
	st, _ := r.svc.Send(ctx, testMRID, 50, time.Minute)
	r.clk.advance(time.Second)
	r.stat.set(testMRID, 3000) // the same value, posted again, is still not 50

	got, _ := r.svc.Status(ctx, st.ID)

	if got.DeviceReported == nil || *got.DeviceReported != 30 {
		t.Errorf("deviceReported = %v, want 30 for a post after the send", got.DeviceReported)
	}
}

func TestBaselineUnreadableAtSendIsNotedOnceTheDevicePosts(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.stat.err = errors.New("store down")
	st, err := r.svc.Send(ctx, testMRID, 80, time.Minute)
	if err != nil {
		t.Fatalf("Send with an unreadable store: %v", err)
	}
	r.stat.err = nil
	r.stat.set(testMRID, 8000)

	got, _ := r.svc.Status(ctx, st.ID)

	if got.PostedAt == nil || !strings.Contains(got.Note, "before the send") {
		t.Errorf("postedAt %v note %q, want a post and a note that the earlier value was not read", got.PostedAt, got.Note)
	}
}

func TestStoreUnreadableForTheWholeWindowIsNoted(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.stat.set(testMRID, 6500)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.stat.err = errors.New("store down password=" + brokerLeak)
	r.clk.advance(2 * time.Second)
	r.svc.Status(ctx, st.ID)
	r.deliver(outputFrame(r.clk.now(), testMRID, 80))
	r.clk.advance(time.Minute + VerifyGrace)

	got, _ := r.svc.Status(ctx, st.ID)

	if got.Verdict != VerdictNotSeen || !strings.Contains(got.Note, "status store could not be read") {
		t.Errorf("verdict %q note %q, want not seen with the store note", got.Verdict, got.Note)
	}
	if strings.Contains(got.Note, brokerLeak) {
		t.Errorf("note %q carries the store error text", got.Note)
	}
}

func TestNoteIsEmptyWhenNothingWentWrong(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.stat.set(testMRID, 6500)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.clk.advance(time.Minute + VerifyGrace)

	got, _ := r.svc.Status(ctx, st.ID)

	if got.Verdict != VerdictNotSeen || got.Note != "" {
		t.Errorf("verdict %q note %q, want not seen with no note", got.Verdict, got.Note)
	}
}

func TestCutFrameNoteIsOmittedOnceTheValueWasSeen(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.stat.set(testMRID, 6500)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.svc.Status(ctx, st.ID)
	r.deliver(outputFrame(r.clk.now(), testMRID, 80))
	cut := `{"input":{"message":{"forward_differences":[{"object":"_other","attribute":"DERStatus.stateOfChargeStatus","value":1},{"object":"_ot`
	r.deliver(busmonitor.Message{Received: r.clk.now(), Body: []byte(cut), Truncated: true})
	r.clk.advance(time.Minute + VerifyGrace)

	got, _ := r.svc.Status(ctx, st.ID)

	if got.Verdict != VerdictNotSeen || got.SeenAt == nil || got.Note != "" {
		t.Errorf("verdict %q seenAt %v note %q, want not seen (never posted), seen, and no cut-frame note", got.Verdict, got.SeenAt, got.Note)
	}
}

// matchedSend returns a send of 80 held for a minute that the device has
// posted and the output topic has carried, at the clock's current time.
func matchedSend(t *testing.T, r *rig) Status {
	t.Helper()
	ctx := context.Background()
	r.stat.set(testMRID, 6500)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.clk.advance(time.Second)
	r.stat.set(testMRID, 8000)
	r.svc.Status(ctx, st.ID)
	r.deliver(outputFrame(r.clk.now(), testMRID, 80))
	got, _ := r.svc.Status(ctx, st.ID)
	if got.Verdict != VerdictMatched {
		t.Fatalf("setup: verdict %q, want matched: %+v", got.Verdict, got)
	}
	return st
}

func TestReleaseWaitsForTheHoldToEndExactly(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	st := matchedSend(t, r)
	holdEnd := st.SentAt.Add(time.Minute)

	r.clk.advance(30 * time.Second)
	r.stat.set(testMRID, 7000)
	if got, _ := r.svc.Status(ctx, st.ID); got.ReleasedAt != nil {
		t.Fatalf("released at %v during the hold: %+v", got.ReleasedAt, got)
	}

	r.clk.advance(holdEnd.Add(-time.Nanosecond).Sub(r.clk.now()))
	if got, _ := r.svc.Status(ctx, st.ID); got.ReleasedAt != nil {
		t.Fatalf("released a nanosecond before the hold ended: %+v", got)
	}

	r.clk.advance(time.Nanosecond)
	got, _ := r.svc.Status(ctx, st.ID)
	if got.ReleasedAt == nil || !got.ReleasedAt.Equal(holdEnd) {
		t.Errorf("releasedAt = %v, want the instant the hold ended, %s", got.ReleasedAt, holdEnd)
	}
}

func TestPostAfterTheDeadlineIsNotCountedEvenIfTheFrameCameInTime(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.stat.set(testMRID, 6500)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.svc.Status(ctx, st.ID)
	r.deliver(outputFrame(r.clk.now(), testMRID, 80))
	r.clk.advance(time.Minute + VerifyGrace)
	r.stat.set(testMRID, 8000)

	got, _ := r.svc.Status(ctx, st.ID)

	if got.PostedAt != nil || got.Verdict != VerdictNotSeen {
		t.Errorf("a post after the deadline counted: %+v", got)
	}
}

func TestOutputFrameAtTheDeadlineIsTooLateAndOneBeforeIsNot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		offset  time.Duration
		seen    bool
		verdict string
	}{
		{"at the deadline", 0, false, VerdictNotSeen},
		{"a nanosecond before", -time.Nanosecond, true, VerdictMatched},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			ctx := context.Background()
			r.stat.set(testMRID, 6500)
			st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
			r.clk.advance(time.Second)
			r.stat.set(testMRID, 8000)
			r.svc.Status(ctx, st.ID)
			deadline := st.SentAt.Add(time.Minute + VerifyGrace)
			r.deliver(outputFrame(deadline.Add(tc.offset), testMRID, 80))
			r.clk.advance(deadline.Sub(r.clk.now()))

			got, _ := r.svc.Status(ctx, st.ID)

			if (got.SeenAt != nil) != tc.seen || got.Verdict != tc.verdict {
				t.Errorf("seenAt %v verdict %q, want seen=%v verdict %q", got.SeenAt, got.Verdict, tc.seen, tc.verdict)
			}
		})
	}
}

func failedStatus() busmonitor.Event {
	return busmonitor.Event{Kind: busmonitor.EventStatus, Status: busmonitor.Status{State: busmonitor.StateFailed, Reason: "dial failed"}}
}

func countLogs(r *rig, sub string) int {
	n := 0
	for _, l := range r.logs {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func TestFailedOutputFeedIsLoggedNotedAndReopenedWithBackoff(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	r.stat.set(testMRID, 6500)
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)
	r.svc.Status(ctx, st.ID)
	first := r.watch.last()
	first.events <- failedStatus()

	got, _ := r.svc.Status(ctx, st.ID)

	if !first.closed {
		t.Errorf("the failed feed was left open")
	}
	if got.Verdict != VerdictPending || !strings.Contains(got.Note, "output topic feed") {
		t.Errorf("verdict %q note %q, want pending with a feed note", got.Verdict, got.Note)
	}
	if n := countLogs(r, "is failed"); n != 1 {
		t.Errorf("failure logged %d times, want once: %v", n, r.logs)
	}

	// No reopen inside the wait; one at 2 s.
	r.clk.advance(time.Second)
	r.svc.Status(ctx, st.ID)
	if len(r.watch.feeds) != 1 {
		t.Fatalf("feed reopened after 1 s, want a 2 s wait: %d feeds", len(r.watch.feeds))
	}
	r.clk.advance(time.Second)
	r.svc.Status(ctx, st.ID)
	if len(r.watch.feeds) != 2 {
		t.Fatalf("feeds = %d after the 2 s wait, want 2", len(r.watch.feeds))
	}
	if n := countLogs(r, "is failed"); n != 1 {
		t.Errorf("polling logged the failure again: %d lines", n)
	}

	// A second failure doubles the wait.
	r.watch.last().events <- failedStatus()
	r.svc.Status(ctx, st.ID)
	r.clk.advance(3 * time.Second)
	r.svc.Status(ctx, st.ID)
	if len(r.watch.feeds) != 2 {
		t.Fatalf("feed reopened after 3 s of a 4 s wait: %d feeds", len(r.watch.feeds))
	}
	r.clk.advance(time.Second)
	r.svc.Status(ctx, st.ID)
	if len(r.watch.feeds) != 3 {
		t.Fatalf("feeds = %d after the 4 s wait, want 3", len(r.watch.feeds))
	}

	// Live again: the note goes and the frame is read.
	r.watch.last().events <- busmonitor.Event{Kind: busmonitor.EventStatus, Status: busmonitor.Status{State: busmonitor.StateLive}}
	r.deliver(outputFrame(r.clk.now(), testMRID, 80))
	got, _ = r.svc.Status(ctx, st.ID)
	if got.Note != "" || got.SeenAt == nil {
		t.Errorf("note %q seenAt %v after the feed went live, want no note and the frame seen", got.Note, got.SeenAt)
	}
}

func TestFailedWatchBacksOffInsteadOfRetryingEveryPoll(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	w := &failingWatcher{}
	r.svc.cfg.Watcher = w
	st, _ := r.svc.Send(ctx, testMRID, 80, time.Minute)

	for i := 0; i < 3; i++ {
		r.svc.Status(ctx, st.ID)
	}
	if w.calls != 1 {
		t.Errorf("Watch called %d times in 3 polls inside the wait, want 1", w.calls)
	}
	got, _ := r.svc.Status(ctx, st.ID)
	if !strings.Contains(got.Note, "output topic feed") {
		t.Errorf("note %q, want the feed note after a failed Watch", got.Note)
	}
	r.clk.advance(2 * time.Second)
	r.svc.Status(ctx, st.ID)
	if w.calls != 2 {
		t.Errorf("Watch called %d times after the wait, want 2", w.calls)
	}
}

type failingWatcher struct{ calls int }

func (w *failingWatcher) Watch(string) (Feed, error) {
	w.calls++
	return nil, errors.New("monitor closed")
}
