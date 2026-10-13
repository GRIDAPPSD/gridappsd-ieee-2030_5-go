package adminui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/monitorpage"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp"
)

type monitorScenario struct {
	Body      json.RawMessage `json:"body,omitempty"`
	Status    int             `json:"status,omitempty"`
	Hidden    bool            `json:"hidden,omitempty"`
	NoTimeout bool            `json:"noTimeout,omitempty"`
	Topic     *string         `json:"topic,omitempty"`
	Steps     []string        `json:"steps,omitempty"`
}

type monitorRun struct {
	URLs    []string `json:"urls"`
	Sources []struct {
		URL    string `json:"url"`
		Closed bool   `json:"closed"`
	} `json:"sources"`
	Log []struct {
		Text  string `json:"text"`
		Class string `json:"cls"`
	} `json:"log"`
	Status            string `json:"status"`
	StatusClass       string `json:"statusClass"`
	StopDisabled      bool   `json:"stopDisabled"`
	TopicsStatus      string `json:"topicsStatus"`
	TopicsStatusClass string `json:"topicsStatusClass"`
	Topics            *struct {
		Columns []string   `json:"columns"`
		Rows    [][]string `json:"rows"`
	} `json:"topics"`
	TopicsEmpty []string `json:"topicsEmpty"`
	Prose       []string `json:"prose"`
	Steps       []struct {
		Step    string `json:"step"`
		Fetches int    `json:"fetches"`
		Timers  int    `json:"timers"`
		Sources int    `json:"sources"`
	} `json:"steps"`
	Timers       int `json:"timers"`
	Prevented    int `json:"prevented"`
	Thrown       int `json:"thrown"`
	Removed      int `json:"removed"`
	InnerHTML    int `json:"innerHTML"`
	Style        int `json:"style"`
	TimeoutCalls int `json:"timeoutCalls"`
	Controllers  int `json:"controllers"`
}

func monitorPageSource(t *testing.T) string {
	t.Helper()
	b, err := monitorpage.Files.ReadFile("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	return string(b)
}

// runMonitorPage runs source against the scenarios on a fake DOM with a fake
// EventSource (testdata/monitor_page.js). A missing node skips the test
// locally and fails it under CI, as the other page tests do.
func runMonitorPage(t *testing.T, source string, scenarios map[string]monitorScenario) map[string]monitorRun {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("node not installed, and CI must run the page test: %v", err)
		}
		t.Skip("node not installed")
	}
	in, err := json.Marshal(map[string]any{"source": source, "scenarios": scenarios})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "testdata/monitor_page.js")
	cmd.Stdin = bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, stderr.String())
	}
	var runs map[string]monitorRun
	if err := json.Unmarshal(out, &runs); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return runs
}

func rawTopics(t *testing.T, s *Server) json.RawMessage {
	t.Helper()
	rec := doRequest(t, s.Handler(), http.MethodGet, monitorPath("topics"), "Bearer "+testKey, "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET topics: %d %s", rec.Code, rec.Body.String())
	}
	return slices.Clone(rec.Body.Bytes())
}

func emptyTopics(t *testing.T) json.RawMessage {
	t.Helper()
	mon, _ := newTestMonitor(t)
	s, _ := monitorServer(t, mon)
	return rawTopics(t, s)
}

func strp(s string) *string { return &s }

func event(kind, text string, final bool) string {
	b, _ := json.Marshal(monitorStreamEvent{Time: "2026-10-12T10:00:00.5Z", Kind: kind, Text: text, Final: final})
	return "emit:" + string(b)
}

func TestMonitorPageListsTheWatchedTopics(t *testing.T) {
	t.Parallel()

	mon, b := newTestMonitor(t)
	s, _ := monitorServer(t, mon)
	const topic = "/topic/test.listed"
	v, err := mon.Watch(topic)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	sub := b.next(t, topic)
	sub.in <- cimstomp.Message{Destination: topic, Body: []byte("one")}
	for range 2 {
		<-v.Events()
	}
	runs := runMonitorPage(t, monitorPageSource(t), map[string]monitorScenario{
		"listed": {Body: rawTopics(t, s)},
		"none":   {Body: emptyTopics(t)},
		"failed": {Status: 500, Body: json.RawMessage(`{"error":"boom"}`)},
	})

	l := runs["listed"]
	if l.Topics == nil || !slices.Equal(l.Topics.Columns, []string{"Topic", "State", "Viewers", "Buffered"}) ||
		!slices.EqualFunc(l.Topics.Rows, [][]string{{topic, "live", "1", "1"}}, slices.Equal) {
		t.Errorf("topics table = %+v, want the one live topic", l.Topics)
	}
	if len(l.Prose) != 2 || !strings.Contains(l.Prose[0], "At most 8 topics") || !strings.HasPrefix(l.TopicsStatus, "updated ") {
		t.Errorf("prose %q, status %q", l.Prose, l.TopicsStatus)
	}
	if n := runs["none"]; n.Topics != nil || !slices.Equal(n.TopicsEmpty, []string{"No topic is being watched."}) {
		t.Errorf("no topic: table %+v, text %q; want only the empty text", n.Topics, n.TopicsEmpty)
	}
	if f := runs["failed"]; !strings.Contains(f.TopicsStatus, "unavailable") || f.TopicsStatusClass != "error" || f.Topics != nil || len(f.TopicsEmpty) != 0 {
		t.Errorf("failed poll shows %q (%q) and table %+v, want an unavailable notice and no stale rows", f.TopicsStatus, f.TopicsStatusClass, f.Topics)
	}
}

func TestMonitorPageStreamsMessagesAndStatus(t *testing.T) {
	t.Parallel()

	run := runMonitorPage(t, monitorPageSource(t), map[string]monitorScenario{"feed": {
		Topic: strp("/topic/goss.gridappsd.*.>"),
		Steps: []string{
			"start",
			event("status", "live", false),
			event("message", `/topic/goss.gridappsd.x 11 bytes: {"a":1}`, false),
			event("status", "monitor stopped this view: monitor closed; press Start to watch again", true),
		},
	}})["feed"]

	if len(run.Sources) != 1 || run.Sources[0].URL != "stream?param=%2Ftopic%2Fgoss.gridappsd.*.%3E" || !run.Sources[0].Closed {
		t.Fatalf("sources = %+v, want one stream with the topic encoded, closed by the final event", run.Sources)
	}
	if run.Prevented != 1 {
		t.Errorf("submit default prevented %d times, want 1", run.Prevented)
	}
	if len(run.Log) != 3 {
		t.Fatalf("log = %+v, want 3 lines", run.Log)
	}
	if got := run.Log[0]; got.Text != "2026-10-12T10:00:00.5Z [status] live" || got.Class != "status" {
		t.Errorf("line 0 = %+v", got)
	}
	if got := run.Log[1]; got.Text != `2026-10-12T10:00:00.5Z /topic/goss.gridappsd.x 11 bytes: {"a":1}` || got.Class != "" {
		t.Errorf("line 1 = %+v, want the message with no status class", got)
	}
	if run.Status != "monitor stopped this view: monitor closed; press Start to watch again" || !run.StopDisabled {
		t.Errorf("status %q, stop disabled %t; want the final status and Stop disabled", run.Status, run.StopDisabled)
	}
}

func TestMonitorPageStartStopAndRestart(t *testing.T) {
	t.Parallel()

	runs := runMonitorPage(t, monitorPageSource(t), map[string]monitorScenario{
		"blank":   {Topic: strp("  "), Steps: []string{"start"}},
		"restart": {Steps: []string{"start:/topic/a", event("message", "m", false), "start:/topic/b"}},
		"stop":    {Steps: []string{"start:/topic/a", "stop"}},
	})
	b := runs["blank"]
	if len(b.Sources) != 0 || b.Status != "Enter a topic first." || b.StatusClass != "error" {
		t.Errorf("blank topic: %d sources, status %q (%q); want none and a prompt", len(b.Sources), b.Status, b.StatusClass)
	}
	r := runs["restart"]
	if len(r.Sources) != 2 || !r.Sources[0].Closed || r.Sources[1].Closed || len(r.Log) != 0 {
		t.Errorf("restart: sources %+v, log %+v; want the first closed, the second open and a cleared log", r.Sources, r.Log)
	}
	s := runs["stop"]
	if !s.Sources[0].Closed || !s.StopDisabled || s.Status != "stopped" {
		t.Errorf("stop: source %+v, stop disabled %t, status %q", s.Sources[0], s.StopDisabled, s.Status)
	}
}

func TestMonitorPageTellsARefusalFromADrop(t *testing.T) {
	t.Parallel()

	runs := runMonitorPage(t, monitorPageSource(t), map[string]monitorScenario{
		"drop":    {Steps: []string{"start:/topic/a", "error:0"}},
		"refused": {Steps: []string{"start:/topic/a", "error:2"}},
	})
	d := runs["drop"]
	if d.Status != "connection lost; retrying" || d.Sources[0].Closed || d.StopDisabled {
		t.Errorf("drop: status %q, closed %t, stop disabled %t; want the browser left to retry", d.Status, d.Sources[0].Closed, d.StopDisabled)
	}
	r := runs["refused"]
	if !strings.HasPrefix(r.Status, "the monitor refused or ended this stream") || r.StatusClass != "error" || !r.StopDisabled {
		t.Errorf("refused: status %q (%q), stop disabled %t", r.Status, r.StatusClass, r.StopDisabled)
	}
}

// hostileEvent is a message whose text is markup.
func hostileEvent() string {
	return event("message", `<img src=x onerror=alert(1)> <script>alert(1)</script>`, false)
}

func TestMonitorPageWritesServerStringsAsText(t *testing.T) {
	t.Parallel()

	run := runMonitorPage(t, monitorPageSource(t), map[string]monitorScenario{"hostile": {
		Body:  json.RawMessage(`{"prose":["<b>p</b>"],"topics":[{"name":"<i>n</i>","state":"<u>s</u>","viewers":1,"buffered":2}]}`),
		Steps: []string{"start:/topic/a", hostileEvent(), "emit:{not json"},
	}})["hostile"]
	if run.InnerHTML != 0 || run.Style != 0 {
		t.Errorf("innerHTML writes = %d, style writes = %d, want none", run.InnerHTML, run.Style)
	}
	if len(run.Log) != 2 || !strings.HasSuffix(run.Log[0].Text, "<img src=x onerror=alert(1)> <script>alert(1)</script>") || run.Log[1].Text != "unreadable event" {
		t.Errorf("log = %+v, want the raw text and a note for the frame that is not JSON", run.Log)
	}
	if run.Prose[0] != "<b>p</b>" || run.Topics.Rows[0][0] != "<i>n</i>" || run.Topics.Rows[0][1] != "<u>s</u>" {
		t.Errorf("prose %q, topics %+v", run.Prose, run.Topics)
	}
}

// TestMonitorPageSurvivesAFrameThatIsNotAnEventObject: JSON that parses to
// null, a number or a string is not an event, and must be noted like text that
// is not JSON, not thrown from the message handler or drawn as an event with
// undefined fields.
func TestMonitorPageSurvivesAFrameThatIsNotAnEventObject(t *testing.T) {
	t.Parallel()

	run := runMonitorPage(t, monitorPageSource(t), map[string]monitorScenario{"odd": {
		Steps: []string{"start:/topic/a", "emit:null", "emit:5", `emit:"text"`, "emit:[1]", event("message", "after", false)},
	}})["odd"]
	if run.Thrown != 0 || len(run.Log) != 5 {
		t.Fatalf("handler threw %d times, log = %+v, want no throw, 4 notes and the one real message", run.Thrown, run.Log)
	}
	for i := range 4 {
		if run.Log[i].Text != "unreadable event" || run.Log[i].Class != "status" {
			t.Errorf("line %d = %+v, want the unreadable-event note", i, run.Log[i])
		}
	}
	if !strings.HasSuffix(run.Log[4].Text, " after") {
		t.Errorf("last line = %+v, want the real message after the odd frames", run.Log[4])
	}
}

func TestMonitorPageKeepsTheNewest500Lines(t *testing.T) {
	t.Parallel()

	steps := []string{"start:/topic/a"}
	for i := range 505 {
		steps = append(steps, event("message", fmt.Sprintf("line %03d", i), false))
	}
	run := runMonitorPage(t, monitorPageSource(t), map[string]monitorScenario{"flood": {Steps: steps}})["flood"]
	if len(run.Log) != 500 || !strings.HasSuffix(run.Log[0].Text, "line 005") || !strings.HasSuffix(run.Log[499].Text, "line 504") || run.Removed != 5 {
		t.Errorf("log holds %d lines from %q to %q, removed %d; want the newest 500 (005 to 504) and 5 removed", len(run.Log), run.Log[0].Text, run.Log[len(run.Log)-1].Text, run.Removed)
	}
}

func monitorPollScenarios(body json.RawMessage) map[string]monitorScenario {
	return map[string]monitorScenario{
		"visible":     {Body: body},
		"hideLater":   {Body: body, Steps: []string{"hide", "tick", "show", "tick"}},
		"hiddenStart": {Body: body, Hidden: true, Steps: []string{"tick", "show"}},
	}
}

func checkMonitorPolling(t *testing.T, runs map[string]monitorRun) {
	t.Helper()
	if v := runs["visible"]; len(v.URLs) != 1 || v.Timers != 1 {
		t.Errorf("visible start: %d fetches, %d timers, want 1 and 1", len(v.URLs), v.Timers)
	}
	h := runs["hideLater"]
	if len(h.Steps) != 4 {
		t.Fatalf("hideLater steps = %+v", h.Steps)
	}
	if s := h.Steps[1]; s.Fetches != 1 || s.Timers != 0 {
		t.Errorf("tick while hidden: %d fetches, %d timers, want no fetch and no new timer", s.Fetches, s.Timers)
	}
	if s := h.Steps[2]; s.Fetches != 2 || s.Timers != 1 {
		t.Errorf("after show: %d fetches, %d timers, want polling resumed", s.Fetches, s.Timers)
	}
	hs := runs["hiddenStart"]
	if hs.Steps[0].Fetches != 0 || hs.Steps[1].Fetches != 1 {
		t.Errorf("hidden start: %d fetches while hidden and %d after show, want 0 and 1", hs.Steps[0].Fetches, hs.Steps[1].Fetches)
	}
}

func TestMonitorPagePollsTopicsOnlyWhileVisibleButKeepsTheFeed(t *testing.T) {
	t.Parallel()

	body := emptyTopics(t)
	checkMonitorPolling(t, runMonitorPage(t, monitorPageSource(t), monitorPollScenarios(body)))

	run := runMonitorPage(t, monitorPageSource(t), map[string]monitorScenario{"hiddenFeed": {
		Body: body, Steps: []string{"start:/topic/a", "hide", event("message", "while hidden", false)},
	}})["hiddenFeed"]
	if len(run.Log) != 1 || len(run.Sources) != 1 || run.Sources[0].Closed {
		t.Errorf("hidden tab: log %+v, sources %+v; want the live feed kept", run.Log, run.Sources)
	}
}

func TestMonitorPageFallsBackWhereAbortSignalHasNoTimeout(t *testing.T) {
	t.Parallel()

	body := emptyTopics(t)
	runs := runMonitorPage(t, monitorPageSource(t), map[string]monitorScenario{"old": {Body: body, NoTimeout: true}, "new": {Body: body}})
	if old := runs["old"]; !strings.HasPrefix(old.TopicsStatus, "updated ") || old.TimeoutCalls != 0 || old.Controllers != 1 {
		t.Errorf("without AbortSignal.timeout: status %q, %d timeout calls, %d controllers; want the list loaded through one controller", old.TopicsStatus, old.TimeoutCalls, old.Controllers)
	}
	if cur := runs["new"]; cur.TimeoutCalls != 1 || cur.Controllers != 0 {
		t.Errorf("with AbortSignal.timeout: %d calls, %d controllers; want 1 and 0", cur.TimeoutCalls, cur.Controllers)
	}
}

// orderWriter is a response writer that supports deadlines and records the
// order of the calls that matter to a stream: a deadline set and a flush.
type orderWriter struct {
	mu     sync.Mutex
	header http.Header
	calls  []string
	first  chan struct{}
	once   sync.Once
}

func newOrderWriter() *orderWriter {
	return &orderWriter{header: http.Header{}, first: make(chan struct{})}
}

func (w *orderWriter) Header() http.Header         { return w.header }
func (w *orderWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *orderWriter) WriteHeader(int)             {}
func (w *orderWriter) Flush() {
	w.mu.Lock()
	w.calls = append(w.calls, "flush")
	w.mu.Unlock()
	w.once.Do(func() { close(w.first) })
}
func (w *orderWriter) SetWriteDeadline(time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, "deadline")
	return nil
}

// TestMonitorStreamSetsTheWriteDeadlineBeforeTheFirstFlush: the write bound
// that cuts a stalled browser is in place before the stream's first bytes
// are flushed, not only once the first event is written.
func TestMonitorStreamSetsTheWriteDeadlineBeforeTheFirstFlush(t *testing.T) {
	t.Parallel()
	mon, _ := newTestMonitor(t)
	s, _ := monitorServer(t, mon)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/stream?param=/topic/test.deadline", nil).WithContext(ctx)
	w := newOrderWriter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.monitorPage().ServeHTTP(w, req)
	}()
	select {
	case <-w.first:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream never flushed")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream did not end when its request was canceled")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.calls) == 0 || w.calls[0] != "deadline" || !slices.Contains(w.calls, "flush") {
		t.Errorf("calls = %q, want a deadline before the first flush", w.calls)
	}
}

// TestMonitorPageMutantsAreCaught proves the page tests can fail: each
// mutant breaks one property of app.js, and the test that guards it must
// see it.
func TestMonitorPageMutantsAreCaught(t *testing.T) {
	t.Parallel()

	src := monitorPageSource(t)
	mutate := func(old, repl string) string {
		if strings.Count(src, old) != 1 {
			t.Fatalf("mutant target %q occurs %d times in app.js, want 1", old, strings.Count(src, old))
		}
		return strings.Replace(src, old, repl, 1)
	}
	body := emptyTopics(t)
	feed := map[string]monitorScenario{"feed": {Topic: strp("/topic/goss.gridappsd.*.>"), Steps: []string{"start", event("status", "stopped", true)}}}

	t.Run("polls topics while hidden", func(t *testing.T) {
		t.Parallel()
		runs := runMonitorPage(t, mutate("if (document.hidden) return;", ""), monitorPollScenarios(body))
		if runs["hideLater"].Steps[1].Fetches == 1 && runs["hiddenStart"].Steps[0].Fetches == 0 {
			t.Error("the polling checks passed against a page that ignores document.hidden")
		}
	})
	t.Run("writes markup with innerHTML", func(t *testing.T) {
		t.Parallel()
		runs := runMonitorPage(t, mutate("e.textContent = text;", "e.innerHTML = text;"), map[string]monitorScenario{"h": {Steps: []string{"start:/topic/a", hostileEvent()}}})
		if runs["h"].InnerHTML == 0 {
			t.Error("the text-only check passed against a page that writes innerHTML")
		}
	})
	t.Run("reads a null frame as an event", func(t *testing.T) {
		t.Parallel()
		mutant := mutate(`if (ev === null || typeof ev !== "object" || Array.isArray(ev)) {`, `if (false) {`)
		run := runMonitorPage(t, mutant, map[string]monitorScenario{"n": {Steps: []string{"start:/topic/a", "emit:null", event("message", "after", false)}}})["n"]
		if run.Thrown == 0 && len(run.Log) == 2 && run.Log[0].Text == "unreadable event" {
			t.Error("the null-frame check passed against a page that reads null as an event")
		}
	})
	t.Run("keeps the stream after a final event", func(t *testing.T) {
		t.Parallel()
		run := runMonitorPage(t, mutate("if (ev.final) endFeed();", ""), feed)["feed"]
		if run.Sources[0].Closed {
			t.Error("the final-event check passed against a page that never ends the stream")
		}
	})
	t.Run("sends the topic unencoded", func(t *testing.T) {
		t.Parallel()
		run := runMonitorPage(t, mutate("encodeURIComponent(topic)", "topic"), feed)["feed"]
		if run.Sources[0].URL == "stream?param=%2Ftopic%2Fgoss.gridappsd.*.%3E" {
			t.Error("the encoding check passed against a page that sends the raw topic")
		}
	})
	t.Run("keeps every line", func(t *testing.T) {
		t.Parallel()
		steps := []string{"start:/topic/a"}
		for i := range 505 {
			steps = append(steps, event("message", fmt.Sprintf("line %03d", i), false))
		}
		run := runMonitorPage(t, mutate("while (log.children.length > MAX_LINES)", "while (false)"), map[string]monitorScenario{"f": {Steps: steps}})["f"]
		if len(run.Log) == 500 {
			t.Error("the bound check passed against a page that never drops a line")
		}
	})
	t.Run("leaves the old stream open on restart", func(t *testing.T) {
		t.Parallel()
		mutant := mutate("    endFeed();\n    $(\"log\").replaceChildren();", "    $(\"log\").replaceChildren();")
		run := runMonitorPage(t, mutant, map[string]monitorScenario{"r": {Steps: []string{"start:/topic/a", "start:/topic/b"}}})["r"]
		if run.Sources[0].Closed {
			t.Error("the restart check passed against a page that leaves the first stream open")
		}
	})
	t.Run("retries a refused stream", func(t *testing.T) {
		t.Parallel()
		run := runMonitorPage(t, mutate("if (es.readyState === CLOSED) {", "if (false) {"), map[string]monitorScenario{"r": {Steps: []string{"start:/topic/a", "error:2"}}})["r"]
		if run.StopDisabled {
			t.Error("the refusal check passed against a page that treats a refusal as a drop")
		}
	})
	t.Run("calls AbortSignal.timeout without a fallback", func(t *testing.T) {
		t.Parallel()
		mutant := mutate(`if (typeof AbortSignal !== "undefined" && typeof AbortSignal.timeout === "function") {`, `if (true) {`)
		if old := runMonitorPage(t, mutant, map[string]monitorScenario{"old": {Body: body, NoTimeout: true}})["old"]; strings.HasPrefix(old.TopicsStatus, "updated ") {
			t.Error("the fallback check passed against a page that always calls AbortSignal.timeout")
		}
	})
}
