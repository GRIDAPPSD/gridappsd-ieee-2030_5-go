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
	req, err := http.NewRequest(http.MethodGet, baseURL+monitorPath("stream")+"?param="+url.QueryEscape(topic), nil)
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

func monitorPath(file string) string { return "/apps/" + panelBusMonitor + "/" + file }

// TestMonitorPanelIsTheSixthTabAsAPage: with a monitor the manifest gains
// the bus monitor after the five fixed tabs, served as a page that declares
// no stream; the plane mounts no descriptor or stream route for it.
func TestMonitorPanelIsTheSixthTabAsAPage(t *testing.T) {
	t.Parallel()
	mon, _ := newTestMonitor(t)
	s, _ := monitorServer(t, mon)

	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels", "Bearer "+testKey, "localhost")
	var manifest []struct {
		ID     string          `json:"id"`
		Label  string          `json:"label"`
		Page   string          `json:"page"`
		Stream json.RawMessage `json:"stream"`
	}
	decodeJSON(t, rec.Body.Bytes(), &manifest)
	if len(manifest) != 6 {
		t.Fatalf("manifest has %d panels, want 6: %+v", len(manifest), manifest)
	}
	got := manifest[5]
	if got.ID != panelBusMonitor || got.Label != "Bus monitor" || got.Page != monitorPath("") {
		t.Fatalf("panel 6 = %+v, want %s \"Bus monitor\" with page %s", got, panelBusMonitor, monitorPath(""))
	}
	for _, m := range manifest {
		if len(m.Stream) != 0 {
			t.Errorf("panel %s declares a stream", m.ID)
		}
	}
	p := s.panels()[5]
	if p.Page == nil || p.View != nil || p.Stream != nil {
		t.Errorf("panel 6 Page set = %t, View set = %t, Stream set = %t, want a page panel only", p.Page != nil, p.View != nil, p.Stream != nil)
	}
	for _, route := range []string{"", "/stream"} {
		if rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels/"+panelBusMonitor+route, "Bearer "+testKey, "localhost"); rec.Code == http.StatusOK {
			t.Errorf("descriptor route %q = %d, want it not served", route, rec.Code)
		}
	}
}

func TestMonitorPageServesItsFilesAndNeedsTheCredential(t *testing.T) {
	t.Parallel()
	mon, _ := newTestMonitor(t)
	s, _ := monitorServer(t, mon)

	for _, f := range []struct{ path, ctype, marker string }{
		{"", "text/html", `src="app.js"`},
		{"app.js", "text/javascript", "EventSource"},
		{"style.css", "text/css", "body"},
	} {
		rec := doRequest(t, s.Handler(), http.MethodGet, monitorPath(f.path), "Bearer "+testKey, "localhost")
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), f.ctype) || !strings.Contains(rec.Body.String(), f.marker) {
			t.Errorf("GET %q: status %d, Content-Type %q, marker %q present %t", f.path, rec.Code, rec.Header().Get("Content-Type"), f.marker, strings.Contains(rec.Body.String(), f.marker))
		}
		csp := strings.Join(rec.Header().Values("Content-Security-Policy"), "; ")
		for _, want := range []string{"script-src 'self'", "style-src 'self'", "connect-src 'self'", "frame-ancestors 'self'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("GET %q: CSP %q lacks %q", f.path, csp, want)
			}
		}
	}
	for _, f := range []string{"", "topics", "stream?param=/topic/x", "app.js"} {
		rec := doRequest(t, s.Handler(), http.MethodGet, monitorPath(f), "", "localhost")
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden && rec.Code != http.StatusSeeOther {
			t.Errorf("GET %q with no credential = %d, want a refusal", f, rec.Code)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, f := range []string{"topics", "stream?param=/topic/x"} {
			if rec := doRequest(t, s.Handler(), m, monitorPath(f), "Bearer "+testKey, "localhost"); rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 405: the page is read only", m, f, rec.Code)
			}
		}
	}
}

// TestMonitorTopicsListsTheWatchedTopics reads the serialized bytes: no
// topic is an empty array, and a watched one carries its state and counts.
func TestMonitorTopicsListsTheWatchedTopics(t *testing.T) {
	t.Parallel()
	mon, b := newTestMonitor(t)
	s, _ := monitorServer(t, mon)

	rec := doRequest(t, s.Handler(), http.MethodGet, monitorPath("topics"), "Bearer "+testKey, "localhost")
	if !strings.Contains(rec.Body.String(), `"topics":[]`) || strings.Contains(rec.Body.String(), "null") {
		t.Errorf("no topic watched: body %s, want an empty array and no null", rec.Body.String())
	}

	const topic = "/topic/test.listed"
	v, err := mon.Watch(topic)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	sub := b.next(t, topic)
	sub.in <- cimstomp.Message{Destination: topic, Body: []byte("one")}
	for n := 0; n < 2; n++ {
		<-v.Events()
	}
	var got monitorTopics
	decodeJSON(t, doRequest(t, s.Handler(), http.MethodGet, monitorPath("topics"), "Bearer "+testKey, "localhost").Body.Bytes(), &got)
	if len(got.Topics) != 1 || got.Topics[0] != (monitorTopicRow{Name: topic, State: "live", Viewers: 1, Buffered: 1}) {
		t.Errorf("topics = %+v, want the one live topic with 1 viewer and 1 buffered message", got.Topics)
	}
	if len(got.Prose) != 2 || !strings.Contains(got.Prose[0], "At most 8 topics") || !strings.Contains(got.Prose[1], "newest 200 messages") {
		t.Errorf("prose = %q, want the usage text with the monitor's bounds", got.Prose)
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
// is refused before the stream starts, so it opens no connection and holds
// none of the eight stream slots.
func TestMonitorStreamRefusalFreesItsSlot(t *testing.T) {
	t.Parallel()
	mon, _ := newTestMonitor(t)
	_, hs := monitorServer(t, mon)

	refused := []string{"/queue/x", testProbe, testProbe + ".>", "/topic/pnnl.goss.token.topic", "/topic/ActiveMQ.Advisory.Conn"}
	for i := 0; i < 9; i++ {
		topic := refused[i%len(refused)]
		req, err := http.NewRequest(http.MethodGet, hs.URL+monitorPath("stream")+"?param="+url.QueryEscape(topic), nil)
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

// rawMonitorStream opens a stream and returns the response, left open. The
// caller closes the returned cancel to end it.
func rawMonitorStream(t *testing.T, baseURL, query string) (*http.Response, context.CancelFunc) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+monitorPath("stream")+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer "+testKey)
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		t.Fatalf("GET stream %s: %v", query, err)
	}
	t.Cleanup(func() { cancel(); _ = resp.Body.Close() })
	return resp, cancel
}

// TestMonitorStreamRefusesABadTopicParam: the parameter admits a topic's
// characters and length only, before any viewer is attached.
func TestMonitorStreamRefusesABadTopicParam(t *testing.T) {
	t.Parallel()
	mon, _ := newTestMonitor(t)
	_, hs := monitorServer(t, mon)

	long := "/topic/" + strings.Repeat("a", 201)
	for name, q := range map[string]string{
		"missing":   "",
		"empty":     "?param=",
		"space":     "?param=" + url.QueryEscape("/topic/a b"),
		"query":     "?param=" + url.QueryEscape("/topic/a?b"),
		"backslash": "?param=" + url.QueryEscape("/topic/a\\b"),
		"too long":  "?param=" + url.QueryEscape(long),
	} {
		resp, _ := rawMonitorStream(t, hs.URL, q)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
	resp, _ := rawMonitorStream(t, hs.URL, "?param="+url.QueryEscape("/topic/"+strings.Repeat("a", 200)))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("200-character topic: status %d, want 200 (the length bound is 207 with the prefix)", resp.StatusCode)
	}
	if got := len(mon.Topics()); got != 1 {
		t.Errorf("topics watched = %d, want only the valid one", got)
	}
}

// TestValidMonitorTopic pins the parameter bound itself: the monitor would
// refuse most of these later, but a bad name must not reach it.
func TestValidMonitorTopic(t *testing.T) {
	t.Parallel()

	for topic, want := range map[string]bool{
		"":                                   false,
		"/topic/a":                           true,
		"/topic/goss.gridappsd.*.>":          true,
		"/topic/" + strings.Repeat("a", 200): true,
		"/topic/" + strings.Repeat("a", 201): false,
		"/topic/a b":                         false,
		"/topic/a?b":                         false,
		"/topic/a\\b":                        false,
		"/topic/a\nb":                        false,
		"/topic/\u00e9":                      false,
	} {
		if got := validMonitorTopic(topic); got != want {
			t.Errorf("validMonitorTopic(%q) = %t, want %t", topic, got, want)
		}
	}
}

// TestMonitorStreamHoldsAtMostEightStreams: a ninth open stream is refused
// with 503 and a retry hint, and a closed stream frees its slot.
func TestMonitorStreamHoldsAtMostEightStreams(t *testing.T) {
	t.Parallel()
	mon, _ := newTestMonitor(t)
	_, hs := monitorServer(t, mon)

	var cancels []context.CancelFunc
	for i := range monitorMaxStreams {
		resp, cancel := rawMonitorStream(t, hs.URL, "?param=/topic/test.slot"+strconv.Itoa(i))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d: status %d, want 200", i, resp.StatusCode)
		}
		cancels = append(cancels, cancel)
	}
	resp, _ := rawMonitorStream(t, hs.URL, "?param=/topic/test.slot1")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("ninth stream: status %d, Retry-After %q, want 503 with a hint", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	cancels[0]()
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, _ := rawMonitorStream(t, hs.URL, "?param=/topic/test.slot1")
		if resp.StatusCode == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a closed stream never freed its slot: last status %d", resp.StatusCode)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestMonitorStreamResumesAfterTheLastEventID: events at or below the
// browser's Last-Event-ID are not replayed.
func TestMonitorStreamResumesAfterTheLastEventID(t *testing.T) {
	t.Parallel()
	mon, b := newTestMonitor(t)
	_, hs := monitorServer(t, mon)
	const topic = "/topic/test.after"

	first, err := mon.Watch(topic)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	sub := b.next(t, topic)
	sub.in <- cimstomp.Message{Destination: topic, Body: []byte("one")}
	sub.in <- cimstomp.Message{Destination: topic, Body: []byte("two")}
	for n := 0; n < 3; n++ {
		<-first.Events()
	}

	all := openMonitorStream(t, hs.URL, topic, "")
	m1, m2 := all.next(), all.next()
	ev := openMonitorStream(t, hs.URL, topic, strconv.FormatUint(m1.id, 10)).next()
	if ev.Kind != "message" || ev.Text != m2.Text || ev.id != m2.id {
		t.Errorf("first event after id %d = %+v, want the second message %+v", m1.id, ev, m2)
	}
	// A header that is not a number replays everything.
	if ev := openMonitorStream(t, hs.URL, topic, "not-a-number").next(); ev.id != m1.id {
		t.Errorf("first event after a bad id = %+v, want the first message %+v", ev, m1)
	}
}

// TestMonitorFeedClosesAViewerThatCannotKeepUp: a send that stalls while the
// topic floods past the viewer's buffer ends the view with the monitor's own
// reason, and the viewer is closed.
func TestMonitorFeedClosesAViewerThatCannotKeepUp(t *testing.T) {
	t.Parallel()
	mon, b := newTestMonitor(t)
	s, _ := monitorServer(t, mon)
	const topic = "/topic/test.slow"

	v, err := mon.Watch(topic)
	if err != nil {
		t.Fatal(err)
	}
	sub := b.next(t, topic)

	release := make(chan struct{})
	var mu sync.Mutex
	var got []monitorEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		first := true
		s.monitor.feed(context.Background(), v, 0, func(ev monitorEvent) bool {
			if first {
				first = false
				<-release
			}
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
			return true
		})
	}()
	for i := 0; i < 400; i++ {
		sub.in <- cimstomp.Message{Destination: topic, Body: []byte("x")}
	}
	deadline := time.Now().Add(3 * time.Second)
	for !errors.Is(v.Err(), busmonitor.ErrSlowViewer) {
		if time.Now().After(deadline) {
			t.Fatalf("viewer error = %v, want ErrSlowViewer after 400 messages with no reader", v.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("feed did not return after the view ended")
	}
	mu.Lock()
	defer mu.Unlock()
	last := got[len(got)-1]
	if !last.Final || last.Kind != "status" || !strings.HasPrefix(last.Text, "monitor stopped this view: viewer closed because it did not keep up") {
		t.Errorf("last event = %+v, want a final status naming the slow viewer", last)
	}
	if len(got) > 300 {
		t.Errorf("delivered %d events, want the buffer's worth and not all 400", len(got))
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
