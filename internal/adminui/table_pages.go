package adminui

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/tablepage"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
)

// The table pages answer their data route with a document in the shape of
// the plane's descriptor, so a section reads the same on both. Every
// collection is an array, never null: an empty table is "rows":[] and an
// empty definition list is "groups":[].

// Cell kinds and badge variants the table page script understands.
const (
	cellText  = "text"
	cellBadge = "badge"
	cellTime  = "time"

	badgeOK      = "ok"
	badgeWarn    = "warn"
	badgeError   = "error"
	badgeNeutral = "neutral"
)

type pageCell struct {
	Kind     string `json:"kind"`
	Text     string `json:"text"`
	Badge    string `json:"badge,omitempty"`
	DateTime string `json:"datetime,omitempty"`
}

type pageEntry struct {
	Key   string   `json:"key"`
	Value pageCell `json:"value"`
}

type pageGroup struct {
	Heading string      `json:"heading,omitempty"`
	Entries []pageEntry `json:"entries"`
}

type pageTableBody struct {
	Columns []string     `json:"columns"`
	Rows    [][]pageCell `json:"rows"`
}

type pageListBody struct {
	Groups []pageGroup `json:"groups"`
}

type pageSection struct {
	Kind    string   `json:"kind"`
	Heading string   `json:"heading"`
	Prose   []string `json:"prose"`
	Empty   string   `json:"empty"`
	Body    any      `json:"body"`
}

type pageDoc struct {
	Title    string        `json:"title"`
	Sections []pageSection `json:"sections"`
}

func pText(v string) pageCell { return pageCell{Kind: cellText, Text: v} }

func pNumber[T int | int64 | uint8 | uint64](v T) pageCell { return pText(fmt.Sprint(v)) }

func pBadge(variant, v string) pageCell { return pageCell{Kind: cellBadge, Text: v, Badge: variant} }

// pTime shows t in the JSON routes' format. An unset time shows as "-".
func pTime(t time.Time) pageCell {
	if t.IsZero() {
		return pText("-")
	}
	return pageCell{Kind: cellTime, Text: t.Format(timeFormat), DateTime: t.UTC().Format(time.RFC3339)}
}

// pOrDash shows an empty value as "-", so a blank cell reads as no value.
func pOrDash(v string) pageCell {
	if v == "" {
		return pText("-")
	}
	return pText(v)
}

// pAge shows an age in whole seconds, such as "3m12s".
func pAge(d time.Duration) pageCell { return pText(d.Truncate(time.Second).String()) }

// pageTableSpec is a table section before it is fitted to the caps.
type pageTableSpec struct {
	heading, empty string
	prose          []string
	columns        []string
	rows           [][]pageCell
}

func pageTable(heading, empty string, prose []string, columns []string, rows [][]pageCell) pageTableSpec {
	return pageTableSpec{heading: heading, empty: empty, prose: prose, columns: columns, rows: rows}
}

func listSection(heading, empty string, groups ...pageGroup) pageSection {
	if groups == nil {
		groups = []pageGroup{}
	}
	return pageSection{Kind: "definitionList", Heading: heading, Prose: []string{}, Empty: empty, Body: pageListBody{Groups: groups}}
}

// pageTables builds the sections of a page that the caps allow, sharing the
// row budget between the tables as the descriptor panels do. A section that
// lost rows says so.
func pageTables(title string, specs ...pageTableSpec) pageDoc {
	lens := make([]int, len(specs))
	for i, s := range specs {
		lens[i] = len(s.rows)
	}
	return fitToCaps(lens, func(limits []int) pageDoc {
		sections := make([]pageSection, 0, len(specs))
		for i, s := range specs {
			prose, empty := append([]string{}, s.prose...), s.empty
			if limits[i] < len(s.rows) {
				note := trimNote(limits[i], len(s.rows))
				prose = append(prose, note)
				empty = note
			}
			rows := s.rows[:limits[i]]
			if rows == nil {
				rows = [][]pageCell{}
			}
			sections = append(sections, pageSection{
				Kind: "table", Heading: s.heading, Prose: prose, Empty: empty,
				Body: pageTableBody{Columns: s.columns, Rows: rows},
			})
		}
		return pageDoc{Title: title, Sections: sections}
	})
}

// tablePage is the handler of a table page panel. The plane mounts it under
// /apps/<id>/ behind its own authentication and strips that prefix. It is
// read only: the plane lets every method through to a page, so the mux
// refuses all but GET. A failed read is a 500 with no detail; the cause is
// logged.
func (s *Server) tablePage(id string, data func(context.Context) (pageDoc, error)) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", pageFile(tablepage.Files, "index.html", "text/html; charset=utf-8"))
	mux.Handle("GET /table.js", pageFile(tablepage.Files, "table.js", "text/javascript; charset=utf-8"))
	mux.Handle("GET /table.css", pageFile(tablepage.Files, "table.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /data", func(w http.ResponseWriter, r *http.Request) {
		doc, err := data(r.Context())
		if err != nil {
			log.Printf("adminui: page %s: %v", id, err)
			http.Error(w, "data unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, doc)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", pageCSP)
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) controlFlowData(context.Context) (pageDoc, error) {
	snap := s.flow.Snapshot()
	state := listSection("Control flow", "", pageGroup{
		Heading: "Topics",
		Entries: []pageEntry{
			{Key: "Simulation output topic", Value: pText(simOutputTopicText(snap.OutputTopic))},
			{Key: "Control delta input topic", Value: pText(snap.InputTopic)},
		},
	}, pageGroup{
		Heading: "Counters",
		Entries: []pageEntry{
			{Key: "Applied", Value: pNumber(snap.Applied)},
			{Key: "Restated", Value: pNumber(snap.Restated)},
			{Key: "Skipped", Value: pNumber(snap.Skipped)},
			{Key: "Empty frames", Value: pNumber(snap.EmptyFrames)},
		},
	})

	// No delta yet is its own state, shown as the section's empty text
	// rather than as a delta with blank fields.
	var lastGroups []pageGroup
	if l := snap.Last; l != nil {
		lastGroups = append(lastGroups, pageGroup{Entries: []pageEntry{
			{Key: "Object", Value: pText(l.Object)},
			{Key: "Attribute", Value: pText(l.Attribute)},
			{Key: "Value", Value: pText(jsonText(l.Value))},
			{Key: "Applied at", Value: pTime(l.AppliedAt)},
		}})
	}
	last := listSection("Last applied delta", "No control delta has been applied yet.", lastGroups...)
	return pageDoc{Title: "Control flow", Sections: []pageSection{state, last}}, nil
}

// simOutputTopicText shows that no simulation id is configured instead of
// a blank cell, which reads as a fault.
func simOutputTopicText(topic string) string {
	if topic == "" {
		return "not subscribed (no simulation id configured)"
	}
	return topic
}

// derProgramsData lists every served DERProgram. It stays its own panel:
// the server's FSAs tab has no hook for an embedder's panel, so it cannot
// be folded in from here.
func (s *Server) derProgramsData(ctx context.Context) (pageDoc, error) {
	edevs, err := s.devices.EndDevices(ctx)
	if err != nil {
		return pageDoc{}, fmt.Errorf("reading end devices: %w", err)
	}
	programs, err := s.servedDERPrograms(ctx, edevs)
	if err != nil {
		return pageDoc{}, err
	}
	rows := make([][]pageCell, 0, len(programs))
	for _, p := range programs {
		defaultControl := pText("absent")
		if p.DefaultDERControlLink != "" {
			defaultControl = pText(p.DefaultDERControlLink)
		}
		rows = append(rows, []pageCell{
			pText(p.EndDeviceID), pText(p.ID), pText(p.MRID), pText(p.Description),
			pNumber(p.Primacy), pText(p.Href), defaultControl,
		})
	}
	return pageTables("DER programs", pageTable("DER programs", "No DERPrograms served.",
		[]string{"DERPrograms as served by the embedded server. Read only."},
		[]string{"EndDevice", "ID", "MRID", "Description", "Primacy", "Href", "DefaultDERControl"}, rows)), nil
}

// connectionsData is what the Devices tab cannot show: LFDIs that made
// requests but are not a served EndDevice, and every handshake attempt.
func (s *Server) connectionsData(ctx context.Context) (pageDoc, error) {
	snap := s.clientSnapshot()
	disabled := s.cfg.ObservationDisabled
	return pageTables("Connections", s.strangersSection(ctx, snap.Clients, disabled), handshakesSection(snap.Handshakes, disabled)), nil
}

// strangersSection lists the observed clients whose LFDI is not a served
// EndDevice. A roster read failure empties only this section: without the
// roster no client can be called a stranger, so it says so rather than
// listing every client.
func (s *Server) strangersSection(ctx context.Context, clients []connobs.ClientSnapshot, disabled bool) pageTableSpec {
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
		return pageTable(heading, empty, []string{fmt.Sprintf("Roster read failed: %v", err)}, columns, nil)
	}
	served := make(map[string]bool, len(edevs))
	for _, e := range edevs {
		served[e.LFDI] = true
	}

	rows := make([][]pageCell, 0)
	connectedCount := 0
	for _, c := range clients {
		if served[c.LFDI] {
			continue
		}
		if s.clientConnected(c) {
			connectedCount++
		}
		rows = append(rows, []pageCell{
			pText(c.LFDI), s.clientStatus(c), pTime(c.LastSeen), pAge(c.Age), pNumber(c.RequestCount), pText(strings.Join(c.Paths, ", ")),
		})
	}
	if disabled {
		prose = []string{"The connection observer is off (SEP2_ENABLE_CCM), so connected and idle counts are not available."}
	} else {
		prose = []string{fmt.Sprintf("%d connected, %d idle. A client is idle once unseen for %s; idle clients stay listed. Read only.",
			connectedCount, len(rows)-connectedCount, s.idleAfter)}
	}
	return pageTable(heading, empty, prose, columns, rows)
}

func handshakesSection(attempts []connobs.HandshakeAttempt, disabled bool) pageTableSpec {
	rows := make([][]pageCell, 0, len(attempts))
	for _, h := range attempts {
		result := pBadge(badgeError, "rejected")
		if h.Accepted {
			result = pBadge(badgeOK, "accepted")
		}
		known := pBadge(badgeNeutral, "unknown")
		if h.Known {
			known = pBadge(badgeOK, "known")
		}
		rows = append(rows, []pageCell{
			pText(h.LFDI), pOrDash(h.RemoteAddr), result, pOrDash(h.Reason), known, pTime(h.At),
		})
	}
	empty := "No handshake attempts recorded yet."
	if disabled {
		empty = "Connection observation is disabled (SEP2_ENABLE_CCM): this list cannot show handshake attempts."
	}
	return pageTable("Handshake attempts (cert validity)", empty, nil,
		[]string{"LFDI", "Remote address", "Result", "Reason", "Known", "At"}, rows)
}

func (s *Server) clientStatus(c connobs.ClientSnapshot) pageCell {
	if s.clientConnected(c) {
		return pBadge(badgeOK, "connected")
	}
	return pBadge(badgeWarn, "idle")
}
