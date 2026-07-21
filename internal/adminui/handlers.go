package adminui

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

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
	mux.Handle("/", s.spaHandler())
	return mux
}

// healthResponse is the fixed, no-secret payload /api/health returns:
// just enough for a caller to confirm the admin UI is reachable and
// authenticated, nothing about bridge internals.
type healthResponse struct {
	Status string `json:"status"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
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
type derWithOwnerResponse struct {
	EndDeviceID string `json:"edevId"`
	ID          string `json:"id"`
	Href        string `json:"href"`
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
type derProgramResponse struct {
	EndDeviceID string `json:"edevId"`
	ID          string `json:"id"`
	Href        string `json:"href"`
	MRID        string `json:"mrid"`
	Description string `json:"description"`
	Primacy     uint8  `json:"primacy"`
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
				EndDeviceID: edev.ID,
				ID:          p.ID,
				Href:        p.Href,
				MRID:        p.MRID,
				Description: p.Description,
				Primacy:     p.Primacy,
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
			AppliedAt: snap.Last.AppliedAt.Format("2006-01-02T15:04:05.000Z07:00"),
		}
	}
	writeJSON(w, http.StatusOK, resp)
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
