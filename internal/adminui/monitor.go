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

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2admin"

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

// monitorParam admits a topic name's characters only; busmonitor.Watch
// still decides whether the name is one that may be watched.
var monitorParam = sep2admin.StreamParam{
	MaxLen:  len("/topic/") + 200,
	Charset: "/ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-.*>",
}

// monitorPanel serves the bus monitor: the View describes how to use it
// and lists the watched topics, and the Stream carries one topic's feed.
type monitorPanel struct {
	mon MonitorSource
	// idBase offsets every event ID by the panel's start time in
	// microseconds. A browser resuming with an ID from a previous bridge
	// process would otherwise be ahead of the new process's sequence, and
	// the plane would drop every event at or below it.
	idBase uint64
}

func newMonitorPanel(mon MonitorSource, start time.Time) *monitorPanel {
	return &monitorPanel{mon: mon, idBase: uint64(start.UnixMicro())}
}

func (p *monitorPanel) panel(slot int) sep2admin.Panel {
	return sep2admin.Panel{
		ID:                panelBusMonitor,
		Label:             "Bus monitor",
		Placement:         sep2admin.ExtensionSlot(slot),
		DescriptorVersion: sep2admin.CurrentDescriptorVersion,
		View:              p.view,
		Stream:            &sep2admin.Stream{Param: monitorParam, Open: p.open},
	}
}

func (p *monitorPanel) view(context.Context) (sep2admin.Descriptor, error) {
	topics := p.mon.Topics()
	rows := make([]sep2admin.Row, 0, len(topics))
	for _, t := range topics {
		rows = append(rows, sep2admin.Row{text(t.Name), text(string(t.State)), number(t.Viewers), number(t.Buffered)})
	}
	return tables(table("Watched topics", "No topic is being watched.",
		[]string{
			fmt.Sprintf("Enter a topic such as /topic/goss.gridappsd.platform.log and press Start. A segment may be * and the name may end in .> to watch many topics. Queues, the bus health probe, the token topics and the broker advisories are refused. At most %d topics are watched at once, each on its own broker connection, closed %s after its last viewer leaves.",
				busmonitor.MaxTopics, busmonitor.IdleClose),
			fmt.Sprintf("A viewer first gets the newest %d messages the topic has buffered, then the live feed. Each line shows the topic, the size and up to %d bytes of the body; a body that is not UTF-8 text shows as its size and a hex prefix.",
				busmonitor.RingSize, monitorBodyShown),
		},
		[]string{"Topic", "State", "Viewers", "Buffered"}, rows)), nil
}

// open attaches a viewer and feeds it from its own goroutine until the
// stream ends. A topic the monitor refuses fails the open, the one way a
// source can refuse without holding one of the plane's stream slots; the
// plane logs the reason, and the browser only learns the stream was refused.
func (p *monitorPanel) open(ctx context.Context, req sep2admin.StreamRequest, send sep2admin.StreamSendFunc) error {
	v, err := p.mon.Watch(req.Param)
	if err != nil {
		return fmt.Errorf("topic %q not watched: %w", req.Param, err)
	}
	go p.feed(ctx, v, req.After, send)
	return nil
}

func (p *monitorPanel) feed(ctx context.Context, v *busmonitor.Viewer, after uint64, send sep2admin.StreamSendFunc) {
	defer v.Close()
	for _, m := range v.Backlog() {
		if id := p.idBase + m.Seq; id > after && !send(p.messageEvent(m)) {
			return
		}
	}
	if !send(sep2admin.StreamEvent{ID: p.idBase + v.JoinSeq(), Time: time.Now(), Kind: sep2admin.StreamStatus, Text: statusText(v.Status())}) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-v.Events():
			if !ok {
				send(sep2admin.StreamEvent{ID: p.idBase + v.EndSeq(), Time: time.Now(), Kind: sep2admin.StreamStatus,
					Text: "monitor stopped this view: " + viewerEndReason(v.Err()) + "; press Start to watch again"})
				return
			}
			if !send(p.event(ev)) {
				return
			}
		}
	}
}

func (p *monitorPanel) event(ev busmonitor.Event) sep2admin.StreamEvent {
	if ev.Kind == busmonitor.EventMessage {
		return p.messageEvent(ev.Message)
	}
	return sep2admin.StreamEvent{ID: p.idBase + ev.Seq, Time: ev.Time, Kind: sep2admin.StreamStatus, Text: statusText(ev.Status)}
}

func (p *monitorPanel) messageEvent(m busmonitor.Message) sep2admin.StreamEvent {
	return sep2admin.StreamEvent{ID: p.idBase + m.Seq, Time: m.Received, Kind: sep2admin.StreamMessage, Text: messageText(m)}
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
