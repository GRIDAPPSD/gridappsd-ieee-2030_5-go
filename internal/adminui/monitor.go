package adminui

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/busmonitor"
)

const panelBusMonitor = "gridappsd-busmonitor"

// MonitorSource is what the bus monitor panel needs from
// *busmonitor.Monitor.
type MonitorSource interface {
	Watch(name string) (*busmonitor.Viewer, error)
	Topics() []busmonitor.TopicInfo
}

// Display bounds for one monitored message.
const (
	monitorBodyShown = 2 << 10
	monitorHexPrefix = 32
)

// Stream bounds. At most monitorMaxStreams streams are open at once, and a
// browser that takes longer than monitorWriteTimeout to accept one event is
// cut, so a stalled connection cannot hold a viewer.
const (
	monitorMaxStreams   = 8
	monitorWriteTimeout = 10 * time.Second
)

// The topic name a stream request may carry: its length and characters
// only; busmonitor.Watch still decides whether the name may be watched.
const (
	monitorTopicMaxLen  = len("/topic/") + 200
	monitorTopicCharset = "/ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-.*>"
)

// Kinds of a monitor event on the wire.
const (
	monitorKindMessage = "message"
	monitorKindStatus  = "status"
)

// monitorEvent is one event of a topic stream. ID is zero for an event that
// is not resumable, which the wire then leaves without an id line.
type monitorEvent struct {
	ID    uint64
	Time  time.Time
	Kind  string
	Text  string
	Final bool
}

// monitorPanel serves the bus monitor page: the topics route lists the
// watched topics and the stream route carries one topic's feed.
type monitorPanel struct {
	mon MonitorSource
	// idBase offsets every event ID by the panel's start time in
	// microseconds. A browser resuming with an ID from a previous bridge
	// process would otherwise be ahead of the new process's sequence, and
	// the stream would drop every event at or below it.
	idBase uint64
	// slots holds one entry for each open stream.
	slots chan struct{}
}

func newMonitorPanel(mon MonitorSource, start time.Time) *monitorPanel {
	return &monitorPanel{mon: mon, idBase: uint64(start.UnixMicro()), slots: make(chan struct{}, monitorMaxStreams)}
}

// feed sends the topic's backlog, its state, then each live event until the
// view ends, send reports a failure, or ctx is done. It closes the viewer.
func (p *monitorPanel) feed(ctx context.Context, v *busmonitor.Viewer, after uint64, send func(monitorEvent) bool) {
	defer v.Close()
	for _, m := range v.Backlog() {
		if id := p.idBase + m.Seq; id > after && !send(p.messageEvent(m)) {
			return
		}
	}
	if !send(monitorEvent{ID: p.idBase + v.JoinSeq(), Time: time.Now(), Kind: monitorKindStatus, Text: statusText(v.Status())}) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			send(monitorEvent{Time: time.Now(), Kind: monitorKindStatus, Text: "stream closed: server shutting down", Final: true})
			return
		case ev, ok := <-v.Events():
			if !ok {
				send(monitorEvent{ID: p.idBase + v.EndSeq(), Time: time.Now(), Kind: monitorKindStatus, Final: true,
					Text: "monitor stopped this view: " + viewerEndReason(v.Err()) + "; press Start to watch again"})
				return
			}
			if !send(p.event(ev)) {
				return
			}
		}
	}
}

func (p *monitorPanel) event(ev busmonitor.Event) monitorEvent {
	if ev.Kind == busmonitor.EventMessage {
		return p.messageEvent(ev.Message)
	}
	return monitorEvent{ID: p.idBase + ev.Seq, Time: ev.Time, Kind: monitorKindStatus, Text: statusText(ev.Status)}
}

func (p *monitorPanel) messageEvent(m busmonitor.Message) monitorEvent {
	return monitorEvent{ID: p.idBase + m.Seq, Time: m.Received, Kind: monitorKindMessage, Text: messageText(m)}
}

func viewerEndReason(err error) string {
	if err == nil {
		return "closed"
	}
	return strings.TrimPrefix(err.Error(), "busmonitor: ")
}

func statusText(st busmonitor.Status) string {
	switch st.State {
	case busmonitor.StateReconnecting:
		return fmt.Sprintf("reconnecting (try %d, in %s): %s", st.Attempt, st.Retry, st.Reason)
	case busmonitor.StateRefused:
		return "refused by the broker: " + st.Reason + "; press Start to try again"
	case busmonitor.StateFailed:
		return "stopped: " + st.Reason + "; press Start to try again"
	}
	if st.Reason != "" {
		return string(st.State) + ": " + st.Reason
	}
	return string(st.State)
}

// messageText is one line: the topic the frame arrived on, its true size,
// and its body compacted for display.
func messageText(m busmonitor.Message) string {
	return fmt.Sprintf("%s %d bytes: %s", m.Destination, m.Size, displayBody(m))
}

func displayBody(m busmonitor.Message) string {
	body := m.Body
	if m.Truncated {
		// The ring cut the body at a byte count, possibly inside a rune.
		body = trimPartialRune(body)
	}
	if !utf8.Valid(body) {
		n := min(len(m.Body), monitorHexPrefix)
		return fmt.Sprintf("binary, %d bytes, hex %s...", m.Size, hex.EncodeToString(m.Body[:n]))
	}
	var compact bytes.Buffer
	if !m.Truncated && json.Compact(&compact, body) == nil {
		body = compact.Bytes()
	}
	shown := oneLine(string(body))
	if len(shown) <= monitorBodyShown && !m.Truncated {
		return shown
	}
	shown = cutAtRune(shown, monitorBodyShown)
	return fmt.Sprintf("%s ... (%d of %d bytes shown)", shown, len(shown), m.Size)
}

// oneLine replaces control characters, and the bidirectional controls that
// reorder text on screen, with spaces, so a body cannot forge a second line
// or disguise itself.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r >= 0x7F && r <= 0x9F, r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
			return ' '
		}
		return r
	}, s)
}

func trimPartialRune(b []byte) []byte {
	for i := 0; i < utf8.UTFMax && len(b) > 0; i++ {
		if r, size := utf8.DecodeLastRune(b); r != utf8.RuneError || size != 1 {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
}

func cutAtRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
