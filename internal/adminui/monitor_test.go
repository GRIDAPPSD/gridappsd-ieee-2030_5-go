package adminui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/busmonitor"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp/cimstomptest"
)

// monitorBroker hands each Subscribe a fresh in-memory subscription the
// test drives.
type monitorBroker struct {
	mu   sync.Mutex
	subs map[string]chan monitorSub
}

type monitorSub struct {
	sub *cimstomp.Subscription
	in  chan<- cimstomp.Message
}

func (b *monitorBroker) ch(dest string) chan monitorSub {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs == nil {
		b.subs = map[string]chan monitorSub{}
	}
	c, ok := b.subs[dest]
	if !ok {
		c = make(chan monitorSub, 16)
		b.subs[dest] = c
	}
	return c
}

func (b *monitorBroker) next(t *testing.T, dest string) monitorSub {
	t.Helper()
	select {
	case s := <-b.ch(dest):
		return s
	case <-time.After(3 * time.Second):
		t.Fatalf("no subscription on %s", dest)
		return monitorSub{}
	}
}

func (b *monitorBroker) dial(context.Context) (busmonitor.Session, error) {
	return monitorSession{b}, nil
}

type monitorSession struct{ b *monitorBroker }

func (s monitorSession) Subscribe(_ context.Context, dest string, _ ...cimstomp.SubscribeOption) (*cimstomp.Subscription, error) {
	sub, in := cimstomptest.NewSubscription()
	s.b.ch(dest) <- monitorSub{sub: sub, in: in}
	return sub, nil
}

func (monitorSession) Close() error { return nil }

const testProbe = "/topic/goss.gridappsd.heartbeat"

func newTestMonitor(t *testing.T) (*busmonitor.Monitor, *monitorBroker) {
	t.Helper()
	b := &monitorBroker{}
	m := busmonitor.New(context.Background(), busmonitor.Config{
		Dial:  b.dial,
		Probe: func() string { return testProbe },
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	t.Cleanup(m.Close)
	return m, b
}

type wireStreamEvent struct {
	id    uint64
	Time  string `json:"time"`
	Kind  string `json:"kind"`
	Text  string `json:"text"`
	Final bool   `json:"final"`
}

// streamReader reads SSE events from a panel stream the way EventSource
// does: an "id:" line, then a "data:" line, then a blank line.
type streamReader struct {
	t *testing.T
	r *bufio.Reader
}

func (sr streamReader) next() wireStreamEvent {
	sr.t.Helper()
	type res struct {
		ev  wireStreamEvent
		err error
	}
	got := make(chan res, 1)
	go func() {
		var ev wireStreamEvent
		for {
			line, err := sr.r.ReadString('\n')
			if err != nil {
				got <- res{err: err}
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case strings.HasPrefix(line, "id: "):
				ev.id, err = strconv.ParseUint(strings.TrimPrefix(line, "id: "), 10, 64)
				if err != nil {
					got <- res{err: err}
					return
				}
			case strings.HasPrefix(line, "data: "):
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
					got <- res{err: err}
					return
				}
			case line == "" && ev.Kind != "":
				got <- res{ev: ev}
				return
			}
		}
	}()
	select {
	case r := <-got:
		if r.err != nil {
			sr.t.Fatalf("read stream event: %v", r.err)
		}
		return r.ev
	case <-time.After(3 * time.Second):
		sr.t.Fatal("no stream event within 3s")
		return wireStreamEvent{}
	}
}

// openMonitorStream opens the bus monitor stream for topic through the
// full listener handler with the Bearer key, as a browser tab would with
// its session.
func openMonitorStream(t *testing.T, baseURL, topic, lastEventID string) streamReader {
	t.Helper()
	return openMonitorStreamWithKey(t, baseURL, topic, lastEventID, testKey)
}

func openMonitorStreamWithKey(t *testing.T, baseURL, topic, lastEventID, key string) streamReader {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/ui/panels/"+panelBusMonitor+"/stream?param="+url.QueryEscape(topic), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer "+key)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET stream: status %d", resp.StatusCode)
	}
	return streamReader{t: t, r: bufio.NewReader(resp.Body)}
}

func monitorServer(t *testing.T, mon MonitorSource) (*Server, *httptest.Server) {
	t.Helper()
	src := testSources()
	src.Monitor = mon
	s := newServer(t, Config{Key: testKey}, src)
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	return s, hs
}

// TestMonitorPanelIsTheEighthTabWithATopicStream: with a monitor the
// manifest gains the bus monitor after the seven read-only tabs, declaring
// a stream whose parameter admits every character of a topic name.
func TestMonitorPanelIsTheEighthTabWithATopicStream(t *testing.T) {
	t.Parallel()
	mon, _ := newTestMonitor(t)
	s, _ := monitorServer(t, mon)

	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels", "Bearer "+testKey, "localhost")
	var manifest []struct {
		ID     string `json:"id"`
		Label  string `json:"label"`
		Stream *struct {
			MaxLen  int    `json:"maxLen"`
			Charset string `json:"charset"`
		} `json:"stream"`
	}
	decodeJSON(t, rec.Body.Bytes(), &manifest)
	if len(manifest) != 8 {
		t.Fatalf("manifest has %d panels, want 8: %+v", len(manifest), manifest)
	}
	got := manifest[7]
	if got.ID != panelBusMonitor || got.Label != "Bus monitor" || got.Stream == nil {
		t.Fatalf("panel 8 = %+v, want %s \"Bus monitor\" with a stream", got, panelBusMonitor)
	}
	if got.Stream.MaxLen != 207 {
		t.Errorf("stream maxLen = %d, want 207 (/topic/ plus 200)", got.Stream.MaxLen)
	}
	for _, c := range "/topic/goss.gridappsd.*.>_-AZ09" {
		if !strings.ContainsRune(got.Stream.Charset, c) {
			t.Errorf("charset %q lacks %q", got.Stream.Charset, c)
		}
	}
	for _, c := range " ?&%#\\" {
		if strings.ContainsRune(got.Stream.Charset, c) {
			t.Errorf("charset %q admits %q", got.Stream.Charset, c)
		}
	}
	for i := range 7 {
		if manifest[i].Stream != nil {
			t.Errorf("panel %s declares a stream", manifest[i].ID)
		}
	}
}

// TestMonitorStreamReplaysThenPushesMessagesAndStatus: a viewer gets the
// buffer, the topic's state, then each new frame and each drop, in order,
// with the values the monitor recorded.
func TestMonitorStreamReplaysThenPushesMessagesAndStatus(t *testing.T) {
	t.Parallel()
	mon, b := newTestMonitor(t)
	_, hs := monitorServer(t, mon)
	const topic = "/topic/test.panel"

	// A first viewer fills the buffer before the browser arrives.
	first, err := mon.Watch(topic)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	sub := b.next(t, topic)
	sub.in <- cimstomp.Message{Destination: topic, Body: []byte(`{ "a" : 1 }`)}
	sub.in <- cimstomp.Message{Destination: topic, Body: []byte("two")}
	for n := 0; n < 3; {
		ev := <-first.Events()
		n++ // live, message, message
		_ = ev
	}

	sr := openMonitorStream(t, hs.URL, topic, "")
	m1, m2, join := sr.next(), sr.next(), sr.next()
	if m1.Kind != "message" || m1.Text != topic+` 11 bytes: {"a":1}` {
		t.Errorf("replay 1 = %+v", m1)
	}
	if m2.Kind != "message" || m2.Text != topic+" 3 bytes: two" {
		t.Errorf("replay 2 = %+v", m2)
	}
	if join.Kind != "status" || join.Text != "live" {
		t.Errorf("join = %+v, want status live", join)
	}
	if !(m1.id < m2.id && m2.id < join.id) {
		t.Errorf("ids %d, %d, %d: want rising", m1.id, m2.id, join.id)
	}

	sub.in <- cimstomp.Message{Destination: topic, Body: []byte("three")}
	live := sr.next()
	if live.Kind != "message" || live.Text != topic+" 5 bytes: three" || live.id <= join.id {
		t.Errorf("live = %+v, want message three above id %d", live, join.id)
	}
	if _, err := time.Parse(time.RFC3339Nano, live.Time); err != nil {
		t.Errorf("live time %q: %v", live.Time, err)
	}

	cimstomptest.SetErr(sub.sub, errors.New("read timeout"))
	close(sub.in)
	drop := sr.next()
	if drop.Kind != "status" || !strings.HasPrefix(drop.Text, "reconnecting (try 1, in ") || !strings.HasSuffix(drop.Text, ": read timeout") {
		t.Errorf("drop = %+v, want a reconnecting status naming the cause", drop)
	}
	if again := sr.next(); again.Kind != "status" || again.Text != "live" {
		t.Errorf("after reconnect = %+v, want status live", again)
	}
}

// TestMonitorStreamRefusalFreesItsSlot: a topic the monitor will not watch
// fails the open, so it opens no connection and holds none of the plane's
// eight stream slots.
func TestMonitorStreamRefusalFreesItsSlot(t *testing.T) {
	t.Parallel()
	mon, _ := newTestMonitor(t)
	_, hs := monitorServer(t, mon)

	refused := []string{"/queue/x", testProbe, testProbe + ".>", "/topic/pnnl.goss.token.topic", "/topic/ActiveMQ.Advisory.Conn"}
	for i := 0; i < 9; i++ {
		topic := refused[i%len(refused)]
		req, err := http.NewRequest(http.MethodGet, hs.URL+"/api/ui/panels/"+panelBusMonitor+"/stream?param="+url.QueryEscape(topic), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "localhost"
		req.Header.Set("Authorization", "Bearer "+testKey)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		resp, err := http.DefaultClient.Do(req.WithContext(ctx))
		if err != nil {
			cancel()
			t.Fatalf("%s: %v", topic, err)
		}
		// The body is left open on purpose: a stream that was accepted
		// would keep its slot for as long as this connection lives.
		t.Cleanup(func() { _ = resp.Body.Close(); cancel() })
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("%s: 200, want the open refused", topic)
		}
	}
	if got := mon.Topics(); len(got) != 0 {
		t.Errorf("refused topics opened %+v", got)
	}
	if ev := openMonitorStream(t, hs.URL, "/topic/test.after-refusals", "").next(); ev.Kind != "status" {
		t.Errorf("valid stream after nine refusals: %+v", ev)
	}
}

// TestMonitorStoppedViewIDCannotHideALaterMessage: the status for a view
// the monitor stopped takes its own number from the monitor's sequence, so
// a browser resuming from it still replays the next message.
func TestMonitorStoppedViewIDCannotHideALaterMessage(t *testing.T) {
	t.Parallel()
	mon, b := newTestMonitor(t)
	s, hs := monitorServer(t, mon)
	const topic = "/topic/test.stopped"

	sr := openMonitorStream(t, hs.URL, topic, "")
	join := sr.next()
	b.next(t, topic)
	// Another topic numbers events after this stream's last one; closing
	// the monitor then stops this view.
	other, err := mon.Watch("/topic/test.other")
	if err != nil {
		t.Fatal(err)
	}
	osub := b.next(t, "/topic/test.other")
	osub.in <- cimstomp.Message{Destination: "/topic/test.other", Body: []byte("o")}
	var otherSeq uint64
	for ev := range other.Events() {
		if ev.Kind == busmonitor.EventMessage {
			otherSeq = ev.Seq
			break
		}
	}
	mon.Close()
	for {
		ev := sr.next()
		if ev.Kind == "status" && strings.HasPrefix(ev.Text, "monitor stopped this view: monitor closed") {
			if ev.id <= join.id || ev.id <= uint64(s.startedAt.UnixMicro())+otherSeq {
				t.Fatalf("stopped id %d: want above the join %d and above every event the monitor numbered before (other topic at %d)",
					ev.id, join.id, uint64(s.startedAt.UnixMicro())+otherSeq)
			}
			return
		}
	}
}

// TestMonitorStreamResumesAcrossABridgeRestart: a browser that reconnects
// with an event ID from the previous bridge process still gets this
// process's buffer, because IDs are offset by the panel's start time.
func TestMonitorStreamResumesAcrossABridgeRestart(t *testing.T) {
	t.Parallel()
	mon, b := newTestMonitor(t)
	s, hs := monitorServer(t, mon)
	const topic = "/topic/test.resume"

	first, err := mon.Watch(topic)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	sub := b.next(t, topic)
	sub.in <- cimstomp.Message{Destination: topic, Body: []byte("kept")}
	for range 2 {
		<-first.Events()
	}

	// An hour-old process that had sent a thousand events.
	previous := uint64(s.startedAt.Add(-time.Hour).UnixMicro()) + 1000
	ev := openMonitorStream(t, hs.URL, topic, strconv.FormatUint(previous, 10)).next()
	if ev.Kind != "message" || ev.Text != topic+" 4 bytes: kept" {
		t.Fatalf("first event after resume = %+v, want the buffered message", ev)
	}
}

// TestRunEndsMonitorStreamsWithAFinalStatus: shutdown tells an open
// stream it is over, so the browser does not reconnect, and Run returns
// promptly.
func TestRunEndsMonitorStreamsWithAFinalStatus(t *testing.T) {
	t.Parallel()
	mon, _ := newTestMonitor(t)
	src := testSources()
	src.Monitor = mon
	s := newServer(t, Config{Key: testKey}, src)
	s.timeouts.shutdown = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	sr := openMonitorStream(t, "http://"+s.Addr(), "/topic/test.shutdown", "")
	if ev := sr.next(); ev.Text != "connecting" && ev.Text != "live" {
		t.Fatalf("join = %+v", ev)
	}
	start := time.Now()
	cancel()
	for {
		ev := sr.next()
		if ev.Final {
			if ev.Kind != "status" || ev.Text != "stream closed: server shutting down" {
				t.Errorf("final = %+v", ev)
			}
			break
		}
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("Run took %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestDisplayBody(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("y", 3000)
	cases := []struct {
		name string
		msg  busmonitor.Message
		want string
	}{
		{"json is compacted", busmonitor.Message{Size: 17, Body: []byte("{\n  \"k\": [1, 2]\n}")}, `{"k":[1,2]}`},
		{"text keeps its spaces, loses its line breaks", busmonitor.Message{Size: 7, Body: []byte("a b\nc\x1bd")}, "a b c d"},
		{"bidi controls cannot reorder the line", busmonitor.Message{Size: 5, Body: []byte("a\xe2\x80\xaeb")}, "a b"},
		{"long text is cut at 2 KiB", busmonitor.Message{Size: 3000, Body: []byte(long)},
			long[:2048] + " ... (2048 of 3000 bytes shown)"},
		{"binary is size and hex prefix", busmonitor.Message{Size: 40, Body: append([]byte{0xff, 0xfe, 0x00}, make([]byte, 37)...)},
			"binary, 40 bytes, hex fffe00" + strings.Repeat("00", 29) + "..."},
		{"a body cut inside a rune is still text", busmonitor.Message{Size: 9000, Truncated: true, Body: []byte("ab\xc3")},
			"ab ... (2 of 9000 bytes shown)"},
	}
	for _, tc := range cases {
		if got := displayBody(tc.msg); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestStatusText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		st   busmonitor.Status
		want string
	}{
		{busmonitor.Status{State: busmonitor.StateLive}, "live"},
		{busmonitor.Status{State: busmonitor.StateReconnecting, Attempt: 2, Retry: 2 * time.Second, Reason: "eof"}, "reconnecting (try 2, in 2s): eof"},
		{busmonitor.Status{State: busmonitor.StateRefused, Reason: "access denied"}, "refused by the broker: access denied; press Start to try again"},
		{busmonitor.Status{State: busmonitor.StateFailed, Reason: "gave up"}, "stopped: gave up; press Start to try again"},
	} {
		if got := statusText(tc.st); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.st, got, tc.want)
		}
	}
}
