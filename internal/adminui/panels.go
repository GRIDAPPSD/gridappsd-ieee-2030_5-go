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
	panelRegistry    = "gridappsd-registry"
	panelDERs        = "gridappsd-ders"
	panelServed      = "gridappsd-served"
	panelClients     = "gridappsd-clients"
	panelControlFlow = "gridappsd-controlflow"
)

// panels are the bridge's views as admin plane tabs. The shell lists
// extension panels by (Rank, ID), so Rank keeps the old bridge nav order.
func (s *Server) panels() []sep2admin.Panel {
	views := []struct {
		id, label string
		view      sep2admin.ViewFunc
	}{
		{panelHealth, "Bridge health", s.healthView},
		{panelRegistry, "Registry", s.registryView},
		{panelDERs, "Discovered DERs", s.dersView},
		{panelServed, "Served resources", s.servedView},
		{panelClients, "Connected clients", s.clientsView},
		{panelControlFlow, "Control flow", s.controlFlowView},
	}
	out := make([]sep2admin.Panel, 0, len(views))
	for i, v := range views {
		out = append(out, sep2admin.Panel{
			ID:                v.id,
			Label:             v.label,
			Placement:         sep2admin.ExtensionSlot(i + 1),
			DescriptorVersion: sep2admin.CurrentDescriptorVersion,
			View:              v.view,
		})
	}
	return out
}

func descriptor(sections ...sep2admin.Section) sep2admin.Descriptor {
	return sep2admin.Descriptor{Version: sep2admin.CurrentDescriptorVersion, Sections: sections}
}

func table(heading, empty string, prose []string, columns []string, rows []sep2admin.Row) sep2admin.Section {
	return sep2admin.Section{
		Heading: heading,
		Prose:   prose,
		Empty:   empty,
		Body:    sep2admin.NewTableBody(sep2admin.TableBody{Columns: columns, Rows: rows}),
	}
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

// orDash shows an empty value as "-", as the old views did.
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
	return descriptor(definitions("Bridge", "", sep2admin.DefinitionGroup{Entries: []sep2admin.DefinitionEntry{
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
	}})), nil
}

func (s *Server) registryView(context.Context) (sep2admin.Descriptor, error) {
	entries := s.registry.Snapshot()
	rows := make([]sep2admin.Row, 0, len(entries))
	for _, e := range entries {
		// A placeholder LFDI is a stand-in, not a device identity, so the
		// operator must be able to tell the two apart at a glance.
		identity := sep2admin.BadgeCell(sep2admin.BadgeOK, "certificate")
		if e.Placeholder {
			identity = sep2admin.BadgeCell(sep2admin.BadgeWarn, "placeholder")
		}
		rows = append(rows, sep2admin.Row{text(e.MRID), text(e.Name), text(e.LFDI), text(e.SFDI), identity})
	}
	return descriptor(table("Registry map", "No registry entries yet.",
		[]string{"mRID to LFDI/SFDI identity mapping. Read only."},
		[]string{"mRID", "Name", "LFDI", "SFDI", "Identity"}, rows)), nil
}

func (s *Server) dersView(ctx context.Context) (sep2admin.Descriptor, error) {
	ders, err := s.discoveredDERs(ctx)
	if err != nil {
		return sep2admin.Descriptor{}, err
	}
	rows := make([]sep2admin.Row, 0, len(ders))
	for _, d := range ders {
		rows = append(rows, sep2admin.Row{text(d.EndDeviceID), text(d.ID), text(d.Href), text(d.FeederMRID)})
	}
	return descriptor(table("Discovered DERs",
		"No DERs discovered (the feeder model had no PowerElectronicsConnection).",
		[]string{
			"DER resources discovered from the CIM model, joined to the owning EndDevice. Read only.",
			"DER type (inverter, solar, battery) is not yet exposed.",
		},
		[]string{"EndDevice", "DER ID", "Href", "Feeder mRID"}, rows)), nil
}

func (s *Server) servedView(ctx context.Context) (sep2admin.Descriptor, error) {
	edevs, err := s.devices.EndDevices(ctx)
	if err != nil {
		return sep2admin.Descriptor{}, fmt.Errorf("reading end devices: %w", err)
	}
	programs, err := s.servedDERPrograms(ctx, edevs)
	if err != nil {
		return sep2admin.Descriptor{}, err
	}

	edevRows := make([]sep2admin.Row, 0, len(edevs))
	for _, e := range edevs {
		enabled := "no"
		if e.Enabled {
			enabled = "yes"
		}
		derIDs := make([]string, 0, len(e.DERs))
		for _, d := range e.DERs {
			derIDs = append(derIDs, d.ID)
		}
		edevRows = append(edevRows, sep2admin.Row{
			text(e.ID), text(e.LFDI), text(e.SFDI), text(e.Href), text(enabled),
			number(len(e.DERs)), text(strings.Join(derIDs, ", ")),
		})
	}

	programRows := make([]sep2admin.Row, 0, len(programs))
	for _, p := range programs {
		defaultControl := text("absent")
		if p.DefaultDERControlLink != "" {
			defaultControl = text(p.DefaultDERControlLink)
		}
		programRows = append(programRows, sep2admin.Row{
			text(p.EndDeviceID), text(p.ID), text(p.MRID), text(p.Description),
			number(p.Primacy), text(p.Href), defaultControl,
		})
	}

	return descriptor(
		table("EndDevices", "No EndDevices served.",
			[]string{"EndDevices, DERs, and DERPrograms as served by the embedded server. Read only."},
			[]string{"ID", "LFDI", "SFDI", "Href", "Enabled", "DERs", "DER IDs"}, edevRows),
		table("DER programs", "No DERPrograms served.", nil,
			[]string{"EndDevice", "ID", "MRID", "Description", "Primacy", "Href", "DefaultDERControl"}, programRows),
	), nil
}

func (s *Server) controlFlowView(context.Context) (sep2admin.Descriptor, error) {
	snap := s.flow.Snapshot()
	state := definitions("Control flow", "", sep2admin.DefinitionGroup{
		Heading: "Topics",
		Entries: []sep2admin.DefinitionEntry{
			{Key: "Simulation output topic", Value: text(snap.OutputTopic)},
			{Key: "Control delta input topic", Value: text(snap.InputTopic)},
		},
	}, sep2admin.DefinitionGroup{
		Heading: "Counters",
		Entries: []sep2admin.DefinitionEntry{
			{Key: "Applied", Value: number(snap.Applied)},
			{Key: "Skipped", Value: number(snap.Skipped)},
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

// jsonText renders a delta value as the JSON route encoded it.
func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func (s *Server) clientsView(ctx context.Context) (sep2admin.Descriptor, error) {
	snap := s.clients.Snapshot()
	disabled := s.cfg.ObservationDisabled

	clientRows := make([]sep2admin.Row, 0, len(snap.Clients))
	for _, c := range snap.Clients {
		clientRows = append(clientRows, sep2admin.Row{
			text(c.LFDI), timeCell(c.LastSeen), number(c.RequestCount), text(strings.Join(c.Paths, ", ")),
		})
	}
	clientsEmpty := "No clients connected yet."
	if disabled {
		clientsEmpty = "Connection observation is disabled (SEP2_ENABLE_CCM): this list cannot show which clients, if any, are connected."
	}

	handshakeRows := make([]sep2admin.Row, 0, len(snap.Handshakes))
	for _, h := range snap.Handshakes {
		result := sep2admin.BadgeCell(sep2admin.BadgeError, "rejected")
		if h.Accepted {
			result = sep2admin.BadgeCell(sep2admin.BadgeOK, "accepted")
		}
		known := sep2admin.BadgeCell(sep2admin.BadgeNeutral, "unknown")
		if h.Known {
			known = sep2admin.BadgeCell(sep2admin.BadgeOK, "known")
		}
		handshakeRows = append(handshakeRows, sep2admin.Row{
			text(h.LFDI), orDash(h.RemoteAddr), result, orDash(h.Reason), known, timeCell(h.At),
		})
	}
	handshakesEmpty := "No handshake attempts recorded yet."
	if disabled {
		handshakesEmpty = "Connection observation is disabled (SEP2_ENABLE_CCM): this list cannot show handshake attempts."
	}

	return descriptor(
		table("Connected clients", clientsEmpty,
			[]string{"LFDIs that have issued authenticated requests. Read only."},
			[]string{"LFDI", "Last seen", "Requests", "Paths touched"}, clientRows),
		s.servedStatusSection(ctx, snap.Clients, disabled),
		table("Handshake attempts (cert validity)", handshakesEmpty, nil,
			[]string{"LFDI", "Remote address", "Result", "Reason", "Known", "At"}, handshakeRows),
	), nil
}

// servedStatusSection cross-references the served EndDevices with the
// client snapshot by LFDI, so a device that never made a request reads
// "never connected". With the observer off nothing can show a
// connection, so every device reads "unknown" instead of a false claim.
// A roster read failure empties only this section, as the old view did.
func (s *Server) servedStatusSection(ctx context.Context, clients []connobs.ClientSnapshot, disabled bool) sep2admin.Section {
	const heading = "Served EndDevices: connection status"
	columns := []string{"EndDevice", "LFDI", "Status", "Last seen", "Requests"}
	prose := []string{"Cross references the served roster against the connected-client snapshot by LFDI."}
	if disabled {
		prose = []string{"The connection observer is disabled on this bridge (SEP2_ENABLE_CCM). Status is unknown for every served device, not \"never connected\"."}
	}

	edevs, err := s.devices.EndDevices(ctx)
	if err != nil {
		log.Printf("adminui: panel %s: reading end devices: %v", panelClients, err)
		return table(heading, "Served EndDevice roster unavailable.", prose, columns, nil)
	}

	byLFDI := make(map[string]connobs.ClientSnapshot, len(clients))
	for _, c := range clients {
		byLFDI[c.LFDI] = c
	}
	rows := make([]sep2admin.Row, 0, len(edevs))
	for _, e := range edevs {
		c, connected := byLFDI[e.LFDI]
		var status sep2admin.Cell
		switch {
		case connected:
			status = sep2admin.BadgeCell(sep2admin.BadgeOK, "connected")
		case disabled:
			status = sep2admin.BadgeCell(sep2admin.BadgeNeutral, "unknown")
		default:
			status = sep2admin.BadgeCell(sep2admin.BadgeWarn, "never connected")
		}
		lastSeen, requests := text("-"), text("-")
		if connected {
			lastSeen, requests = timeCell(c.LastSeen), number(c.RequestCount)
		}
		rows = append(rows, sep2admin.Row{text(e.ID), text(e.LFDI), status, lastSeen, requests})
	}
	return table(heading, "No EndDevices served.", prose, columns, rows)
}
