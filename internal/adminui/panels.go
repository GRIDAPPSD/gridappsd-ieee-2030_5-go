package adminui

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
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
		view      sep2admin.ViewFunc
	}{
		{panelHealth, "Bridge health", s.healthView},
		{panelConnections, "Connections", s.connectionsView},
		{panelDERPrograms, "DER programs", s.derProgramsView},
		{panelControlFlow, "Control flow", s.controlFlowView},
		{panelGraphInput, "Graph: device status", s.graphInputView},
	}
	out := make([]sep2admin.Panel, 0, len(views))
	for i, v := range views {
		p := sep2admin.Panel{
			ID:                v.id,
			Label:             v.label,
			Placement:         sep2admin.ExtensionSlot(i + 1),
			DescriptorVersion: sep2admin.CurrentDescriptorVersion,
			View:              v.view,
		}
		if v.id == panelGraphInput {
			// View answers with no selection: the newest batteries.
			p.Picker = &sep2admin.Picker{Choices: s.graphChoices, Select: s.graphSelectView}
		}
		out = append(out, p)
	}
	if s.monitor != nil {
		out = append(out, s.monitor.panel(len(out)+1))
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

func definitions(heading, empty string, groups ...sep2admin.DefinitionGroup) sep2admin.Section {
	return sep2admin.Section{
		Heading: heading,
		Empty:   empty,
		Body:    sep2admin.NewDefinitionListBody(sep2admin.DefinitionListBody{Groups: groups}),
	}
}

func text(v string) sep2admin.Cell { return sep2admin.TextCell(sep2admin.Value(v)) }

func number[T int | int64 | uint8 | uint64](v T) sep2admin.Cell {
	return text(fmt.Sprint(v))
}

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

func (s *Server) healthView(context.Context) (sep2admin.Descriptor, error) {
	h := s.health()
	stomp := sep2admin.BadgeCell(sep2admin.BadgeWarn, "disconnected")
	if h.StompConnected {
		stomp = sep2admin.BadgeCell(sep2admin.BadgeOK, "connected")
	}
	sor := text("not set")
	if h.SORLink != "" {
		sor = linkOrText(h.SORLink, "Server of record dashboard")
	}
	groups := append([]sep2admin.DefinitionGroup{{Entries: []sep2admin.DefinitionEntry{
		{Key: "Status", Value: sep2admin.BadgeCell(sep2admin.BadgeOK, sep2admin.Value(h.Status))},
		{Key: "STOMP connection", Value: stomp},
		{Key: "mTLS listener", Value: text(h.MTLSListener)},
		{Key: "Server SFDI", Value: text(h.ServerSFDI)},
		{Key: "Server LFDI", Value: text(h.ServerLFDI)},
		{Key: "Feeder mRID", Value: text(h.FeederMRID)},
		{Key: "Simulation ID", Value: text(h.SimulationID)},
		{Key: "Registry entries", Value: number(h.RegistryCount)},
		{Key: "Placeholder identities", Value: number(h.PlaceholderCount)},
		{Key: "Certificate derived identities", Value: number(h.CertificateCount)},
		{Key: "Uptime (seconds)", Value: number(h.UptimeSeconds)},
		{Key: "Server of record", Value: sor},
	}}}, s.publishingGroup()...)
	return descriptor(definitions("Bridge", "", groups...)), nil
}

// publishingGroup repeats the sender's switch on the health panel, so the
// state of writes to the simulation is visible without opening the sender.
func (s *Server) publishingGroup() []sep2admin.DefinitionGroup {
	if s.sender == nil {
		return nil
	}
	st := s.sender.s.Publishing()
	badge := sep2admin.BadgeCell(sep2admin.BadgeNeutral, "OFF")
	if st.On {
		badge = sep2admin.BadgeCell(sep2admin.BadgeWarn, "ON")
	}
	return []sep2admin.DefinitionGroup{{Heading: "Bus sender", Entries: []sep2admin.DefinitionEntry{
		{Key: "Publishing", Value: badge},
		{Key: "Last changed", Value: timeCell(st.ChangedAt)},
		{Key: "Changed from", Value: text(st.ChangedBy)},
	}}}
}

// simOutputTopicText shows that no simulation id is configured instead of
// a blank cell, which reads as a fault.
func simOutputTopicText(topic string) string {
	if topic == "" {
		return "not subscribed (no simulation id configured)"
	}
	return topic
}

func (s *Server) controlFlowView(context.Context) (sep2admin.Descriptor, error) {
	snap := s.flow.Snapshot()
	state := definitions("Control flow", "", sep2admin.DefinitionGroup{
		Heading: "Topics",
		Entries: []sep2admin.DefinitionEntry{
			{Key: "Simulation output topic", Value: text(simOutputTopicText(snap.OutputTopic))},
			{Key: "Control delta input topic", Value: text(snap.InputTopic)},
		},
	}, sep2admin.DefinitionGroup{
		Heading: "Counters",
		Entries: []sep2admin.DefinitionEntry{
			{Key: "Applied", Value: number(snap.Applied)},
			{Key: "Restated", Value: number(snap.Restated)},
			{Key: "Skipped", Value: number(snap.Skipped)},
			{Key: "Empty frames", Value: number(snap.EmptyFrames)},
		},
	})

	// No delta yet is its own state, shown as the section's empty text
	// rather than as a delta with blank fields.
	var lastGroups []sep2admin.DefinitionGroup
	if l := snap.Last; l != nil {
		lastGroups = append(lastGroups, sep2admin.DefinitionGroup{Entries: []sep2admin.DefinitionEntry{
			{Key: "Object", Value: text(l.Object)},
			{Key: "Attribute", Value: text(l.Attribute)},
			{Key: "Value", Value: text(jsonText(l.Value))},
			{Key: "Applied at", Value: timeCell(l.AppliedAt)},
		}})
	}
	last := definitions("Last applied delta", "No control delta has been applied yet.", lastGroups...)
	return descriptor(state, last), nil
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

// derProgramsView lists every served DERProgram. It stays its own panel:
// the server's FSAs tab has no hook for an embedder's panel, so it cannot
// be folded in from here.
func (s *Server) derProgramsView(ctx context.Context) (sep2admin.Descriptor, error) {
	edevs, err := s.devices.EndDevices(ctx)
	if err != nil {
		return sep2admin.Descriptor{}, fmt.Errorf("reading end devices: %w", err)
	}
	programs, err := s.servedDERPrograms(ctx, edevs)
	if err != nil {
		return sep2admin.Descriptor{}, err
	}
	rows := make([]sep2admin.Row, 0, len(programs))
	for _, p := range programs {
		defaultControl := text("absent")
		if p.DefaultDERControlLink != "" {
			defaultControl = text(p.DefaultDERControlLink)
		}
		rows = append(rows, sep2admin.Row{
			text(p.EndDeviceID), text(p.ID), text(p.MRID), text(p.Description),
			number(p.Primacy), text(p.Href), defaultControl,
		})
	}
	return tables(table("DER programs", "No DERPrograms served.",
		[]string{"DERPrograms as served by the embedded server. Read only."},
		[]string{"EndDevice", "ID", "MRID", "Description", "Primacy", "Href", "DefaultDERControl"}, rows)), nil
}

// connectionsView is what the Devices tab cannot show: LFDIs that made
// requests but are not a served EndDevice, and every handshake attempt.
func (s *Server) connectionsView(ctx context.Context) (sep2admin.Descriptor, error) {
	snap := s.clientSnapshot()
	disabled := s.cfg.ObservationDisabled
	return tables(s.strangersSection(ctx, snap.Clients, disabled), handshakesSection(snap.Handshakes, disabled)), nil
}

// strangersSection lists the observed clients whose LFDI is not a served
// EndDevice. A roster read failure empties only this section: without the
// roster no client can be called a stranger, so it says so rather than
// listing every client.
func (s *Server) strangersSection(ctx context.Context, clients []connobs.ClientSnapshot, disabled bool) tableSpec {
	const heading = "LFDIs seen with no EndDevice"
	columns := []string{"LFDI", "Status", "Last seen", "Age", "Requests", "Paths touched"}
	empty := "Every LFDI seen is a served EndDevice, or none has connected yet."
	if disabled {
		empty = "Connection observation is disabled (SEP2_ENABLE_CCM): this list cannot show which clients, if any, are connected."
	}
	var prose []string
	edevs, err := s.devices.EndDevices(ctx)
	if err != nil {
		log.Printf("adminui: panel %s: reading end devices: %v", panelConnections, err)
		// Without the roster no client can be called a stranger. The
		// observer-disabled text, when it applies, stays the empty text.
		if !disabled {
			empty = "Served EndDevice roster unavailable."
		}
		return table(heading, empty, []string{fmt.Sprintf("Roster read failed: %v", err)}, columns, nil)
	}
	served := make(map[string]bool, len(edevs))
	for _, e := range edevs {
		served[e.LFDI] = true
	}

	rows := make([]sep2admin.Row, 0)
	connectedCount := 0
	for _, c := range clients {
		if served[c.LFDI] {
			continue
		}
		if s.clientConnected(c) {
			connectedCount++
		}
		rows = append(rows, sep2admin.Row{
			text(c.LFDI), s.clientStatus(c), timeCell(c.LastSeen), ageCell(c.Age), number(c.RequestCount), text(strings.Join(c.Paths, ", ")),
		})
	}
	if disabled {
		prose = []string{"The connection observer is off (SEP2_ENABLE_CCM), so connected and idle counts are not available."}
	} else {
		prose = []string{fmt.Sprintf("%d connected, %d idle. A client is idle once unseen for %s; idle clients stay listed. Read only.",
			connectedCount, len(rows)-connectedCount, s.idleAfter)}
	}
	return table(heading, empty, prose, columns, rows)
}

func handshakesSection(attempts []connobs.HandshakeAttempt, disabled bool) tableSpec {
	rows := make([]sep2admin.Row, 0, len(attempts))
	for _, h := range attempts {
		result := sep2admin.BadgeCell(sep2admin.BadgeError, "rejected")
		if h.Accepted {
			result = sep2admin.BadgeCell(sep2admin.BadgeOK, "accepted")
		}
		known := sep2admin.BadgeCell(sep2admin.BadgeNeutral, "unknown")
		if h.Known {
			known = sep2admin.BadgeCell(sep2admin.BadgeOK, "known")
		}
		rows = append(rows, sep2admin.Row{
			text(h.LFDI), orDash(h.RemoteAddr), result, orDash(h.Reason), known, timeCell(h.At),
		})
	}
	empty := "No handshake attempts recorded yet."
	if disabled {
		empty = "Connection observation is disabled (SEP2_ENABLE_CCM): this list cannot show handshake attempts."
	}
	return table("Handshake attempts (cert validity)", empty, nil,
		[]string{"LFDI", "Remote address", "Result", "Reason", "Known", "At"}, rows)
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

func (s *Server) clientStatus(c connobs.ClientSnapshot) sep2admin.Cell {
	if s.clientConnected(c) {
		return sep2admin.BadgeCell(sep2admin.BadgeOK, "connected")
	}
	return sep2admin.BadgeCell(sep2admin.BadgeWarn, "idle")
}

// ageCell shows an age in whole seconds, such as "3m12s".
func ageCell(d time.Duration) sep2admin.Cell {
	return text(d.Truncate(time.Second).String())
}
