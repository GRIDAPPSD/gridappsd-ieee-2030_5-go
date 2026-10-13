package adminui

import (
	"encoding/json"
	"fmt"
	"net/http"

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

// panelView is one tab: its id, label and page handler.
type panelView struct {
	id, label string
	page      http.Handler
}

// panels are the bridge's views as admin plane tabs, each a page the bridge
// serves. The shell lists extension panels by (Rank, ID), so Rank fixes the
// tab order.
func (s *Server) panels() []sep2admin.Panel {
	views := []panelView{
		{panelHealth, "Bridge health", s.healthPage()},
		{panelConnections, "Connections", s.tablePage(panelConnections, s.connectionsData)},
		{panelDERPrograms, "DER programs", s.tablePage(panelDERPrograms, s.derProgramsData)},
		{panelControlFlow, "Control flow", s.tablePage(panelControlFlow, s.controlFlowData)},
		{panelGraphInput, "Graph: device status", s.graphPage()},
	}
	if s.monitor != nil {
		views = append(views, panelView{panelBusMonitor, "Bus monitor", s.monitorPage()})
	}
	if s.sender != nil {
		views = append(views,
			panelView{panelPublishing, "Bus publishing", s.switchPage()},
			panelView{panelSender, "Bus sender", s.senderPage()},
		)
	}
	out := make([]sep2admin.Panel, 0, len(views))
	for i, v := range views {
		out = append(out, sep2admin.Panel{
			ID:                v.id,
			Label:             v.label,
			Placement:         sep2admin.ExtensionSlot(i + 1),
			DescriptorVersion: sep2admin.CurrentDescriptorVersion,
			Page:              v.page,
		})
	}
	return out
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
