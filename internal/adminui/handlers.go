package adminui

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// timeFormat is the RFC 3339 layout (millisecond precision, UTC offset
// preserved) every timestamp in the JSON routes and panels uses.
const timeFormat = "2006-01-02T15:04:05.000Z07:00"

// healthResponse is the /api/health payload and the health panel's data.
// Every field comes from state this Server holds; a field it cannot reach
// (DER kind) is absent rather than invented.
type healthResponse struct {
	// Status is the fixed liveness value, kept first for readers that
	// only check it.
	Status string `json:"status"`

	StompConnected bool   `json:"stompConnected"`
	MTLSListener   string `json:"mtlsListener"`

	// ServerSFDI and ServerLFDI are this bridge's own server identity,
	// distinct from any served device's.
	ServerSFDI string `json:"serverSfdi"`
	ServerLFDI string `json:"serverLfdi"`

	FeederMRID   string `json:"feederMrid"`
	SimulationID string `json:"simulationId"`

	// PlaceholderCount + CertificateCount == RegistryCount always holds.
	RegistryCount    int `json:"registryCount"`
	PlaceholderCount int `json:"placeholderCount"`
	CertificateCount int `json:"certificateCount"`

	// UptimeSeconds is whole seconds since New, truncated.
	UptimeSeconds int64 `json:"uptimeSeconds"`

	// SORLink is serialized as "" when unset, never omitted.
	SORLink string `json:"sorLink"`
}

func (s *Server) health() healthResponse {
	entries := s.registry.Snapshot()
	placeholderCount := 0
	for _, e := range entries {
		if e.Placeholder {
			placeholderCount++
		}
	}
	identity := s.identity.Identity()
	return healthResponse{
		Status:           "ok",
		StompConnected:   s.stomp.IsConnected(),
		MTLSListener:     s.identity.Addr(),
		ServerSFDI:       identity.SFDI,
		ServerLFDI:       identity.LFDI,
		FeederMRID:       s.cfg.FeederMRID,
		SimulationID:     s.cfg.SimulationID,
		RegistryCount:    len(entries),
		PlaceholderCount: placeholderCount,
		CertificateCount: len(entries) - placeholderCount,
		UptimeSeconds:    int64(time.Since(s.startedAt).Seconds()),
		SORLink:          s.cfg.SORLink,
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.health())
}

// registryEntryResponse is one /api/registry element. A distinct type, so
// a field registry.Entry gains later does not reach the wire without a
// decision. The client config generator reads mrid, name, lfdi and
// placeholder.
type registryEntryResponse struct {
	MRID        string `json:"mrid"`
	Name        string `json:"name"`
	LFDI        string `json:"lfdi"`
	SFDI        string `json:"sfdi"`
	Placeholder bool   `json:"placeholder"`
}

// handleRegistry serves the registry as a JSON array, [] when empty.
func (s *Server) handleRegistry(w http.ResponseWriter, _ *http.Request) {
	entries := s.registry.Snapshot()
	out := make([]registryEntryResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, registryEntryResponse{MRID: e.MRID, Name: e.Name, LFDI: e.LFDI, SFDI: e.SFDI, Placeholder: e.Placeholder})
	}
	writeJSON(w, http.StatusOK, out)
}

// discoveredDER is one DER of one served EndDevice. FeederMRID is the
// bridge's single configured feeder, stamped on every entry; it is not a
// per-DER value.
type discoveredDER struct {
	EndDeviceID string
	ID          string
	Href        string
	FeederMRID  string
}

// discoveredDERs flattens every served EndDevice's DERs, each with the
// owning EndDevice's ID, since a DER's ID means nothing without it.
func (s *Server) discoveredDERs(ctx context.Context) ([]discoveredDER, error) {
	edevs, err := s.devices.EndDevices(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading end devices: %w", err)
	}
	var out []discoveredDER
	for _, edev := range edevs {
		for _, der := range edev.DERs {
			out = append(out, discoveredDER{EndDeviceID: edev.ID, ID: der.ID, Href: der.Href, FeederMRID: s.cfg.FeederMRID})
		}
	}
	return out, nil
}

// servedProgram is one DERProgram with its owning EndDevice's ID.
type servedProgram struct {
	EndDeviceID string
	sep2embed.DERProgramSnapshot
}

// servedDERPrograms lists every served EndDevice's DERPrograms. It reads
// in two steps because sep2embed scopes programs to one device.
func (s *Server) servedDERPrograms(ctx context.Context, edevs []sep2embed.EndDeviceSnapshot) ([]servedProgram, error) {
	var out []servedProgram
	for _, edev := range edevs {
		programs, err := s.programs.DERPrograms(ctx, edev.ID)
		if err != nil {
			return nil, fmt.Errorf("reading der programs of %s: %w", edev.ID, err)
		}
		for _, p := range programs {
			out = append(out, servedProgram{EndDeviceID: edev.ID, DERProgramSnapshot: p})
		}
	}
	return out, nil
}

// clientSnapshotResponse mirrors connobs.ClientSnapshot's exported fields.
// A distinct type, so a field connobs adds later does not appear here
// without a decision.
type clientSnapshotResponse struct {
	LFDI         string   `json:"lfdi"`
	LastSeen     string   `json:"lastSeen"`
	RequestCount uint64   `json:"requestCount"`
	Paths        []string `json:"paths"`

	// Connected is true when the client was seen within the idle
	// threshold; an idle client stays listed with it false. AgeSeconds is
	// whole seconds since LastSeen, truncated.
	Connected  bool  `json:"connected"`
	AgeSeconds int64 `json:"ageSeconds"`
}

// handshakeAttemptResponse mirrors connobs.HandshakeAttempt's exported
// fields.
type handshakeAttemptResponse struct {
	LFDI       string `json:"lfdi"`
	RemoteAddr string `json:"remoteAddr"`
	Accepted   bool   `json:"accepted"`
	Reason     string `json:"reason"`
	Known      bool   `json:"known"`
	At         string `json:"at"`
}

// clientsResponse mirrors connobs.Snapshot, plus ObservationDisabled so a
// reader can tell "nothing connected" from "the observer is not wired".
type clientsResponse struct {
	Clients             []clientSnapshotResponse   `json:"clients"`
	Handshakes          []handshakeAttemptResponse `json:"handshakes"`
	ObservationDisabled bool                       `json:"observationDisabled"`
}

// handleClients reports the connection observer's state: which LFDIs
// have made requests, and the recent mTLS handshake attempts.
func (s *Server) handleClients(w http.ResponseWriter, _ *http.Request) {
	snap := s.clients.Snapshot()

	clients := make([]clientSnapshotResponse, 0, len(snap.Clients))
	for _, c := range snap.Clients {
		clients = append(clients, clientSnapshotResponse{
			LFDI:         c.LFDI,
			LastSeen:     c.LastSeen.Format(timeFormat),
			RequestCount: c.RequestCount,
			Paths:        c.Paths,
			Connected:    s.clientConnected(c),
			AgeSeconds:   int64(c.Age / time.Second),
		})
	}

	handshakes := make([]handshakeAttemptResponse, 0, len(snap.Handshakes))
	for _, h := range snap.Handshakes {
		handshakes = append(handshakes, handshakeAttemptResponse{
			LFDI:       h.LFDI,
			RemoteAddr: h.RemoteAddr,
			Accepted:   h.Accepted,
			Reason:     h.Reason,
			Known:      h.Known,
			At:         h.At.Format(timeFormat),
		})
	}

	writeJSON(w, http.StatusOK, clientsResponse{
		Clients:             clients,
		Handshakes:          handshakes,
		ObservationDisabled: s.cfg.ObservationDisabled,
	})
}

// writeJSON encodes v with the given status. An encode failure after the
// header is written can only be logged.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("adminui: encode response: %v", err)
	}
}
