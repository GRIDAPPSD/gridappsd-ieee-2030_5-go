package adminui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2admin"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
)

// The bridge's panel IDs. They carry the gridappsd- prefix because a bare
// slug such as "control" is a server tab or route the registry refuses.
const (
	panelHealth      = "gridappsd-health"
	panelConnections = "gridappsd-connections"
	panelDERPrograms = "gridappsd-derprograms"
	panelControlFlow = "gridappsd-controlflow"
)

// panels are the bridge's views as admin plane tabs. The shell lists
// extension panels by (Rank, ID), so Rank fixes the tab order.
func (s *Server) panels() []sep2admin.Panel {
	views := []struct {
		id, label string
		page      http.Handler
	}{
		{panelHealth, "Bridge health", s.healthPage()},
		{panelConnections, "Connections", s.tablePage(panelConnections, s.connectionsData)},
		{panelDERPrograms, "DER programs", s.tablePage(panelDERPrograms, s.derProgramsData)},
		{panelControlFlow, "Control flow", s.tablePage(panelControlFlow, s.controlFlowData)},
		{panelGraphInput, "Graph: device status", s.graphPage()},
	}
	if s.monitor != nil {
		views = append(views, struct {
			id, label string
			page      http.Handler
		}{panelBusMonitor, "Bus monitor", s.monitorPage()})
	}
	out := make([]sep2admin.Panel, 0, len(views)+2)
	for i, v := range views {
		out = append(out, sep2admin.Panel{
			ID:                v.id,
			Label:             v.label,
			Placement:         sep2admin.ExtensionSlot(i + 1),
			DescriptorVersion: sep2admin.CurrentDescriptorVersion,
			Page:              v.page,
		})
	}
	if s.sender != nil {
		out = append(out, s.sender.switchPanel(len(out)+1))
		out = append(out, s.sender.panel(len(out)+1))
	}
	return out
}

func descriptor(sections ...sep2admin.Section) sep2admin.Descriptor {
	return sep2admin.Descriptor{Version: sep2admin.CurrentDescriptorVersion, Sections: sections}
}

func text(v string) sep2admin.Cell { return sep2admin.TextCell(sep2admin.Value(v)) }

// timeCell shows t in the JSON routes' format. The descriptor refuses a
// zero time, which an unset field would be, so that shows as "-".
func timeCell(t time.Time) sep2admin.Cell {
	if t.IsZero() {
		return text("-")
	}
	return sep2admin.TimeCell(t, sep2admin.Value(t.Format(timeFormat)))
}

// orDash shows an empty value as "-", so a blank cell reads as no value.
func orDash(v string) sep2admin.Cell {
	if v == "" {
		return text("-")
	}
	return text(v)
}

// linkOrText is a link when sep2admin's own href rule accepts href, and
// escaped text otherwise. Checking by encoding the cell keeps one rule:
// an unsafe link built into a View would fail the whole panel.
func linkOrText(href, label string) sep2admin.Cell {
	link := sep2admin.LinkCell(href, sep2admin.Value(label))
	if _, err := json.Marshal(link); err != nil {
		return text(href)
	}
	return link
}

// jsonText renders a delta value as JSON, so a string, a number and an
// object stay distinguishable.
func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// clientSnapshot is the connobs snapshot with each client's last-seen time
// moved to the shared recorder's when that is later. connobs sees every
// authenticated request, including ones the ACL then refuses; the recorder
// sees only accepted ones, so a device in a refusal loop must stay visible
// here. The count and paths stay connobs's, since the recorder keeps no
// paths and its count is accepted requests only. The Devices tab reads the
// recorder alone, so it shows the last accepted request.
func (s *Server) clientSnapshot() connobs.Snapshot {
	snap := s.clients.Snapshot()
	if s.activity == nil {
		return snap
	}
	now := s.now()
	clients := make([]connobs.ClientSnapshot, len(snap.Clients))
	for i, c := range snap.Clients {
		if last, _, ok := s.activity.Last(c.LFDI); ok && last.After(c.LastSeen) {
			c.LastSeen = last.UTC()
			c.Age = max(now.Sub(last), 0)
		}
		clients[i] = c
	}
	snap.Clients = clients
	return snap
}

// clientConnected reports whether c was seen strictly within the idle
// threshold, so a client exactly at it is already idle.
func (s *Server) clientConnected(c connobs.ClientSnapshot) bool {
	return c.Age < s.idleAfter
}
