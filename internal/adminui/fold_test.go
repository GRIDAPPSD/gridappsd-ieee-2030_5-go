package adminui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

const (
	lfdiPV  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	lfdiBat = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	lfdiNew = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
)

// TestPanelListAfterTheFold asserts the exact panel manifest: the folded
// panels are gone, the Connections and DER programs panels are present,
// and a request for a folded id is a 404 rather than a stale view.
func TestPanelListAfterTheFold(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
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
		{"gridappsd-connections", "Connections"},
		{"gridappsd-derprograms", "DER programs"},
		{"gridappsd-controlflow", "Control flow"},
		{"gridappsd-graph-input", "Graph: device status"},
	}
	if len(manifest) != len(want) {
		t.Fatalf("manifest = %+v, want exactly %d panels", manifest, len(want))
	}
	for i, w := range want {
		if manifest[i].ID != w.id || manifest[i].Label != w.label {
			t.Errorf("manifest[%d] = %+v, want %s %q", i, manifest[i], w.id, w.label)
		}
	}
	for _, folded := range []string{"gridappsd-registry", "gridappsd-ders", "gridappsd-served", "gridappsd-clients"} {
		for _, m := range manifest {
			if m.ID == folded {
				t.Errorf("folded panel %s is still listed", folded)
			}
		}
		if rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels/"+folded, "Bearer "+testKey, "localhost"); rec.Code != http.StatusNotFound {
			t.Errorf("GET panel %s = %d, want 404", folded, rec.Code)
		}
	}
}

// TestPanelListKeepsTheOptionalPanels: the bus monitor and the sender
// panels still follow the fixed ones.
func TestPanelListKeepsTheOptionalPanels(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Sender = newTestSender(t, &recordingBus{}, false)
	s := newServer(t, Config{Key: testKey}, src)
	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels", "Bearer "+testKey, "localhost")
	var manifest []struct {
		ID string `json:"id"`
	}
	decodeJSON(t, rec.Body.Bytes(), &manifest)
	if len(manifest) <= 5 {
		t.Fatalf("manifest = %+v, want the five fixed panels plus the sender's", manifest)
	}
	for i, id := range []string{"gridappsd-health", "gridappsd-connections", "gridappsd-derprograms", "gridappsd-controlflow", "gridappsd-graph-input"} {
		if manifest[i].ID != id {
			t.Errorf("manifest[%d] = %s, want %s", i, manifest[i].ID, id)
		}
	}
}

// foldSources is a registry of one certificate device, one placeholder
// device and the served EndDevices for them plus one never registered.
func foldSources(t *testing.T) Sources {
	t.Helper()
	proto := newFakeProtocol()
	for id, lfdi := range map[string]string{"1": lfdiPV, "2": lfdiBat, "3": lfdiNew} {
		if err := proto.stores.EndDevices.Create(context.Background(), id, sep2.EndDevice{SFDI: "s" + id, LFDI: lfdi}); err != nil {
			t.Fatal(err)
		}
	}
	src := testSources()
	src.Protocol = proto
	src.Registry = &fakeRegistry{entries: []registry.Entry{
		{MRID: "_AAAA-0001", Name: "pv-1", LFDI: lfdiPV, SFDI: "111"},
		{MRID: "_BBBB-0002", Name: "bat-2", LFDI: lfdiBat, SFDI: "222", Placeholder: true},
		{MRID: "_DDDD-0004", LFDI: "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD", Placeholder: true},
	}}
	src.Devices = &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{
		{ID: "edev-1", LFDI: lfdiPV, DERs: []sep2embed.DERSnapshot{{ID: "der-1"}, {ID: "der-2"}}},
		{ID: "edev-2", LFDI: lfdiBat},
		{ID: "edev-3", LFDI: lfdiNew, DERs: []sep2embed.DERSnapshot{{ID: "der-9"}}},
	}}
	return src
}

// TestDeviceColumnsHeadersAndCells reads the Devices payload as the shell
// does and checks the column ids, their headers, and each cell by LFDI.
func TestDeviceColumnsHeadersAndCells(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, foldSources(t))
	p := devicesColumns(t, s)
	if p.Error != "" {
		t.Errorf("payload error = %q, want none", p.Error)
	}
	gotCols := make([]string, 0, len(p.Columns))
	for _, c := range p.Columns {
		gotCols = append(gotCols, c.ID+"="+c.Label)
		if c.Error != "" {
			t.Errorf("column %s error = %q, want none", c.ID, c.Error)
		}
	}
	if want := []string{"name=Name / mRID", "identity=Identity", "ders=DERs"}; !slices.Equal(gotCols, want) {
		t.Fatalf("columns = %q, want %q", gotCols, want)
	}
	cells := map[string]map[string]string{}
	for _, d := range p.Devices {
		cells[d.LFDI] = d.Cells
	}
	want := map[string]map[string]string{
		lfdiPV:  {"name": "pv-1 / _AAAA-0001", "identity": "certificate", "ders": "2 (der-1, der-2)"},
		lfdiBat: {"name": "bat-2 / _BBBB-0002", "identity": "placeholder", "ders": "0"},
		// Served but not in the registry: no name or identity to show, but
		// its DER is real.
		lfdiNew: {"name": "-", "identity": "-", "ders": "1 (der-9)"},
	}
	if len(cells) != len(want) {
		t.Fatalf("devices = %d, want %d: %+v", len(cells), len(want), cells)
	}
	for lfdi, w := range want {
		for col, v := range w {
			if got := cells[lfdi][col]; got != v {
				t.Errorf("%s %s = %q, want %q", lfdi[:2], col, got, v)
			}
		}
	}
}

// TestDeviceColumnsNameFallsBackToTheMRID: an entry with no name shows its
// mRID alone, not "/ mRID" or a blank.
func TestDeviceColumnsNameFallsBackToTheMRID(t *testing.T) {
	t.Parallel()

	src := foldSources(t)
	src.Registry = &fakeRegistry{entries: []registry.Entry{{MRID: "_AAAA-0001", LFDI: lfdiPV}}}
	s := newServer(t, Config{Key: testKey}, src)
	for _, d := range devicesColumns(t, s).Devices {
		if d.LFDI == lfdiPV && d.Cells["name"] != "_AAAA-0001" {
			t.Errorf("name = %q, want the bare mRID", d.Cells["name"])
		}
	}
}

// TestDeviceColumnsFailureKeepsEveryRow: a roster read error discards the
// cells, keeps all three rows with "-", and names the error on the column.
func TestDeviceColumnsFailureKeepsEveryRow(t *testing.T) {
	t.Parallel()

	src := foldSources(t)
	src.Devices = &fakeEndDevices{err: errors.New("store down")}
	s := newServer(t, Config{Key: testKey}, src)
	p := devicesColumns(t, s)
	if len(p.Devices) != 3 {
		t.Fatalf("devices = %d, want all 3 rows kept", len(p.Devices))
	}
	for _, d := range p.Devices {
		for _, col := range []string{"name", "identity", "ders"} {
			if d.Cells[col] != "-" {
				t.Errorf("%s %s = %q, want -", d.LFDI[:2], col, d.Cells[col])
			}
		}
	}
	if len(p.Columns) == 0 || p.Columns[0].Error == "" {
		t.Errorf("columns = %+v, want the error shown on the column", p.Columns)
	}
}

// TestDeviceColumnIDsMeetTheServersPattern: the server refuses an id
// outside ^[a-z0-9_-]{1,64}$ and drops the column, so assert the source's
// own ids against it.
func TestDeviceColumnIDsMeetTheServersPattern(t *testing.T) {
	t.Parallel()

	src := &deviceColumns{registry: &fakeRegistry{}, devices: &fakeEndDevices{}}
	seen := map[string]bool{}
	for _, c := range src.Columns() {
		if len(c.ID) == 0 || len(c.ID) > 64 || strings.Trim(c.ID, "abcdefghijklmnopqrstuvwxyz0123456789_-") != "" || seen[c.ID] {
			t.Errorf("column id %q is invalid or repeated", c.ID)
		}
		seen[c.ID] = true
	}
}

// TestConnectionsPanelShowsOnlyLFDIsWithNoEndDevice: a client whose LFDI
// is a served EndDevice belongs on the Devices tab, so only the stranger
// is listed here, with the handshake log beside it.
func TestConnectionsPanelShowsOnlyLFDIsWithNoEndDevice(t *testing.T) {
	t.Parallel()

	lastSeen := time.Date(2026, 7, 27, 9, 15, 30, 0, time.UTC)
	handshakeAt := time.Date(2026, 7, 27, 9, 14, 0, 0, time.UTC)
	src := testSources()
	src.Clients = &fakeClientObserver{snap: connobs.Snapshot{
		Clients: []connobs.ClientSnapshot{
			{LFDI: "LFDIA", LastSeen: lastSeen, Age: time.Minute, RequestCount: 4, Paths: []string{"/dcap"}},
			{LFDI: "LFDIX", LastSeen: lastSeen, Age: 3*time.Minute + 12*time.Second + 400*time.Millisecond, RequestCount: 7, Paths: []string{"/dcap", "/edev"}},
		},
		Handshakes: []connobs.HandshakeAttempt{
			{LFDI: "LFDIX", RemoteAddr: "10.0.0.5:54321", Accepted: false, Reason: "x509: unknown authority", Known: false, At: handshakeAt},
			{LFDI: "LFDIA", RemoteAddr: "", Accepted: true, Reason: "", Known: true, At: handshakeAt},
		},
	}}
	src.Devices = &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{{ID: "edev-a", LFDI: "LFDIA"}}}
	s := newServer(t, Config{Key: testKey}, src)
	d := getPanel(t, s, "gridappsd-connections")

	if len(d.Sections) != 2 {
		t.Fatalf("sections = %d, want 2", len(d.Sections))
	}
	seen := section(t, d, "LFDIs seen with no EndDevice")
	assertColumns(t, seen, "LFDI", "Status", "Last seen", "Age", "Requests", "Paths touched")
	if len(seen.Body.Rows) != 1 {
		t.Fatalf("rows = %d, want only the LFDI with no EndDevice", len(seen.Body.Rows))
	}
	if got := texts(seen.Body.Rows[0]); !slices.Equal(got, []string{"LFDIX", "connected", "2026-07-27T09:15:30.000Z", "3m12s", "7", "/dcap, /edev"}) {
		t.Errorf("row = %q", got)
	}
	if c := seen.Body.Rows[0][2]; c.Kind != "time" || c.DateTime != "2026-07-27T09:15:30Z" {
		t.Errorf("Last seen = %+v, want a time cell", c)
	}

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

// TestConnectionsPanelCountsOnlyConnected: with a 5 minute threshold, a
// stranger 1s inside it reads connected and is counted, one exactly at it
// and one 1s past it read idle and are not, and every row is kept.
func TestConnectionsPanelCountsOnlyConnected(t *testing.T) {
	t.Parallel()

	const idleAfter = 5 * time.Minute
	lastSeen := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	src := testSources()
	src.Clients = &fakeClientObserver{snap: connobs.Snapshot{Clients: []connobs.ClientSnapshot{
		{LFDI: "LFDIA", LastSeen: lastSeen, Age: idleAfter - time.Second, RequestCount: 1},
		{LFDI: "LFDIB", LastSeen: lastSeen, Age: idleAfter, RequestCount: 1},
		{LFDI: "LFDIC", LastSeen: lastSeen, Age: idleAfter + time.Second, RequestCount: 1},
	}}}
	s := newServer(t, Config{Key: testKey, ClientIdleAfter: idleAfter}, src)
	seen := section(t, getPanel(t, s, "gridappsd-connections"), "LFDIs seen with no EndDevice")
	if len(seen.Body.Rows) != 3 {
		t.Fatalf("rows = %d, want all 3 kept", len(seen.Body.Rows))
	}
	for i, w := range []struct{ variant, text, age string }{
		{"ok", "connected", "4m59s"}, {"warn", "idle", "5m0s"}, {"warn", "idle", "5m1s"},
	} {
		row := seen.Body.Rows[i]
		assertBadge(t, "status "+row[0].Text, row[1], w.variant, w.text)
		if row[3].Text != w.age {
			t.Errorf("%s age = %q, want %q", row[0].Text, row[3].Text, w.age)
		}
	}
	if len(seen.Prose) == 0 || !strings.HasPrefix(seen.Prose[0], "1 connected, 2 idle.") {
		t.Errorf("prose = %q, want it to open with \"1 connected, 2 idle.\"", seen.Prose)
	}
}

// TestConnectionsPanelSaysWhenTheObserverIsOff: with the observer off the
// empty texts say so instead of reading as "nothing connected".
func TestConnectionsPanelSaysWhenTheObserverIsOff(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey, ObservationDisabled: true}, testSources())
	d := getPanel(t, s, "gridappsd-connections")
	for _, h := range []string{"LFDIs seen with no EndDevice", "Handshake attempts (cert validity)"} {
		if e := section(t, d, h).Empty; !strings.Contains(e, "SEP2_ENABLE_CCM") {
			t.Errorf("%s empty text = %q, want the observer-disabled explanation", h, e)
		}
	}
}

// TestConnectionsPanelSaysWhenTheRosterIsUnreadable: without the roster
// nobody can say which LFDIs have no EndDevice, so that section says so
// instead of listing every client; the handshakes still show.
func TestConnectionsPanelSaysWhenTheRosterIsUnreadable(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Devices = &fakeEndDevices{err: errors.New("store down")}
	src.Clients = &fakeClientObserver{snap: connobs.Snapshot{
		Clients:    []connobs.ClientSnapshot{{LFDI: "LFDIA", LastSeen: time.Unix(1700000000, 0)}},
		Handshakes: []connobs.HandshakeAttempt{{LFDI: "LFDIA", At: time.Unix(1700000000, 0)}},
	}}
	s := newServer(t, Config{Key: testKey}, src)
	d := getPanel(t, s, "gridappsd-connections")
	seen := section(t, d, "LFDIs seen with no EndDevice")
	if len(seen.Body.Rows) != 0 || seen.Empty != "Served EndDevice roster unavailable." {
		t.Errorf("section = %+v, want no rows and the roster-unavailable text", seen)
	}
	if got := len(section(t, d, "Handshake attempts (cert validity)").Body.Rows); got != 1 {
		t.Errorf("handshake rows = %d, want 1", got)
	}
}

// TestDERProgramsPanelCarriesEveryProgramField covers Primacy (a number
// easily zeroed) and the DefaultDERControl link, present on one program
// and absent on another.
func TestDERProgramsPanelCarriesEveryProgramField(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Devices = &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{{ID: "edev-1"}, {ID: "edev-2"}}}
	src.Programs = &fakePrograms{byEdevID: map[string][]sep2embed.DERProgramSnapshot{
		"edev-1": {{ID: "1", Href: "/edev/1/fsa/1/derp/1", MRID: "derp-mrid-1", Description: "default", Primacy: 0, DefaultDERControlLink: "/edev/1/fsa/1/derp/1/dderc"}},
		"edev-2": {{ID: "1", Href: "/edev/2/fsa/1/derp/1", MRID: "derp-mrid-2", Description: "critical peak", Primacy: 5}},
	}}
	s := newServer(t, Config{Key: testKey}, src)
	d := getPanel(t, s, "gridappsd-derprograms")
	if len(d.Sections) != 1 {
		t.Fatalf("sections = %d, want only the programs table", len(d.Sections))
	}
	programs := section(t, d, "DER programs")
	assertColumns(t, programs, "EndDevice", "ID", "MRID", "Description", "Primacy", "Href", "DefaultDERControl")
	want := [][]string{
		{"edev-1", "1", "derp-mrid-1", "default", "0", "/edev/1/fsa/1/derp/1", "/edev/1/fsa/1/derp/1/dderc"},
		{"edev-2", "1", "derp-mrid-2", "critical peak", "5", "/edev/2/fsa/1/derp/1", "absent"},
	}
	if len(programs.Body.Rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(programs.Body.Rows), len(want))
	}
	for i, w := range want {
		if got := texts(programs.Body.Rows[i]); !slices.Equal(got, w) {
			t.Errorf("row %d = %q, want %q", i, got, w)
		}
	}
}

// TestDERProgramsPanelFailsOnAReadError: an unreadable roster or program
// list is a failed panel (500), never an empty table reading as "none".
func TestDERProgramsPanelFailsOnAReadError(t *testing.T) {
	t.Parallel()

	for name, mod := range map[string]func(*Sources){
		"roster": func(s *Sources) { s.Devices = &fakeEndDevices{err: errors.New("store down")} },
		"programs": func(s *Sources) {
			s.Devices = &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{{ID: "edev-1"}}}
			s.Programs = &fakePrograms{err: errors.New("store down")}
		},
	} {
		src := testSources()
		mod(&src)
		s := newServer(t, Config{Key: testKey}, src)
		rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels/gridappsd-derprograms", "Bearer "+testKey, "localhost")
		body := rec.Body.String()
		if rec.Code != http.StatusInternalServerError || strings.Contains(body, "store down") || strings.Contains(body, "No DERPrograms served") {
			t.Errorf("%s read failure: %d %s, want 500 without the error text", name, rec.Code, body)
		}
	}
}

// TestFoldedPanelsNeverFailOnRowCount: the plane refuses more than 1000
// rows in a Descriptor. At the cap every row shows with no notice; one
// past it the panel answers 200 and says how many it left out.
func TestFoldedPanelsNeverFailOnRowCount(t *testing.T) {
	t.Parallel()

	programs := func(n int) Sources {
		src := testSources()
		src.Devices = &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{{ID: "edev-1"}}}
		list := make([]sep2embed.DERProgramSnapshot, n)
		for i := range list {
			list[i] = sep2embed.DERProgramSnapshot{ID: fmt.Sprint(i), MRID: fmt.Sprintf("derp-%04d", i)}
		}
		src.Programs = &fakePrograms{byEdevID: map[string][]sep2embed.DERProgramSnapshot{"edev-1": list}}
		return src
	}
	strangers := func(n int) Sources {
		src := testSources()
		src.Clients = manyClients(n)
		return src
	}
	for _, tc := range []struct {
		name, panel, heading string
		src                  Sources
		rows                 int
		notice               string
	}{
		{"programs at the cap", "gridappsd-derprograms", "DER programs", programs(1000), 1000, ""},
		{"programs past the cap", "gridappsd-derprograms", "DER programs", programs(1001), 1000, "Showing 1000 of 1001 rows"},
		{"strangers at the cap", "gridappsd-connections", "LFDIs seen with no EndDevice", strangers(1000), 1000, ""},
		{"strangers past the cap", "gridappsd-connections", "LFDIs seen with no EndDevice", strangers(1001), 1000, "Showing 1000 of 1001 rows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newServer(t, Config{Key: testKey}, tc.src)
			sec := section(t, getPanel(t, s, tc.panel), tc.heading)
			if got := notice(sec); !strings.HasPrefix(got, tc.notice) || (tc.notice == "") != (got == "") {
				t.Errorf("notice = %q, want %q", got, tc.notice)
			}
			if len(sec.Body.Rows) != tc.rows {
				t.Errorf("rows = %d, want %d", len(sec.Body.Rows), tc.rows)
			}
		})
	}
}

// TestFoldedPanelTrimmedBySizeSaysWhy: rows under the row cap but over the
// 1 MiB cap are shed with a notice, and a single oversized row leaves the
// section empty with the notice as its empty text.
func TestFoldedPanelTrimmedBySizeSaysWhy(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Clients = &fakeClientObserver{snap: connobs.Snapshot{Handshakes: []connobs.HandshakeAttempt{
		{LFDI: "LFDIA", Reason: strings.Repeat("r", 1200*1024), At: time.Unix(1700000000, 0)},
	}}}
	s := newServer(t, Config{Key: testKey}, src)
	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels/gridappsd-connections", "Bearer "+testKey, "localhost")
	if rec.Code != http.StatusOK || rec.Body.Len() > 1<<20 {
		t.Fatalf("oversized row: %d, %d bytes, want 200 within 1 MiB", rec.Code, rec.Body.Len())
	}
	var d wireDescriptor
	decodeJSON(t, rec.Body.Bytes(), &d)
	hs := section(t, d, "Handshake attempts (cert validity)")
	if len(hs.Body.Rows) != 0 || !strings.HasPrefix(hs.Empty, "Showing 0 of 1 rows") {
		t.Errorf("section: %d rows, empty %q; want 0 rows and \"Showing 0 of 1 rows\"", len(hs.Body.Rows), hs.Empty)
	}
}

// devicesColumns fetches the Devices payload the way the shell does.
func devicesColumns(t *testing.T, s *Server) columnsPayload {
	t.Helper()
	w := doRequest(t, s.Handler(), http.MethodGet, "/dashboard/data", "Bearer "+testKey, "localhost")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/data = %d %s", w.Code, w.Body)
	}
	var p columnsPayload
	decodeJSON(t, w.Body.Bytes(), &p)
	return p
}

type columnsPayload struct {
	Columns []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Error string `json:"error"`
	} `json:"columns"`
	Devices []struct {
		LFDI  string            `json:"lfdi"`
		Cells map[string]string `json:"cells"`
	} `json:"devices"`
	Error string `json:"error"`
}
