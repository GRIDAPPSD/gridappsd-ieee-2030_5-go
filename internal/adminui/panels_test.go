package adminui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// The wire shape of a served Descriptor, decoded the way the shell reads
// it, so the tests assert what reaches the browser rather than the Go
// values a View built.
type wireCell struct {
	Kind     string `json:"kind"`
	Text     string `json:"text"`
	Badge    string `json:"badge"`
	DateTime string `json:"datetime"`
	Href     string `json:"href"`
}

type wireEntry struct {
	Key   string   `json:"key"`
	Value wireCell `json:"value"`
}

type wireGroup struct {
	Heading string      `json:"heading"`
	Entries []wireEntry `json:"entries"`
}

type wireBody struct {
	Columns []string     `json:"columns"`
	Rows    [][]wireCell `json:"rows"`
	Groups  []wireGroup  `json:"groups"`
}

type wireSection struct {
	Kind    string   `json:"kind"`
	Heading string   `json:"heading"`
	Prose   []string `json:"prose"`
	Empty   string   `json:"empty"`
	Body    wireBody `json:"body"`
}

type wireDescriptor struct {
	Version  int           `json:"version"`
	Sections []wireSection `json:"sections"`
}

// getPanel fetches a panel the way the shell does, through the full
// listener handler, and fails unless it answers 200.
func getPanel(t *testing.T, s *Server, id string) wireDescriptor {
	t.Helper()
	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels/"+id, "Bearer "+testKey, "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET panel %s: status %d, body %s", id, rec.Code, rec.Body.String())
	}
	var d wireDescriptor
	decodeJSON(t, rec.Body.Bytes(), &d)
	if d.Version != 2 {
		t.Fatalf("panel %s: version %d, want 2", id, d.Version)
	}
	return d
}

// section returns the section with heading, failing when it is absent.
func section(t *testing.T, d wireDescriptor, heading string) wireSection {
	t.Helper()
	for _, s := range d.Sections {
		if s.Heading == heading {
			return s
		}
	}
	t.Fatalf("no section %q in %+v", heading, d)
	return wireSection{}
}

// entries flattens a definition-list section into key -> cell.
func entries(t *testing.T, s wireSection) map[string]wireCell {
	t.Helper()
	if s.Kind != "definitionList" {
		t.Fatalf("section %q: kind %q, want definitionList", s.Heading, s.Kind)
	}
	out := map[string]wireCell{}
	for _, g := range s.Body.Groups {
		for _, e := range g.Entries {
			out[e.Key] = e.Value
		}
	}
	return out
}

// texts is a table row's cell texts.
func texts(row []wireCell) []string {
	out := make([]string, 0, len(row))
	for _, c := range row {
		out = append(out, c.Text)
	}
	return out
}

func assertColumns(t *testing.T, s wireSection, want ...string) {
	t.Helper()
	if s.Kind != "table" {
		t.Fatalf("section %q: kind %q, want table", s.Heading, s.Kind)
	}
	if !slices.Equal(s.Body.Columns, want) {
		t.Fatalf("section %q columns = %q, want %q", s.Heading, s.Body.Columns, want)
	}
}

func assertBadge(t *testing.T, where string, c wireCell, variant, text string) {
	t.Helper()
	if c.Kind != "badge" || c.Badge != variant || c.Text != text {
		t.Errorf("%s = %+v, want badge %q %q", where, c, variant, text)
	}
}

// TestUIListsTheSevenGridappsdTabs is the issue's first done-when line: the
// shell loads with the key, and its panel manifest lists the seven bridge
// tabs, in rank order, and nothing else. The core tabs are
// the shell's own and never appear in the manifest; the extension band
// always sorts after them.
func TestUIListsTheSevenGridappsdTabs(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	if rec := doRequest(t, s.Handler(), http.MethodGet, "/ui/", "Bearer "+testKey, "localhost"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<div id="app">`) {
		t.Fatalf("GET /ui/ with the key: %d %q", rec.Code, rec.Body.String())
	}

	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels", "Bearer "+testKey, "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/ui/panels: %d %s", rec.Code, rec.Body.String())
	}
	var manifest []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	decodeJSON(t, rec.Body.Bytes(), &manifest)
	want := []struct{ id, label string }{
		{"gridappsd-health", "Bridge health"},
		{"gridappsd-registry", "Registry"},
		{"gridappsd-ders", "Discovered DERs"},
		{"gridappsd-served", "Served resources"},
		{"gridappsd-clients", "Connected clients"},
		{"gridappsd-controlflow", "Control flow"},
		{"gridappsd-graph-input", "Graph: device status"},
	}
	if len(manifest) != len(want) {
		t.Fatalf("manifest = %+v, want %d panels", manifest, len(want))
	}
	for i, w := range want {
		if manifest[i].ID != w.id || manifest[i].Label != w.label {
			t.Errorf("manifest[%d] = %+v, want %s %q", i, manifest[i], w.id, w.label)
		}
	}
}

// TestRegistryPanelRowsMatchTheRegistryFieldForField is the issue's
// second done-when line: one row per registry entry, in order, carrying
// every registry field, with placeholder as its own badge.
func TestRegistryPanelRowsMatchTheRegistryFieldForField(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistry{entries: []registry.Entry{
		{MRID: "mrid-1", Name: "inverter-1", LFDI: "ABCDEF0123456789", SFDI: "123456789", Placeholder: false},
		{MRID: "mrid-2", Name: "battery-1", LFDI: "PLACEHOLDER0002", SFDI: "", Placeholder: true},
	}}
	src := testSources()
	src.Registry = reg
	s := newServer(t, Config{Key: testKey}, src)

	sec := section(t, getPanel(t, s, panelRegistry), "Registry map")
	assertColumns(t, sec, "mRID", "Name", "LFDI", "SFDI", "Identity")
	if len(sec.Body.Rows) != len(reg.entries) {
		t.Fatalf("rows = %d, want %d", len(sec.Body.Rows), len(reg.entries))
	}
	for i, e := range reg.entries {
		row := sec.Body.Rows[i]
		if got, want := texts(row[:4]), []string{e.MRID, e.Name, e.LFDI, e.SFDI}; !slices.Equal(got, want) {
			t.Errorf("row %d = %q, want %q", i, got, want)
		}
		if e.Placeholder {
			assertBadge(t, "row identity", row[4], "warn", "placeholder")
		} else {
			assertBadge(t, "row identity", row[4], "ok", "certificate")
		}
	}
	if sec.Empty != "No registry entries yet." {
		t.Errorf("empty text = %q", sec.Empty)
	}
}

// TestHealthPanelCarriesEveryHealthField: each /api/health field appears
// in the panel with the same value the JSON route serves.
func TestHealthPanelCarriesEveryHealthField(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Registry = &fakeRegistry{entries: []registry.Entry{{MRID: "a"}, {MRID: "b", Placeholder: true}, {MRID: "c"}}}
	src.Identity = &fakeIdentity{addr: "127.0.0.1:8443", identity: sep2srv.Identity{SFDI: "999888777", LFDI: "FEDCBA9876543210"}}
	src.Stomp = &fakeStomp{connected: true}
	s := newServer(t, Config{Key: testKey, FeederMRID: "feeder-1", SimulationID: "sim-1", SORLink: "https://sor.example/dash"}, src)

	got := entries(t, section(t, getPanel(t, s, panelHealth), "Bridge"))
	want := map[string]string{
		"mTLS listener":                  "127.0.0.1:8443",
		"Server SFDI":                    "999888777",
		"Server LFDI":                    "FEDCBA9876543210",
		"Feeder mRID":                    "feeder-1",
		"Simulation ID":                  "sim-1",
		"Registry entries":               "3",
		"Placeholder identities":         "1",
		"Certificate derived identities": "2",
	}
	for k, v := range want {
		if got[k].Text != v {
			t.Errorf("%s = %+v, want %q", k, got[k], v)
		}
	}
	assertBadge(t, "Status", got["Status"], "ok", "ok")
	assertBadge(t, "STOMP connection", got["STOMP connection"], "ok", "connected")
	if _, err := time.ParseDuration(got["Uptime (seconds)"].Text + "s"); err != nil {
		t.Errorf("Uptime (seconds) = %+v, want a whole number", got["Uptime (seconds)"])
	}
	if sor := got["Server of record"]; sor.Kind != "link" || sor.Href != "https://sor.example/dash" {
		t.Errorf("Server of record = %+v, want a link to the configured URL", sor)
	}
}

// TestHealthPanelShowsAnUnsafeSORLinkAsText: a link the descriptor would
// refuse must not fail the whole panel, and must not become a link.
func TestHealthPanelShowsAnUnsafeSORLinkAsText(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey, SORLink: "javascript:alert(1)"}, testSources())
	sor := entries(t, section(t, getPanel(t, s, panelHealth), "Bridge"))["Server of record"]
	if sor.Kind != "text" || sor.Text != "javascript:alert(1)" || sor.Href != "" {
		t.Errorf("Server of record = %+v, want the raw value as plain text", sor)
	}
}

// TestDERsPanelCarriesEachDERWithItsOwnerAndFeeder: one row per DER
// across every device, with the owning device and the configured feeder.
func TestDERsPanelCarriesEachDERWithItsOwnerAndFeeder(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Devices = &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{
		{ID: "edev-1", DERs: []sep2embed.DERSnapshot{{ID: "der-1", Href: "/h1"}, {ID: "der-2", Href: "/h2"}}},
		{ID: "edev-2", DERs: []sep2embed.DERSnapshot{{ID: "der-3", Href: "/h3"}}},
	}}
	s := newServer(t, Config{Key: testKey, FeederMRID: "feeder-mrid-1"}, src)

	sec := section(t, getPanel(t, s, panelDERs), "Discovered DERs")
	assertColumns(t, sec, "EndDevice", "DER ID", "Href", "Feeder mRID")
	want := [][]string{
		{"edev-1", "der-1", "/h1", "feeder-mrid-1"},
		{"edev-1", "der-2", "/h2", "feeder-mrid-1"},
		{"edev-2", "der-3", "/h3", "feeder-mrid-1"},
	}
	if len(sec.Body.Rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(sec.Body.Rows), len(want))
	}
	for i, w := range want {
		if got := texts(sec.Body.Rows[i]); !slices.Equal(got, w) {
			t.Errorf("row %d = %q, want %q", i, got, w)
		}
	}
}

// TestServedPanelCarriesEveryEndDeviceAndProgramField covers both
// tables, including Primacy (a number easily zeroed) and the
// DefaultDERControl link, present on one program and absent on another.
func TestServedPanelCarriesEveryEndDeviceAndProgramField(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Devices = &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{
		{ID: "edev-1", LFDI: "LFDI1", SFDI: "SFDI1", Href: "/edev/1", Enabled: true,
			DERs: []sep2embed.DERSnapshot{{ID: "der-1", Href: "/edev/1/der/1"}, {ID: "der-2", Href: "/edev/1/der/2"}}},
		{ID: "edev-2", LFDI: "LFDI2", SFDI: "SFDI2", Href: "/edev/2", Enabled: false},
	}}
	src.Programs = &fakePrograms{byEdevID: map[string][]sep2embed.DERProgramSnapshot{
		"edev-1": {{ID: "1", Href: "/edev/1/fsa/1/derp/1", MRID: "derp-mrid-1", Description: "default", Primacy: 0, DefaultDERControlLink: "/edev/1/fsa/1/derp/1/dderc"}},
		"edev-2": {{ID: "1", Href: "/edev/2/fsa/1/derp/1", MRID: "derp-mrid-2", Description: "critical peak", Primacy: 5}},
	}}
	s := newServer(t, Config{Key: testKey}, src)
	d := getPanel(t, s, panelServed)

	edevs := section(t, d, "EndDevices")
	assertColumns(t, edevs, "ID", "LFDI", "SFDI", "Href", "Enabled", "DERs", "DER IDs")
	wantEdevs := [][]string{
		{"edev-1", "LFDI1", "SFDI1", "/edev/1", "yes", "2", "der-1, der-2"},
		{"edev-2", "LFDI2", "SFDI2", "/edev/2", "no", "0", ""},
	}
	if len(edevs.Body.Rows) != len(wantEdevs) {
		t.Fatalf("EndDevice rows = %d, want %d", len(edevs.Body.Rows), len(wantEdevs))
	}
	for i, w := range wantEdevs {
		if got := texts(edevs.Body.Rows[i]); !slices.Equal(got, w) {
			t.Errorf("EndDevice row %d = %q, want %q", i, got, w)
		}
	}

	programs := section(t, d, "DER programs")
	assertColumns(t, programs, "EndDevice", "ID", "MRID", "Description", "Primacy", "Href", "DefaultDERControl")
	wantPrograms := [][]string{
		{"edev-1", "1", "derp-mrid-1", "default", "0", "/edev/1/fsa/1/derp/1", "/edev/1/fsa/1/derp/1/dderc"},
		{"edev-2", "1", "derp-mrid-2", "critical peak", "5", "/edev/2/fsa/1/derp/1", "absent"},
	}
	if len(programs.Body.Rows) != len(wantPrograms) {
		t.Fatalf("program rows = %d, want %d", len(programs.Body.Rows), len(wantPrograms))
	}
	for i, w := range wantPrograms {
		if got := texts(programs.Body.Rows[i]); !slices.Equal(got, w) {
			t.Errorf("program row %d = %q, want %q", i, got, w)
		}
	}
}

// TestServedPanelFailsWhenTheDeviceReadFails: a read error is a failed
// panel (500), never an empty table that reads as "nothing served".
func TestServedPanelFailsWhenTheDeviceReadFails(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Devices = &fakeEndDevices{err: errors.New("store down")}
	s := newServer(t, Config{Key: testKey}, src)
	for _, id := range []string{panelServed, panelDERs} {
		rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels/"+id, "Bearer "+testKey, "localhost")
		if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "store down") {
			t.Errorf("panel %s with a failing device read: %d %s, want 500 without the error text", id, rec.Code, rec.Body.String())
		}
	}
}

// TestControlFlowPanelCarriesEverySnapshotField covers the topics, both
// counters and every field of the last delta.
func TestControlFlowPanelCarriesEverySnapshotField(t *testing.T) {
	t.Parallel()

	appliedAt := time.Date(2026, 7, 18, 12, 30, 0, 0, time.UTC)
	src := testSources()
	src.Flow = &fakeFlow{snap: controlobs.Snapshot{
		Applied:     4,
		Restated:    3,
		Skipped:     1,
		EmptyFrames: 2,
		Last: &controlobs.LastDelta{
			Object:    "mrid-inv-1",
			Attribute: "DERControl.DERControlBase.opModTargetW",
			Value:     map[string]any{"multiplier": float64(0), "value": float64(4200)},
			AppliedAt: appliedAt,
		},
		OutputTopic: "/topic/goss.gridappsd.simulation.output.12345",
		InputTopic:  "/topic/goss.gridappsd.simulation.input.12345",
	}}
	s := newServer(t, Config{Key: testKey}, src)
	d := getPanel(t, s, panelControlFlow)

	state := entries(t, section(t, d, "Control flow"))
	for k, v := range map[string]string{
		"Simulation output topic":   "/topic/goss.gridappsd.simulation.output.12345",
		"Control delta input topic": "/topic/goss.gridappsd.simulation.input.12345",
		"Applied":                   "4",
		"Restated":                  "3",
		"Skipped":                   "1",
		"Empty frames":              "2",
	} {
		if state[k].Text != v {
			t.Errorf("%s = %+v, want %q", k, state[k], v)
		}
	}
	last := entries(t, section(t, d, "Last applied delta"))
	for k, v := range map[string]string{
		"Object":     "mrid-inv-1",
		"Attribute":  "DERControl.DERControlBase.opModTargetW",
		"Value":      `{"multiplier":0,"value":4200}`,
		"Applied at": "2026-07-18T12:30:00.000Z",
	} {
		if last[k].Text != v {
			t.Errorf("%s = %+v, want %q", k, last[k], v)
		}
	}
	if at := last["Applied at"]; at.Kind != "time" || at.DateTime != "2026-07-18T12:30:00Z" {
		t.Errorf("Applied at = %+v, want a time cell at 2026-07-18T12:30:00Z", at)
	}
}

// TestControlFlowPanelShowsNoDeltaAsItsOwnState: with no delta applied,
// the section is empty with its explanation, not a delta of blanks.
func TestControlFlowPanelShowsNoDeltaAsItsOwnState(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	last := section(t, getPanel(t, s, panelControlFlow), "Last applied delta")
	if len(last.Body.Groups) != 0 || last.Empty != "No control delta has been applied yet." {
		t.Errorf("last delta section = %+v, want no groups and the idle text", last)
	}
}

// TestClientsPanelCarriesClientsHandshakesAndServedStatus covers all three
// sections, including the served-vs-connected cross-reference by LFDI.
func TestClientsPanelCarriesClientsHandshakesAndServedStatus(t *testing.T) {
	t.Parallel()

	lastSeen := time.Date(2026, 7, 27, 9, 15, 30, 0, time.UTC)
	handshakeAt := time.Date(2026, 7, 27, 9, 14, 0, 0, time.UTC)
	src := testSources()
	src.Clients = &fakeClientObserver{snap: connobs.Snapshot{
		Clients: []connobs.ClientSnapshot{{LFDI: "LFDIA", LastSeen: lastSeen, Age: 3*time.Minute + 12*time.Second + 400*time.Millisecond, RequestCount: 7, Paths: []string{"/dcap", "/edev"}}},
		Handshakes: []connobs.HandshakeAttempt{
			{LFDI: "LFDIX", RemoteAddr: "10.0.0.5:54321", Accepted: false, Reason: "x509: unknown authority", Known: false, At: handshakeAt},
			{LFDI: "LFDIA", RemoteAddr: "", Accepted: true, Reason: "", Known: true, At: handshakeAt},
		},
	}}
	src.Devices = &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{
		{ID: "edev-a", LFDI: "LFDIA"},
		{ID: "edev-b", LFDI: "LFDIB"},
	}}
	s := newServer(t, Config{Key: testKey}, src)
	d := getPanel(t, s, panelClients)

	clients := section(t, d, "Connected clients")
	assertColumns(t, clients, "LFDI", "Status", "Last seen", "Age", "Requests", "Paths touched")
	if len(clients.Body.Rows) != 1 {
		t.Fatalf("client rows = %d, want 1", len(clients.Body.Rows))
	}
	if got := texts(clients.Body.Rows[0]); !slices.Equal(got, []string{"LFDIA", "connected", "2026-07-27T09:15:30.000Z", "3m12s", "7", "/dcap, /edev"}) {
		t.Errorf("client row = %q", got)
	}
	if c := clients.Body.Rows[0][2]; c.Kind != "time" || c.DateTime != "2026-07-27T09:15:30Z" {
		t.Errorf("Last seen = %+v, want a time cell", c)
	}

	served := section(t, d, "Served EndDevices: connection status")
	assertColumns(t, served, "EndDevice", "LFDI", "Status", "Last seen", "Age", "Requests")
	if len(served.Body.Rows) != 2 {
		t.Fatalf("served rows = %d, want 2", len(served.Body.Rows))
	}
	a, b := served.Body.Rows[0], served.Body.Rows[1]
	if got := texts(a); got[0] != "edev-a" || got[1] != "LFDIA" || got[3] != "2026-07-27T09:15:30.000Z" || got[4] != "3m12s" || got[5] != "7" {
		t.Errorf("served row a = %q", got)
	}
	assertBadge(t, "served a status", a[2], "ok", "connected")
	if got := texts(b); got[0] != "edev-b" || got[1] != "LFDIB" || got[3] != "-" || got[4] != "-" || got[5] != "-" {
		t.Errorf("served row b = %q", got)
	}
	assertBadge(t, "served b status", b[2], "warn", "never connected")

	hs := section(t, d, "Handshake attempts (cert validity)")
	assertColumns(t, hs, "LFDI", "Remote address", "Result", "Reason", "Known", "At")
	if len(hs.Body.Rows) != 2 {
		t.Fatalf("handshake rows = %d, want 2", len(hs.Body.Rows))
	}
	rejected, accepted := hs.Body.Rows[0], hs.Body.Rows[1]
	if got := texts(rejected); got[0] != "LFDIX" || got[1] != "10.0.0.5:54321" || got[3] != "x509: unknown authority" || got[5] != "2026-07-27T09:14:00.000Z" {
		t.Errorf("rejected handshake row = %q", got)
	}
	assertBadge(t, "rejected result", rejected[2], "error", "rejected")
	assertBadge(t, "rejected known", rejected[4], "neutral", "unknown")
	if got := texts(accepted); got[1] != "-" || got[3] != "-" {
		t.Errorf("accepted handshake row = %q, want - for the empty address and reason", got)
	}
	assertBadge(t, "accepted result", accepted[2], "ok", "accepted")
	assertBadge(t, "accepted known", accepted[4], "ok", "known")
}

// TestClientsPanelNeverClaimsNeverConnectedWithTheObserverOff: with the
// observer off, a device's status is unknown, not "never connected".
func TestClientsPanelNeverClaimsNeverConnectedWithTheObserverOff(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Devices = &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{{ID: "edev-a", LFDI: "LFDIA"}}}
	s := newServer(t, Config{Key: testKey, ObservationDisabled: true}, src)
	d := getPanel(t, s, panelClients)

	served := section(t, d, "Served EndDevices: connection status")
	assertBadge(t, "status", served.Body.Rows[0][2], "neutral", "unknown")
	if c := section(t, d, "Connected clients"); !strings.Contains(c.Empty, "SEP2_ENABLE_CCM") {
		t.Errorf("connected clients empty text = %q, want the observer-disabled explanation", c.Empty)
	}
}

// TestEveryRouteAnswers401WithoutACredential is the issue's last
// done-when line, over every route the listener mounts, from loopback:
// the bypass is off, so loopback is no exception. The two login routes
// are the only routes a caller with no credential can reach, by design,
// and neither of them hands out a session.
func TestEveryRouteAnswers401WithoutACredential(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	routes := append(slices.Clone(s.planePatterns), bridgeJSONRoutes...)
	if len(routes) < 10 {
		t.Fatalf("only %d routes: %q", len(routes), routes)
	}
	login := map[string]bool{"GET /login": true, "POST /auth/login": true}
	fill := strings.NewReplacer("{id}", "x", "{lfdi}", "x", "{edevId}", "x", "{frqId}", "x", "{$}", "")
	checked := 0
	for _, p := range routes {
		method, path, ok := strings.Cut(p, " ")
		if !ok {
			method, path = http.MethodGet, p
		}
		path = fill.Replace(path)
		if strings.Contains(path, "{") {
			t.Errorf("route %q has a wildcard this test does not fill", p)
			continue
		}
		rec := boundedRequest(t, s.Handler(), method, path)
		if login[p] {
			if c := rec.Header().Values("Set-Cookie"); len(c) != 0 {
				t.Errorf("%s with no credential set a cookie: %q", p, c)
			}
			continue
		}
		checked++
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no credential: %d, want 401", method, path, rec.Code)
		}
	}
	t.Logf("%d routes answered 401, %d login routes checked for no session", checked, len(login))
}

// TestPlaneMountsNoBridgeJSONRoute: the bridge's exact JSON patterns sit
// in front of the plane, so a plane route on the same path would be
// silently shadowed.
func TestPlaneMountsNoBridgeJSONRoute(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	for _, p := range s.planePatterns {
		_, path, _ := strings.Cut(p, " ")
		if slices.Contains(bridgeJSONRoutes, path) {
			t.Errorf("the plane mounts %q, which the bridge route shadows", p)
		}
	}
}

// TestNoResponseEverContainsTheAdminKey drives the bridge JSON routes and
// every bridge panel with populated sources and asserts the key never
// reaches a body.
func TestNoResponseEverContainsTheAdminKey(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Registry = &fakeRegistry{entries: []registry.Entry{{MRID: "mrid-1", Name: "inverter-1", LFDI: "LFDI1", SFDI: "SFDI1"}}}
	src.Devices = &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{{ID: "edev-1", LFDI: "LFDI1", DERs: []sep2embed.DERSnapshot{{ID: "der-1", Href: "/h1"}}}}}
	src.Programs = &fakePrograms{byEdevID: map[string][]sep2embed.DERProgramSnapshot{"edev-1": {{ID: "1", Href: "/h1", MRID: "derp-1"}}}}
	src.Flow = &fakeFlow{snap: controlobs.Snapshot{Applied: 1, Last: &controlobs.LastDelta{Object: "mrid-1", Value: 1.0, AppliedAt: time.Now()}}}
	s := newServer(t, Config{Key: testKey}, src)

	paths := slices.Clone(bridgeJSONRoutes)
	for _, p := range s.panels() {
		paths = append(paths, "/api/ui/panels/"+p.ID)
	}
	for _, path := range paths {
		rec := doRequest(t, s.Handler(), http.MethodGet, path, "Bearer "+testKey, "localhost")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: %d", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), testKey) {
			t.Errorf("GET %s body contains the admin key", path)
		}
	}
}

// boundedRequest is a loopback request with no credential that gives up
// after a second, so a stream (the dashboard's SSE) that wrongly admits
// it answers 200 instead of hanging the test.
func boundedRequest(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, method, path, nil)
	req.RemoteAddr = "127.0.0.1:40000"
	req.Host = "localhost"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestNoControlWriteRouteIsMounted: the bridge issues DER controls itself,
// so the plane's DER control and flow reservation writes stay unmounted.
// The store set here mounts the flow reservation reads, so their writes
// would appear if ControlWrites were on.
func TestNoControlWriteRouteIsMounted(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	if !slices.Contains(s.planePatterns, "GET /api/derms/flow-reservations") {
		t.Fatalf("flow reservation reads are not mounted, so this test cannot see their writes: %q", s.planePatterns)
	}
	for _, p := range s.planePatterns {
		method, path, _ := strings.Cut(p, " ")
		if method == http.MethodGet {
			continue
		}
		if strings.HasPrefix(path, "/api/der/controls") || strings.HasPrefix(path, "/api/derms/flow-reservations") {
			t.Errorf("control write route %q is mounted", p)
		}
	}
}

// TestListenerMountsNoWriteRoute: the bridge seeds and writes the stores
// itself, so every route on the listener reads, except the two auth POSTs
// that login and the SSE ticket need. A plane pattern with no method
// would accept every method, so it fails too; the bridge JSON routes are
// method-less but sit behind requireGET.
func TestListenerMountsNoWriteRoute(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	allowed := map[string]bool{"POST /auth/login": true, "POST /auth/ticket": true}
	if len(s.planePatterns) == 0 {
		t.Fatal("the plane reports no patterns, so this test checks nothing")
	}
	for _, p := range s.planePatterns {
		method, _, ok := strings.Cut(p, " ")
		switch {
		case allowed[p]:
		case !ok || strings.HasPrefix(method, "/"):
			t.Errorf("plane pattern %q has no method, so it admits writes", p)
		case method != http.MethodGet && method != http.MethodHead:
			t.Errorf("write route %q is mounted", p)
		}
	}
	for _, route := range bridgeJSONRoutes {
		if rec := doRequest(t, s.Handler(), http.MethodPost, route, "Bearer "+testKey, "localhost"); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s with the key: %d, want 405", route, rec.Code)
		}
	}
}

// manyEntries returns n registry entries, each name padded to pad bytes.
func manyEntries(n, pad int) []registry.Entry {
	out := make([]registry.Entry, n)
	for i := range out {
		out[i] = registry.Entry{MRID: fmt.Sprintf("mrid-%04d", i), Name: strings.Repeat("n", pad), LFDI: fmt.Sprintf("LFDI%04d", i)}
	}
	return out
}

// manyDevices returns n served EndDevices, each with one DERProgram.
func manyDevices(n int) (*fakeEndDevices, *fakePrograms) {
	devices := &fakeEndDevices{}
	programs := &fakePrograms{byEdevID: map[string][]sep2embed.DERProgramSnapshot{}}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("edev-%04d", i)
		devices.edevs = append(devices.edevs, sep2embed.EndDeviceSnapshot{ID: id, LFDI: "L" + id})
		programs.byEdevID[id] = []sep2embed.DERProgramSnapshot{{ID: "1", MRID: "derp-" + id}}
	}
	return devices, programs
}

// manyClients returns n connected clients.
func manyClients(n int) *fakeClientObserver {
	obs := &fakeClientObserver{}
	for i := 0; i < n; i++ {
		obs.snap.Clients = append(obs.snap.Clients, connobs.ClientSnapshot{LFDI: fmt.Sprintf("LFDI%04d", i), LastSeen: time.Unix(1700000000, 0), RequestCount: 1})
	}
	return obs
}

// notice reports whether a section says it shows only part of its rows.
func notice(s wireSection) string {
	for _, p := range s.Prose {
		if strings.HasPrefix(p, "Showing ") {
			return p
		}
	}
	return ""
}

// TestPanelsNeverFailOnRowCount: the plane refuses a Descriptor of more
// than 1000 rows in total, across its sections. At the cap every row is
// shown with no notice; one past it the panel still answers 200, showing
// the rows that fit and saying how many it left out.
func TestPanelsNeverFailOnRowCount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, panel string
		src         func(Sources) Sources
		want        map[string]string // section heading -> notice, "" for none
		rows        map[string]int
	}{
		{"registry at the cap", panelRegistry,
			func(s Sources) Sources { s.Registry = &fakeRegistry{entries: manyEntries(1000, 1)}; return s },
			map[string]string{"Registry map": ""}, map[string]int{"Registry map": 1000}},
		{"registry past the cap", panelRegistry,
			func(s Sources) Sources { s.Registry = &fakeRegistry{entries: manyEntries(1001, 1)}; return s },
			map[string]string{"Registry map": "Showing 1000 of 1001 rows"}, map[string]int{"Registry map": 1000}},
		{"served at the cap", panelServed,
			func(s Sources) Sources { s.Devices, s.Programs = manyDevices(500); return s },
			map[string]string{"EndDevices": "", "DER programs": ""}, map[string]int{"EndDevices": 500, "DER programs": 500}},
		{"served past the cap", panelServed,
			func(s Sources) Sources { s.Devices, s.Programs = manyDevices(501); return s },
			map[string]string{"EndDevices": "Showing 500 of 501 rows", "DER programs": "Showing 500 of 501 rows"},
			map[string]int{"EndDevices": 500, "DER programs": 500}},
		{"clients at the cap", panelClients,
			func(s Sources) Sources { s.Clients = manyClients(1000); return s },
			map[string]string{"Connected clients": ""}, map[string]int{"Connected clients": 1000}},
		{"clients past the cap", panelClients,
			func(s Sources) Sources { s.Clients = manyClients(1001); return s },
			map[string]string{"Connected clients": "Showing 1000 of 1001 rows"}, map[string]int{"Connected clients": 1000}},
		{"ders past the cap", panelDERs,
			func(s Sources) Sources {
				d := &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{{ID: "edev-1"}}}
				for i := 0; i < 1001; i++ {
					d.edevs[0].DERs = append(d.edevs[0].DERs, sep2embed.DERSnapshot{ID: fmt.Sprint(i)})
				}
				s.Devices = d
				return s
			},
			map[string]string{"Discovered DERs": "Showing 1000 of 1001 rows"}, map[string]int{"Discovered DERs": 1000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newServer(t, Config{Key: testKey}, tc.src(testSources()))
			d := getPanel(t, s, tc.panel)
			for heading, want := range tc.want {
				sec := section(t, d, heading)
				if got := notice(sec); !strings.HasPrefix(got, want) || (want == "") != (got == "") {
					t.Errorf("%s notice = %q, want %q", heading, got, want)
				}
				if got := len(sec.Body.Rows); got != tc.rows[heading] {
					t.Errorf("%s rows = %d, want %d", heading, got, tc.rows[heading])
				}
			}
		})
	}
}

// TestPanelsNeverFailOnSize: the plane also refuses a Descriptor that
// encodes past 1 MiB. Under the row cap but over the size cap, the panel
// sheds rows until it fits and says so.
func TestPanelsNeverFailOnSize(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Registry = &fakeRegistry{entries: manyEntries(900, 2000)}
	s := newServer(t, Config{Key: testKey}, src)
	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels/"+panelRegistry, "Bearer "+testKey, "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("registry of 900 large rows: %d %s", rec.Code, rec.Body.String())
	}
	if n := rec.Body.Len(); n > 1<<20 {
		t.Errorf("body is %d bytes, over 1 MiB", n)
	}
	var d wireDescriptor
	decodeJSON(t, rec.Body.Bytes(), &d)
	sec := section(t, d, "Registry map")
	if got, want := notice(sec), fmt.Sprintf("Showing %d of 900 rows", len(sec.Body.Rows)); !strings.HasPrefix(got, want) || len(sec.Body.Rows) == 0 || len(sec.Body.Rows) >= 900 {
		t.Errorf("notice %q with %d rows, want %q and some but not all rows", got, len(sec.Body.Rows), want)
	}
}

// TestServedPanelFailsWhenAProgramReadFails: an unreadable DERProgram
// list fails the panel rather than reading "No DERPrograms served".
func TestServedPanelFailsWhenAProgramReadFails(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Devices = &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{{ID: "edev-1"}}}
	src.Programs = &fakePrograms{err: errors.New("program store down")}
	s := newServer(t, Config{Key: testKey}, src)
	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels/"+panelServed, "Bearer "+testKey, "localhost")
	if body := rec.Body.String(); rec.Code != http.StatusInternalServerError || strings.Contains(body, "No DERPrograms served") || strings.Contains(body, "program store down") {
		t.Errorf("served panel with a failing program read: %d %s, want 500 without the error text", rec.Code, body)
	}
}

// TestClientsPanelSaysWhenTheRosterIsUnreadable: a failed roster read
// empties only the served-status section, and says the roster is
// unavailable rather than "No EndDevices served". The other sections
// still show the client snapshot.
func TestClientsPanelSaysWhenTheRosterIsUnreadable(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Devices = &fakeEndDevices{err: errors.New("store down")}
	src.Clients = manyClients(1)
	s := newServer(t, Config{Key: testKey}, src)
	d := getPanel(t, s, panelClients)

	served := section(t, d, "Served EndDevices: connection status")
	if len(served.Body.Rows) != 0 || served.Empty != "Served EndDevice roster unavailable." {
		t.Errorf("served status section = %+v, want no rows and the roster-unavailable text", served)
	}
	if got := len(section(t, d, "Connected clients").Body.Rows); got != 1 {
		t.Errorf("connected clients rows = %d, want 1", got)
	}
}

// TestServedPanelSharesTheBudgetBetweenUnequalSections: one device with
// 1001 programs. The EndDevices section needs one row and gets it; the
// programs section gets the 999 left, not an equal half and not a budget
// that forgot the row already given.
func TestServedPanelSharesTheBudgetBetweenUnequalSections(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Devices = &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{{ID: "edev-1"}}}
	programs := make([]sep2embed.DERProgramSnapshot, 1001)
	for i := range programs {
		programs[i] = sep2embed.DERProgramSnapshot{ID: fmt.Sprint(i), MRID: fmt.Sprintf("derp-%04d", i)}
	}
	src.Programs = &fakePrograms{byEdevID: map[string][]sep2embed.DERProgramSnapshot{"edev-1": programs}}
	s := newServer(t, Config{Key: testKey}, src)
	d := getPanel(t, s, panelServed)

	edevs := section(t, d, "EndDevices")
	if len(edevs.Body.Rows) != 1 || notice(edevs) != "" {
		t.Errorf("EndDevices: %d rows, notice %q; want 1 row and no notice", len(edevs.Body.Rows), notice(edevs))
	}
	progs := section(t, d, "DER programs")
	if len(progs.Body.Rows) != 999 || !strings.HasPrefix(notice(progs), "Showing 999 of 1001 rows") {
		t.Errorf("DER programs: %d rows, notice %q; want 999 rows and \"Showing 999 of 1001 rows\"", len(progs.Body.Rows), notice(progs))
	}
}

// TestASectionTrimmedToNothingSaysWhy: a single row too large for the
// byte cap leaves its section empty, and the empty text is the notice,
// not "No registry entries yet."
func TestASectionTrimmedToNothingSaysWhy(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Registry = &fakeRegistry{entries: manyEntries(1, 1200*1024)}
	s := newServer(t, Config{Key: testKey}, src)
	sec := section(t, getPanel(t, s, panelRegistry), "Registry map")
	if len(sec.Body.Rows) != 0 || !strings.HasPrefix(sec.Empty, "Showing 0 of 1 rows") {
		t.Errorf("registry section: %d rows, empty text %q; want 0 rows and \"Showing 0 of 1 rows\"", len(sec.Body.Rows), sec.Empty)
	}
}

func TestControlFlowPanelSaysNoSimulationIDInsteadOfBlank(t *testing.T) {
	src := testSources()
	src.Flow = &fakeFlow{snap: controlobs.Snapshot{
		InputTopic: "/topic/goss.gridappsd.IEEE_2030_5.input",
	}}
	s := newServer(t, Config{Key: testKey}, src)
	d := getPanel(t, s, panelControlFlow)

	state := entries(t, section(t, d, "Control flow"))
	const want = "not subscribed (no simulation id configured)"
	if got := state["Simulation output topic"].Text; got != want {
		t.Errorf("Simulation output topic = %q, want %q", got, want)
	}
	if got := state["Control delta input topic"].Text; got != "/topic/goss.gridappsd.IEEE_2030_5.input" {
		t.Errorf("Control delta input topic = %q", got)
	}
}

// TestClientsPanelShowsIdleClientsAndCountsOnlyConnected: with a 5 minute
// threshold, a client 1s inside it reads connected and is counted, one
// exactly at it and one 1s past it read idle and are not, a served device
// never seen stays "never connected", and every client row is kept.
func TestClientsPanelShowsIdleClientsAndCountsOnlyConnected(t *testing.T) {
	t.Parallel()

	const idleAfter = 5 * time.Minute
	lastSeen := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	src := testSources()
	src.Clients = &fakeClientObserver{snap: connobs.Snapshot{Clients: []connobs.ClientSnapshot{
		{LFDI: "LFDIA", LastSeen: lastSeen, Age: idleAfter - time.Second, RequestCount: 1},
		{LFDI: "LFDIB", LastSeen: lastSeen, Age: idleAfter, RequestCount: 1},
		{LFDI: "LFDIC", LastSeen: lastSeen, Age: idleAfter + time.Second, RequestCount: 1},
	}}}
	src.Devices = &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{
		{ID: "edev-a", LFDI: "LFDIA"},
		{ID: "edev-b", LFDI: "LFDIB"},
		{ID: "edev-c", LFDI: "LFDIC"},
		{ID: "edev-d", LFDI: "LFDID"},
	}}
	s := newServer(t, Config{Key: testKey, ClientIdleAfter: idleAfter}, src)
	d := getPanel(t, s, panelClients)

	clients := section(t, d, "Connected clients")
	if len(clients.Body.Rows) != 3 {
		t.Fatalf("client rows = %d, want all 3 kept", len(clients.Body.Rows))
	}
	wantStatus := []struct{ variant, text, age string }{
		{"ok", "connected", "4m59s"}, {"warn", "idle", "5m0s"}, {"warn", "idle", "5m1s"},
	}
	for i, w := range wantStatus {
		row := clients.Body.Rows[i]
		assertBadge(t, "client status "+row[0].Text, row[1], w.variant, w.text)
		if row[3].Text != w.age {
			t.Errorf("client %s age = %q, want %q", row[0].Text, row[3].Text, w.age)
		}
	}
	if len(clients.Prose) == 0 || !strings.HasPrefix(clients.Prose[0], "1 connected, 2 idle.") {
		t.Errorf("clients prose = %q, want it to open with \"1 connected, 2 idle.\"", clients.Prose)
	}

	served := section(t, d, "Served EndDevices: connection status")
	wantServed := []struct{ variant, text string }{
		{"ok", "connected"}, {"warn", "idle"}, {"warn", "idle"}, {"warn", "never connected"},
	}
	if len(served.Body.Rows) != len(wantServed) {
		t.Fatalf("served rows = %d, want %d", len(served.Body.Rows), len(wantServed))
	}
	for i, w := range wantServed {
		assertBadge(t, "served status "+served.Body.Rows[i][0].Text, served.Body.Rows[i][2], w.variant, w.text)
	}
}
