package adminui

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/monitorpage"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/busmonitor"
)

type monitorTopicRow struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Viewers  int    `json:"viewers"`
	Buffered int    `json:"buffered"`
}

// monitorTopics is the data of the topics route. Topics is an array, never
// null.
type monitorTopics struct {
	Prose  []string          `json:"prose"`
	Topics []monitorTopicRow `json:"topics"`
}

// monitorStreamEvent is the data line of one stream event.
type monitorStreamEvent struct {
	Time  string `json:"time"`
	Kind  string `json:"kind"`
	Text  string `json:"text"`
	Final bool   `json:"final,omitempty"`
}

func (p *monitorPanel) topics() monitorTopics {
	topics := p.mon.Topics()
	rows := make([]monitorTopicRow, 0, len(topics))
	for _, t := range topics {
		rows = append(rows, monitorTopicRow{Name: t.Name, State: string(t.State), Viewers: t.Viewers, Buffered: t.Buffered})
	}
	return monitorTopics{
		Prose: []string{
			fmt.Sprintf("Enter a topic such as /topic/goss.gridappsd.platform.log and press Start. A segment may be * and the name may end in .> to watch many topics. Queues, the bus health probe, the token topics and the broker advisories are refused. At most %d topics are watched at once, each on its own broker connection, closed %s after its last viewer leaves.",
				busmonitor.MaxTopics, busmonitor.IdleClose),
			fmt.Sprintf("A viewer first gets the newest %d messages the topic has buffered, then the live feed. Each line shows the topic, the size and up to %d bytes of the body; a body that is not UTF-8 text shows as its size and a hex prefix.",
				busmonitor.RingSize, monitorBodyShown),
		},
		Topics: rows,
	}
}

// monitorPage is the handler of the bus monitor page panel. The plane
// mounts it under /apps/gridappsd-busmonitor/ behind its own authentication
// and strips that prefix. It is read only: the mux refuses all but GET.
func (s *Server) monitorPage() http.Handler {
	p := s.monitor
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", pageFile(monitorpage.Files, "index.html", "text/html; charset=utf-8"))
	mux.Handle("GET /app.js", pageFile(monitorpage.Files, "app.js", "text/javascript; charset=utf-8"))
	mux.Handle("GET /style.css", pageFile(monitorpage.Files, "style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /topics", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, p.topics())
	})
	mux.HandleFunc("GET /stream", p.serveStream)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", pageCSP)
		mux.ServeHTTP(w, r)
	})
}

// validMonitorTopic admits a topic name's length and characters only.
func validMonitorTopic(topic string) bool {
	if topic == "" || len(topic) > monitorTopicMaxLen {
		return false
	}
	for _, r := range topic {
		if !strings.ContainsRune(monitorTopicCharset, r) {
			return false
		}
	}
	return true
}

// serveStream answers GET /stream?param=<topic> with a server-sent event
// stream of that topic. A browser resuming sends Last-Event-ID, and events
// at or below it are not replayed. A topic the monitor refuses, or a ninth
// open stream, is refused before any stream starts; the reason is logged
// and the browser only learns the stream was refused.
func (p *monitorPanel) serveStream(w http.ResponseWriter, r *http.Request) {
	topic := r.URL.Query().Get("param")
	if !validMonitorTopic(topic) {
		http.Error(w, "bad topic", http.StatusBadRequest)
		return
	}
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		w.Header().Set("Retry-After", "5")
		http.Error(w, "too many open streams", http.StatusServiceUnavailable)
		return
	}
	v, err := p.mon.Watch(topic)
	if err != nil {
		log.Printf("adminui: monitor topic %q not watched: %v", topic, err)
		http.Error(w, "topic not watched", http.StatusBadRequest)
		return
	}
	// A header that is not a number resumes from the start.
	after, _ := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64)

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	// The bound is in place before the first bytes go out. A recorder
	// supports no deadlines; a real connection that fails to take one is cut
	// at the listener's own limits.
	_ = rc.SetWriteDeadline(time.Now().Add(monitorWriteTimeout))
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		v.Close()
		return
	}
	p.feed(r.Context(), v, after, func(ev monitorEvent) bool {
		return writeMonitorEvent(w, rc, ev)
	})
}

// writeMonitorEvent writes one event and flushes it. A false return means
// the browser is gone or too slow, and the stream ends.
func writeMonitorEvent(w http.ResponseWriter, rc *http.ResponseController, ev monitorEvent) bool {
	data, err := json.Marshal(monitorStreamEvent{Time: ev.Time.UTC().Format(time.RFC3339Nano), Kind: ev.Kind, Text: ev.Text, Final: ev.Final})
	if err != nil {
		log.Printf("adminui: monitor event: %v", err)
		return false
	}
	// Each event gets a fresh bound.
	_ = rc.SetWriteDeadline(time.Now().Add(monitorWriteTimeout))
	var id string
	if ev.ID != 0 {
		id = "id: " + strconv.FormatUint(ev.ID, 10) + "\n"
	}
	if _, err := fmt.Fprintf(w, "%sdata: %s\n\n", id, data); err != nil {
		return false
	}
	return rc.Flush() == nil
}
