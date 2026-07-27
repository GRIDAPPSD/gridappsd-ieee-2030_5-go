package adminui

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// timeFormat is the fixed RFC 3339 (millisecond precision, UTC offset
// preserved) layout every timestamp field in this package's JSON
// responses uses, matching the format handleControlFlow's AppliedAt
// field already established.
const timeFormat = "2006-01-02T15:04:05.000Z07:00"

// mux registers the GAGO-059 read only JSON endpoints, plus the
// GAGO-060 SPA handler on "/". Every /api/... handler here is a pure
// reader over the sources injected into Server by New: none of them
// ever writes to the registry, the embedded server, or the control
// observation hook. requireGET (in the outer middleware chain built by
// buildHandler) already rejects any non-GET method before a handler
// here runs, so no handler needs its own method check; that includes
// the SPA handler, which is equally GET only.
//
// "/" is registered last in this list purely for readability: Go's
// http.ServeMux dispatches by longest-prefix match on the registered
// patterns, not registration order, so each exact /api/... pattern
// above always wins over the "/" catch-all regardless of where "/" sits
// in this function. The SPA handler's own internal check (spa.go) is
// the second, explicit line of defense against ever shadowing an
// unmatched /api/... path with index.html.
func (s *Server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/registry", s.handleRegistry)
	mux.HandleFunc("/api/ders", s.handleDERs)
	mux.HandleFunc("/api/served/edev", s.handleServedEndDevices)
	mux.HandleFunc("/api/served/derprogram", s.handleServedDERPrograms)
	mux.HandleFunc("/api/controlflow", s.handleControlFlow)
	mux.HandleFunc("/api/clients", s.handleClients)
	mux.Handle("/", s.spaHandler())
	return mux
}

// healthResponse is GAGO-074's enriched /api/health payload. Every
// field is sourced from state this Server already holds (the registry,
// the injected identity/STOMP sources, and its own config/startedAt);
// no field here is fabricated. Fields that are genuinely not reachable
// from this Server's current dependencies are simply absent from this
// struct rather than filled with an invented value; see the card
// report for the one field (DER kind/type) that stays out of scope for
// this reason.
type healthResponse struct {
	// Status is the fixed, pre-existing "ok" liveness value. Kept as
	// the first field, unchanged, so an existing consumer that only
	// reads Status keeps working unmodified (additive only).
	Status string `json:"status"`

	// StompConnected reports the GridAPPS-D message bus's current
	// connection state, from the injected StompSource.
	StompConnected bool `json:"stompConnected"`

	// MTLSListener is the embedded IEEE 2030.5 server's bound listener
	// address, from the injected IdentitySource.
	MTLSListener string `json:"mtlsListener"`

	// ServerSFDI, ServerLFDI are this bridge's own embedded server
	// identity (spec sections 6.3.3 and 6.3.4 respectively), derived
	// from its own leaf certificate. Distinct from any served device's
	// SFDI/LFDI.
	ServerSFDI string `json:"serverSfdi"`
	ServerLFDI string `json:"serverLfdi"`

	// FeederMRID, SimulationID are the bridge's own configured values
	// (config.FeederMRID / config.SimulationID), passed straight
	// through. Empty means unconfigured, not an error.
	FeederMRID   string `json:"feederMrid"`
	SimulationID string `json:"simulationId"`

	// RegistryCount is the total number of entries in the mRID to LFDI
	// registry. PlaceholderCount and CertificateCount partition that
	// same total by registry.Entry.Placeholder, so
	// PlaceholderCount + CertificateCount == RegistryCount always
	// holds.
	RegistryCount    int `json:"registryCount"`
	PlaceholderCount int `json:"placeholderCount"`
	CertificateCount int `json:"certificateCount"`

	// UptimeSeconds is the whole number of seconds since this Server
	// was constructed (New's startedAt), truncated, not rounded.
	UptimeSeconds int64 `json:"uptimeSeconds"`

	// SORLink is the optional server-of-record dashboard URL (GAGO-075,
	// SEP2_ADMIN_UI_SOR_LINK). Serialized as an empty string, never
	// omitted, when unset: a future frontend reads an always-present
	// field rather than having to distinguish "absent" from "present
	// but empty" for a value where those two states carry no different
	// meaning (this is the explicit serialization-contract choice for
	// this field: empty means unset, full stop).
	SORLink string `json:"sorLink"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	entries := s.registry.Snapshot()
	placeholderCount := 0
	for _, e := range entries {
		if e.Placeholder {
			placeholderCount++
		}
	}
	certificateCount := len(entries) - placeholderCount

	identity := s.identity.Identity()

	writeJSON(w, http.StatusOK, healthResponse{
		Status:           "ok",
		StompConnected:   s.stomp.IsConnected(),
		MTLSListener:     s.identity.Addr(),
		ServerSFDI:       identity.SFDI,
		ServerLFDI:       identity.LFDI,
		FeederMRID:       s.cfg.FeederMRID,
		SimulationID:     s.cfg.SimulationID,
		RegistryCount:    len(entries),
		PlaceholderCount: placeholderCount,
		CertificateCount: certificateCount,
		UptimeSeconds:    int64(time.Since(s.startedAt).Seconds()),
		SORLink:          s.cfg.SORLink,
	})
}

// registryEntryResponse mirrors registry.Entry's exported fields
// exactly. It is a distinct type (rather than serializing
// registry.Entry directly) so a future field added to Entry for
// internal bookkeeping does not silently leak into this JSON response
// without an explicit decision to add it here too.
type registryEntryResponse struct {
	MRID        string `json:"mrid"`
	Name        string `json:"name"`
	LFDI        string `json:"lfdi"`
	SFDI        string `json:"sfdi"`
	Placeholder bool   `json:"placeholder"`
}

func (s *Server) handleRegistry(w http.ResponseWriter, r *http.Request) {
	entries := s.registry.Snapshot()
	out := make([]registryEntryResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, registryEntryResponseOf(e))
	}
	writeJSON(w, http.StatusOK, out)
}

func registryEntryResponseOf(e registry.Entry) registryEntryResponse {
	return registryEntryResponse{
		MRID:        e.MRID,
		Name:        e.Name,
		LFDI:        e.LFDI,
		SFDI:        e.SFDI,
		Placeholder: e.Placeholder,
	}
}

// derResponse mirrors sep2embed.DERSnapshot's exported fields.
type derResponse struct {
	ID   string `json:"id"`
	Href string `json:"href"`
}

// handleDERs reports every DER across every served EndDevice, flattened
// into a single list. Each entry carries the owning EndDevice's ID
// (edevId) alongside the DER's own ID and Href, since a DER's identity
// is only meaningful relative to the device that serves it.
//
// FeederMRID (GAGO-074) is the bridge's own configured feeder mRID
// (s.cfg.FeederMRID), stamped onto every entry: it is not a per-DER
// value, since this bridge enumerates every DER from a single
// configured feeder. A DER kind/type field (inverter, solar, battery)
// was scoped for this endpoint too, but is not added here: see the
// card report's Finding, it is not reachable from this Server's
// current dependencies without a CIM query change.
type derWithOwnerResponse struct {
	EndDeviceID string `json:"edevId"`
	ID          string `json:"id"`
	Href        string `json:"href"`
	FeederMRID  string `json:"feederMrid"`
}

func (s *Server) handleDERs(w http.ResponseWriter, r *http.Request) {
	edevs, err := s.devices.EndDevices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reading end devices")
		return
	}
	out := make([]derWithOwnerResponse, 0)
	for _, edev := range edevs {
		for _, der := range edev.DERs {
			out = append(out, derWithOwnerResponse{
				EndDeviceID: edev.ID,
				ID:          der.ID,
				Href:        der.Href,
				FeederMRID:  s.cfg.FeederMRID,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// endDeviceResponse mirrors sep2embed.EndDeviceSnapshot's exported
// fields, with DERs rendered as derResponse rather than the raw
// sep2embed type, per the same explicit-shape rationale as
// registryEntryResponse above.
type endDeviceResponse struct {
	ID      string        `json:"id"`
	LFDI    string        `json:"lfdi"`
	SFDI    string        `json:"sfdi"`
	Href    string        `json:"href"`
	Enabled bool          `json:"enabled"`
	DERs    []derResponse `json:"ders"`
}

func endDeviceResponseOf(edev sep2embed.EndDeviceSnapshot) endDeviceResponse {
	ders := make([]derResponse, 0, len(edev.DERs))
	for _, d := range edev.DERs {
		ders = append(ders, derResponse{ID: d.ID, Href: d.Href})
	}
	return endDeviceResponse{
		ID:      edev.ID,
		LFDI:    edev.LFDI,
		SFDI:    edev.SFDI,
		Href:    edev.Href,
		Enabled: edev.Enabled,
		DERs:    ders,
	}
}

func (s *Server) handleServedEndDevices(w http.ResponseWriter, r *http.Request) {
	edevs, err := s.devices.EndDevices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reading end devices")
		return
	}
	out := make([]endDeviceResponse, 0, len(edevs))
	for _, edev := range edevs {
		out = append(out, endDeviceResponseOf(edev))
	}
	writeJSON(w, http.StatusOK, out)
}

// derProgramResponse mirrors sep2embed.DERProgramSnapshot's exported
// fields, plus the owning EndDevice's ID, since a DERProgram is scoped
// to the device serving it.
//
// DefaultDERControlLink (GAGO-074) is the CSIP-critical addition: the
// href of this program's DefaultDERControl singleton (spec section
// CSIP profile requires every DERProgram to carry one). It is sourced
// straight from sep2embed.DERProgramSnapshot.DefaultDERControlLink,
// which core already populates in full; no snapshot.go change was
// needed to add this field.
type derProgramResponse struct {
	EndDeviceID           string `json:"edevId"`
	ID                    string `json:"id"`
	Href                  string `json:"href"`
	MRID                  string `json:"mrid"`
	Description           string `json:"description"`
	Primacy               uint8  `json:"primacy"`
	DefaultDERControlLink string `json:"defaultDerControlLink"`
}

// handleServedDERPrograms reports every DERProgram across every served
// EndDevice. It first lists EndDevices via s.devices, then calls
// s.programs.DERPrograms for each device's ID: this two-step read
// matches sep2embed's own layering (DERPrograms takes an edevID, it does
// not itself know the full device list), and is why Server needs both
// an EndDeviceSource and a DERProgramSource rather than a single
// combined interface.
func (s *Server) handleServedDERPrograms(w http.ResponseWriter, r *http.Request) {
	edevs, err := s.devices.EndDevices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reading end devices")
		return
	}
	out := make([]derProgramResponse, 0)
	for _, edev := range edevs {
		programs, err := s.programs.DERPrograms(r.Context(), edev.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "reading der programs")
			return
		}
		for _, p := range programs {
			out = append(out, derProgramResponse{
				EndDeviceID:           edev.ID,
				ID:                    p.ID,
				Href:                  p.Href,
				MRID:                  p.MRID,
				Description:           p.Description,
				Primacy:               p.Primacy,
				DefaultDERControlLink: p.DefaultDERControlLink,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// lastDeltaResponse mirrors controlobs.LastDelta's exported fields.
type lastDeltaResponse struct {
	Object    string `json:"object"`
	Attribute string `json:"attribute"`
	Value     any    `json:"value"`
	AppliedAt string `json:"appliedAt"`
}

// controlFlowResponse mirrors controlobs.Snapshot's exported fields.
// Every field here traces to the GAGO-057 hook's own recorded state
// (applied/skipped counters, the last applied delta, and the two STOMP
// topic strings): none of it is, or ever derives from, a credential.
type controlFlowResponse struct {
	Applied     uint64             `json:"applied"`
	Skipped     uint64             `json:"skipped"`
	Last        *lastDeltaResponse `json:"last"`
	OutputTopic string             `json:"outputTopic"`
	InputTopic  string             `json:"inputTopic"`
}

func (s *Server) handleControlFlow(w http.ResponseWriter, r *http.Request) {
	snap := s.flow.Snapshot()
	resp := controlFlowResponse{
		Applied:     snap.Applied,
		Skipped:     snap.Skipped,
		OutputTopic: snap.OutputTopic,
		InputTopic:  snap.InputTopic,
	}
	if snap.Last != nil {
		resp.Last = &lastDeltaResponse{
			Object:    snap.Last.Object,
			Attribute: snap.Last.Attribute,
			Value:     snap.Last.Value,
			AppliedAt: snap.Last.AppliedAt.Format(timeFormat),
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// clientSnapshotResponse mirrors connobs.ClientSnapshot's exported
// fields. A distinct type, per this package's established
// explicit-shape rationale (see registryEntryResponse): connobs is free
// to add an internal-only field later without it silently appearing
// here.
type clientSnapshotResponse struct {
	LFDI         string   `json:"lfdi"`
	LastSeen     string   `json:"lastSeen"`
	RequestCount uint64   `json:"requestCount"`
	Paths        []string `json:"paths"`
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

// clientsResponse mirrors connobs.Snapshot's exported fields.
type clientsResponse struct {
	Clients    []clientSnapshotResponse   `json:"clients"`
	Handshakes []handshakeAttemptResponse `json:"handshakes"`
}

// handleClients reports the GAGO-090 per-LFDI connection observer's
// current state: which LFDIs have issued requests (and what they
// touched), plus the recent mTLS handshake attempt log, accepted and
// rejected alike. Every field traces to the connobs.Hook's own recorded
// state; this handler never fabricates or defaults a value the hook did
// not itself record.
func (s *Server) handleClients(w http.ResponseWriter, r *http.Request) {
	snap := s.clients.Snapshot()

	clients := make([]clientSnapshotResponse, 0, len(snap.Clients))
	for _, c := range snap.Clients {
		clients = append(clients, clientSnapshotResponse{
			LFDI:         c.LFDI,
			LastSeen:     c.LastSeen.Format(timeFormat),
			RequestCount: c.RequestCount,
			Paths:        c.Paths,
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

	writeJSON(w, http.StatusOK, clientsResponse{Clients: clients, Handshakes: handshakes})
}

// writeJSON encodes v as the response body with the given status code
// and a JSON content type. It logs (never to the response body) when
// encoding fails after headers are already written, since at that point
// there is no way to report the failure to the caller.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("adminui: encode response: %v", err)
	}
}

// errorResponse is the fixed shape every non-2xx response uses. message
// is always a short, static, caller-safe string: never an underlying
// error's own text, which could echo internal detail back to the
// caller.
type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}
