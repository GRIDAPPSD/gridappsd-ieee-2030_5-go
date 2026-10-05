package busmonitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp"
	"github.com/go-stomp/stomp/v3"
	"github.com/go-stomp/stomp/v3/frame"
)

func TestValidateTopic(t *testing.T) {
	const probe = "/topic/goss.gridappsd.heartbeat"
	long200 := "/topic/" + strings.Repeat("a", 200)
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"plain topic", "/topic/goss.gridappsd.process.log", nil},
		{"every allowed class", "/topic/Az09_-.x", nil},
		{"200 chars", long200, nil},
		{"201 chars", long200 + "a", ErrInvalidTopic},
		{"empty name", "/topic/", ErrInvalidTopic},
		{"trailing wildcard", "/topic/test.>", nil},
		{"deep trailing wildcard", "/topic/goss.gridappsd.simulation.>", nil},
		{"lone trailing wildcard", "/topic/>", ErrProbeTopic},
		{"star segment", "/topic/test.*", nil},
		{"star mid segment", "/topic/goss.*.log", nil},
		{"star then trailing", "/topic/goss.*.>", ErrProbeTopic},
		{"mid-pattern angle", "/topic/goss.>.simulation", ErrInvalidTopic},
		{"double angle", "/topic/goss.>.>", ErrInvalidTopic},
		{"angle inside segment", "/topic/goss.a>", ErrInvalidTopic},
		{"star inside segment", "/topic/goss.a*", ErrInvalidTopic},
		{"double star segment", "/topic/goss.**", ErrInvalidTopic},
		{"empty segment", "/topic/goss..log", ErrInvalidTopic},
		{"leading dot", "/topic/.goss", ErrInvalidTopic},
		{"trailing dot", "/topic/goss.", ErrInvalidTopic},
		{"dot before angle only", "/topic/.>", ErrInvalidTopic},
		{"queue", "/queue/goss.gridappsd.process.request", ErrInvalidTopic},
		{"queue wildcard", "/queue/>", ErrInvalidTopic},
		{"temp queue", "/temp-queue/x", ErrInvalidTopic},
		{"bare name", "goss.gridappsd", ErrInvalidTopic},
		{"space", "/topic/a b", ErrInvalidTopic},
		{"trailing newline", "/topic/abc\n", ErrInvalidTopic},
		{"trailing newline after angle", "/topic/abc.>\n", ErrInvalidTopic},
		{"nul byte", "/topic/a\x00b", ErrInvalidTopic},
		{"slash inside", "/topic/a/b", ErrInvalidTopic},
		{"non-ascii", "/topic/caf\u00e9", ErrInvalidTopic},
		{"probe topic", probe, ErrProbeTopic},
		{"wildcard matching probe", "/topic/goss.gridappsd.>", ErrProbeTopic},
		{"star matching probe", "/topic/goss.gridappsd.*", ErrProbeTopic},
		{"top wildcard matching probe", "/topic/>", ErrProbeTopic},
		{"wildcard not matching probe", "/topic/goss.gridappsd.simulation.>", nil},
		{"star with wrong depth", "/topic/goss.*", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ValidateTopic(c.in, probe); !errors.Is(got, c.want) {
				t.Fatalf("ValidateTopic(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
	for _, c := range cases {
		if c.want == ErrInvalidTopic {
			continue
		}
		if err := ValidateTopic(c.in, ""); err != nil {
			t.Fatalf("with no probe set %q is valid, got %v", c.in, err)
		}
	}
	if err := ValidateTopic("/topic/>", ""); err != nil {
		t.Fatalf("with no probe set a wildcard is valid, got %v", err)
	}
}

func TestWildcardFramesCarryTheirDestination(t *testing.T) {
	b := newBroker()
	m := newTestMonitor(t, b, nil)
	v, _ := m.Watch("/topic/test.>")
	r := b.next(t, "/topic/test.>")
	r.in <- cimstomp.Message{Destination: "/topic/test.a.b", Body: []byte("x")}
	got := waitMessages(t, v, 1)[0]
	if got.Destination != "/topic/test.a.b" {
		t.Fatalf("destination = %q", got.Destination)
	}
}

func TestWatchRefusesBeforeDialing(t *testing.T) {
	b := newBroker()
	m := newTestMonitor(t, b, func(c *Config) { c.Probe = func() string { return "/topic/probe" } })
	for _, name := range []string{"/queue/x", "/topic/a.>.b", "/topic/probe"} {
		if _, err := m.Watch(name); err == nil {
			t.Fatalf("Watch(%q) accepted", name)
		}
	}
	if n := b.dials.Load(); n != 0 {
		t.Fatalf("dials = %d, want 0 for refused names", n)
	}
	if len(m.Topics()) != 0 {
		t.Fatalf("refused names left topics: %v", m.Topics())
	}
}

func TestDefaultsMatchDesign(t *testing.T) {
	c := (Config{}).withDefaults()
	if c.IdleClose != 30*time.Second || c.BackoffMin != time.Second || c.BackoffMax != 30*time.Second || c.MaxTries != 5 {
		t.Fatalf("defaults = idle %v backoff %v..%v tries %d", c.IdleClose, c.BackoffMin, c.BackoffMax, c.MaxTries)
	}
	if MaxTopics != 8 || RingSize != 200 || MaxBodyBytes != 8192 {
		t.Fatalf("limits = %d topics, ring %d, body %d", MaxTopics, RingSize, MaxBodyBytes)
	}
}

func TestEachTopicGetsItsOwnConnection(t *testing.T) {
	b := newBroker()
	m := newTestMonitor(t, b, nil)
	va, err := m.Watch("/topic/a")
	if err != nil {
		t.Fatal(err)
	}
	vb, err := m.Watch("/topic/b")
	if err != nil {
		t.Fatal(err)
	}
	ra, rb := b.next(t, "/topic/a"), b.next(t, "/topic/b")
	if ra.sess == rb.sess {
		t.Fatal("two topics share one session")
	}
	if n := b.dials.Load(); n != 2 {
		t.Fatalf("dials = %d, want 2", n)
	}
	// A second viewer on a topic shares its connection.
	va2, err := m.Watch("/topic/a")
	if err != nil {
		t.Fatal(err)
	}
	if n := b.dials.Load(); n != 2 {
		t.Fatalf("second viewer dialed: dials = %d, want 2", n)
	}
	ra.send([]byte("one"))
	for _, v := range []*Viewer{va, va2} {
		if got := string(waitMessages(t, v, 1)[0].Body); got != "one" {
			t.Fatalf("viewer got %q", got)
		}
	}
	rb.send([]byte("two"))
	if got := string(waitMessages(t, vb, 1)[0].Body); got != "two" {
		t.Fatalf("b got %q", got)
	}
}

func TestBrokerRefusalClosesOnlyThatConnection(t *testing.T) {
	b := newBroker()
	b.refuse.Store("/topic/denied", refusalErr("User system is not authorized to read from: topic://denied"))
	m := newTestMonitor(t, b, nil)

	good, err := m.Watch("/topic/good")
	if err != nil {
		t.Fatal(err)
	}
	rg := b.next(t, "/topic/good")
	waitStatus(t, good, StateLive)

	bad, err := m.Watch("/topic/denied")
	if err != nil {
		t.Fatal(err)
	}
	st := waitStatus(t, bad, StateRefused)
	if !strings.Contains(st.Reason, "not authorized to read from: topic://denied") {
		t.Fatalf("reason = %q, want the broker's message", st.Reason)
	}
	if !strings.Contains(st.Reason, "detail") || strings.ContainsAny(st.Reason, "\x00\x01") {
		t.Fatalf("reason = %q, want body text without control bytes", st.Reason)
	}

	// The healthy topic's connection is untouched and still delivers.
	rg.send([]byte("still here"))
	if got := string(waitMessages(t, good, 1)[0].Body); got != "still here" {
		t.Fatalf("good topic got %q", got)
	}
	if rg.sess.closed.Load() {
		t.Fatal("refusal closed the healthy topic's session")
	}
	// No retry of a refused topic until a viewer asks again.
	if n := b.dials.Load(); n != 2 {
		t.Fatalf("dials = %d after refusal, want 2 (no retry)", n)
	}
	if bad2, err := m.Watch("/topic/denied"); err != nil {
		t.Fatal(err)
	} else {
		waitStatus(t, bad2, StateRefused)
	}
	if n := b.dials.Load(); n != 3 {
		t.Fatalf("dials = %d after a new viewer, want 3", n)
	}
}

func TestLocallyBuiltErrorFrameIsADrop(t *testing.T) {
	bare := &stomp.Error{Message: "connection closed", Frame: frame.New(frame.ERROR, frame.Message, "connection closed")}
	if reason, ok := refusal(bare); ok {
		t.Fatalf("go-stomp's own ERROR frame classified as refusal: %q", reason)
	}
	f := frame.New(frame.ERROR, frame.Message, "not authorized", frame.ContentType, "text/plain")
	if reason, ok := refusal(&stomp.Error{Message: "not authorized", Frame: f}); !ok || reason != "not authorized" {
		t.Fatalf("broker-shaped ERROR frame: reason %q ok %v", reason, ok)
	}
}

func TestTransportDropIsNotARefusal(t *testing.T) {
	if reason, ok := refusal(io.EOF); ok {
		t.Fatalf("EOF classified as refusal: %q", reason)
	}
	if _, ok := refusal(fmt.Errorf("wrap: %w", refusalErr("denied"))); !ok {
		t.Fatal("wrapped broker ERROR not classified as refusal")
	}
}

func TestReconnectBackoffAndGiveUp(t *testing.T) {
	b := newBroker()
	var mu sync.Mutex
	var delays []time.Duration
	m := newTestMonitor(t, b, func(c *Config) {
		c.Sleep = func(_ context.Context, d time.Duration) error {
			mu.Lock()
			delays = append(delays, d)
			mu.Unlock()
			return nil
		}
	})
	b.dialErr.Store(errors.New("connection refused"))
	v, err := m.Watch("/topic/flaky")
	if err != nil {
		t.Fatal(err)
	}
	st := waitStatus(t, v, StateFailed)
	if !strings.Contains(st.Reason, "gave up after 5 reconnect tries") || !strings.Contains(st.Reason, "connection refused") {
		t.Fatalf("reason = %q", st.Reason)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	if fmt.Sprint(delays) != fmt.Sprint(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	if n := b.dials.Load(); n != 6 {
		t.Fatalf("dials = %d, want 1 initial plus 5 tries", n)
	}
}

func TestBackoffCapsAtThirtySeconds(t *testing.T) {
	b := newBroker()
	var mu sync.Mutex
	var delays []time.Duration
	m := newTestMonitor(t, b, func(c *Config) {
		c.MaxTries = 8
		c.Sleep = func(_ context.Context, d time.Duration) error {
			mu.Lock()
			delays = append(delays, d)
			mu.Unlock()
			return nil
		}
	})
	b.dialErr.Store(errors.New("down"))
	v, _ := m.Watch("/topic/down")
	waitStatus(t, v, StateFailed)
	mu.Lock()
	defer mu.Unlock()
	if got := delays[len(delays)-1]; got != 30*time.Second {
		t.Fatalf("last delay = %v, want 30s cap (all %v)", got, delays)
	}
}

func TestDropThenReconnectResumesDelivery(t *testing.T) {
	b := newBroker()
	m := newTestMonitor(t, b, nil)
	v, _ := m.Watch("/topic/t")
	first := b.next(t, "/topic/t")
	waitStatus(t, v, StateLive)
	first.end(io.EOF)
	st := waitStatus(t, v, StateReconnecting)
	if st.Attempt != 1 || st.Retry != time.Second {
		t.Fatalf("status = %+v, want attempt 1 retry 1s", st)
	}
	second := b.next(t, "/topic/t")
	if second.sess == first.sess || !first.sess.closed.Load() {
		t.Fatal("reconnect reused the dead session")
	}
	second.send([]byte("back"))
	if got := string(waitMessages(t, v, 1)[0].Body); got != "back" {
		t.Fatalf("got %q", got)
	}
}

func TestRingKeepsNewest200(t *testing.T) {
	b := newBroker()
	m := newTestMonitor(t, b, nil)
	v, _ := m.Watch("/topic/r")
	r := b.next(t, "/topic/r")
	for i := 1; i <= 250; i++ {
		r.send([]byte(fmt.Sprintf("m%d", i)))
	}
	waitMessages(t, v, 250)

	late, err := m.Watch("/topic/r")
	if err != nil {
		t.Fatal(err)
	}
	bl := late.Backlog()
	if len(bl) != RingSize {
		t.Fatalf("backlog = %d, want %d", len(bl), RingSize)
	}
	if bl[0].Seq != 51 || string(bl[0].Body) != "m51" || bl[199].Seq != 250 || string(bl[199].Body) != "m250" {
		t.Fatalf("backlog spans %d/%s .. %d/%s, want 51/m51 .. 250/m250", bl[0].Seq, bl[0].Body, bl[199].Seq, bl[199].Body)
	}
	// Replay then live: the next frame arrives after the backlog with the next seq.
	r.send([]byte("m251"))
	got := waitMessages(t, late, 1)[0]
	if got.Seq != 251 || string(got.Body) != "m251" {
		t.Fatalf("live after replay = %d/%s", got.Seq, got.Body)
	}
}

func TestBodyTruncatedWithTrueSize(t *testing.T) {
	b := newBroker()
	m := newTestMonitor(t, b, nil)
	v, _ := m.Watch("/topic/big")
	r := b.next(t, "/topic/big")
	big := []byte(strings.Repeat("x", 20000))
	exact := []byte(strings.Repeat("y", MaxBodyBytes))
	r.send(big)
	r.send(exact)
	got := waitMessages(t, v, 2)
	if len(got[0].Body) != MaxBodyBytes || got[0].Size != 20000 || !got[0].Truncated {
		t.Fatalf("big: body %d size %d truncated %v", len(got[0].Body), got[0].Size, got[0].Truncated)
	}
	if len(got[1].Body) != MaxBodyBytes || got[1].Size != MaxBodyBytes || got[1].Truncated {
		t.Fatalf("exact: body %d size %d truncated %v", len(got[1].Body), got[1].Size, got[1].Truncated)
	}
}

func TestAtMostEightTopics(t *testing.T) {
	b := newBroker()
	m := newTestMonitor(t, b, nil)
	for i := 0; i < MaxTopics; i++ {
		if _, err := m.Watch(fmt.Sprintf("/topic/t%d", i)); err != nil {
			t.Fatalf("topic %d: %v", i, err)
		}
	}
	if _, err := m.Watch("/topic/t8"); !errors.Is(err, ErrTooManyTopics) {
		t.Fatalf("ninth topic: %v, want ErrTooManyTopics", err)
	}
	for i := 0; i < MaxTopics; i++ {
		b.next(t, fmt.Sprintf("/topic/t%d", i))
	}
	if n := b.dials.Load(); n != 8 {
		t.Fatalf("dials = %d, want 8", n)
	}
	// An existing topic is still reachable at the cap.
	if _, err := m.Watch("/topic/t3"); err != nil {
		t.Fatalf("existing topic at cap: %v", err)
	}
}

func TestClosesAfterLastViewerLeaves(t *testing.T) {
	b := newBroker()
	m := newTestMonitor(t, b, func(c *Config) { c.IdleClose = 80 * time.Millisecond })
	v1, _ := m.Watch("/topic/i")
	v2, _ := m.Watch("/topic/i")
	r := b.next(t, "/topic/i")
	v1.Close()
	time.Sleep(200 * time.Millisecond)
	if r.sess.closed.Load() {
		t.Fatal("closed while a viewer remained")
	}
	v2.Close()
	deadline := time.Now().Add(3 * time.Second)
	for !r.sess.closed.Load() || len(m.Topics()) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("not closed after idle: closed=%v topics=%v", r.sess.closed.Load(), m.Topics())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestViewerReturnCancelsIdleClose(t *testing.T) {
	b := newBroker()
	m := newTestMonitor(t, b, func(c *Config) { c.IdleClose = 150 * time.Millisecond })
	v1, _ := m.Watch("/topic/k")
	r := b.next(t, "/topic/k")
	v1.Close()
	time.Sleep(60 * time.Millisecond)
	v2, _ := m.Watch("/topic/k")
	time.Sleep(300 * time.Millisecond)
	if r.sess.closed.Load() || len(m.Topics()) != 1 {
		t.Fatalf("closed despite a returning viewer: closed=%v topics=%v", r.sess.closed.Load(), m.Topics())
	}
	if n := b.dials.Load(); n != 1 {
		t.Fatalf("dials = %d, want 1", n)
	}
	v2.Close()
}

func TestSlowViewerIsClosedNotBlocking(t *testing.T) {
	b := newBroker()
	m := newTestMonitor(t, b, nil)
	slow, _ := m.Watch("/topic/s")
	fast, _ := m.Watch("/topic/s")
	r := b.next(t, "/topic/s")
	total := viewerBuffer + 50
	go func() {
		for i := 0; i < total; i++ {
			r.send([]byte("x"))
		}
	}()
	waitMessages(t, fast, total)
	deadline := time.Now().Add(3 * time.Second)
	for slow.Err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("slow viewer never closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !errors.Is(slow.Err(), ErrSlowViewer) {
		t.Fatalf("slow viewer err = %v", slow.Err())
	}
}

func TestMonitorCloseClosesEverything(t *testing.T) {
	b := newBroker()
	m := New(context.Background(), Config{Dial: b.dial})
	v, _ := m.Watch("/topic/c")
	r := b.next(t, "/topic/c")
	m.Close()
	if !r.sess.closed.Load() {
		t.Fatal("session left open after Close")
	}
	for range v.Events() {
		// drain buffered status events; the range ends only when Close closed the channel
	}
	if !errors.Is(v.Err(), ErrClosed) {
		t.Fatalf("viewer err = %v", v.Err())
	}
	if _, err := m.Watch("/topic/c"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Watch after Close: %v", err)
	}
}

func TestCleanReason(t *testing.T) {
	if got := cleanReason("a\x00b\nc\u00e9"); got != "a b c??" {
		t.Fatalf("cleanReason = %q", got)
	}
	if got := cleanReason(strings.Repeat("z", 2000)); len(got) != maxReasonBytes {
		t.Fatalf("len = %d, want %d", len(got), maxReasonBytes)
	}
}
