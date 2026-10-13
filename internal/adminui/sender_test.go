package adminui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sender"
)

// switchPanelID is the panel the Publishing switch is served from.
const switchPanelID = panelPublishing

const testInputTopic = "/topic/goss.gridappsd.simulation.IEEE_2030_5.input"

// recordingBus keeps every frame; hold, when set, blocks each Send until
// it is closed, ignoring the context as go-stomp's Send does.
type recordingBus struct {
	mu      sync.Mutex
	frames  []recordedFrame
	hold    chan struct{}
	entered chan struct{}
	err     error
}

type recordedFrame struct {
	dest string
	body []byte
}

func (b *recordingBus) Send(_ context.Context, dest, _ string, body []byte) error {
	if b.hold != nil {
		b.entered <- struct{}{}
		<-b.hold
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	b.frames = append(b.frames, recordedFrame{dest, append([]byte(nil), body...)})
	return nil
}

func (b *recordingBus) sent() []recordedFrame {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.frames)
}

func newTestSender(t *testing.T, bus sender.Bus, on bool) *sender.Sender {
	t.Helper()
	reg := registry.New()
	for _, e := range []registry.Entry{
		{MRID: "_dev-b", Name: "Bravo", LFDI: strings.Repeat("B", 40)},
		{MRID: "_dev-a", Name: "Alpha", LFDI: strings.Repeat("A", 40)},
	} {
		if err := reg.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	s, err := sender.New(sender.Config{Bus: bus, Registry: reg, Destination: testInputTopic, PublishAtStart: on,
		FlipWait: 100 * time.Millisecond, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func senderServer(t *testing.T, snd *sender.Sender) *Server {
	t.Helper()
	src := testSources()
	src.Sender = snd
	return newServer(t, Config{Key: testKey}, src)
}

// postAction submits an action the way the shell does, from the fixed
// loopback address doRequest uses.
func postAction(t *testing.T, h http.Handler, action, body string) (int, map[string]any) {
	t.Helper()
	return postPanelAction(t, h, panelSender, action, body)
}

func postPanelAction(t *testing.T, h http.Handler, panel, action, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/apps/"+panel+"/"+action, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:40000"
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("POST %s: %d, body %q is not JSON", action, rec.Code, rec.Body.String())
	}
	return rec.Code, out
}

// senderPath is where a sender page panel's file or route is served.
func senderPath(panel, file string) string { return "/apps/" + panel + "/" + file }

// rawPageData fetches a sender page's data route through the full listener
// handler, as the page does.
func rawPageData(t *testing.T, s *Server, panel string) json.RawMessage {
	t.Helper()
	rec := doRequest(t, s.Handler(), http.MethodGet, senderPath(panel, "data"), "Bearer "+testKey, "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s data: status %d, body %s", panel, rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("%s data Content-Type %q, want JSON", panel, ct)
	}
	return slices.Clone(rec.Body.Bytes())
}

// TestSenderPagesDataCarriesTheDevicesLimitsAndSwitch: the sender page's
// data names each registered device by its mRID and label, in label order,
// with the bounds the forms enforce; the switch page's data carries the
// switch alone. Neither carries a null.
func TestSenderPagesDataCarriesTheDevicesLimitsAndSwitch(t *testing.T) {
	t.Parallel()
	snd := newTestSender(t, &recordingBus{}, false)
	s := senderServer(t, snd)

	var d struct {
		Title   string `json:"title"`
		On      bool   `json:"on"`
		Devices []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"devices"`
		Limits struct {
			MultiplierMin int64 `json:"multiplierMin"`
			MultiplierMax int64 `json:"multiplierMax"`
			ValueMin      int64 `json:"valueMin"`
			ValueMax      int64 `json:"valueMax"`
			RawMax        int64 `json:"rawMax"`
		} `json:"limits"`
		Sections []wireSection `json:"sections"`
	}
	body := rawPageData(t, s, panelSender)
	decodeJSON(t, body, &d)
	if d.Title != "Bus sender" || d.On {
		t.Errorf("title %q, on %t; want Bus sender and off", d.Title, d.On)
	}
	if len(d.Devices) != 2 || d.Devices[0].ID != "_dev-a" || d.Devices[0].Label != "Alpha" || d.Devices[1].ID != "_dev-b" || d.Devices[1].Label != "Bravo" {
		t.Errorf("devices = %+v, want Alpha=_dev-a then Bravo=_dev-b", d.Devices)
	}
	if l := d.Limits; l.MultiplierMin != -9 || l.MultiplierMax != 9 || l.ValueMin != -32768 || l.ValueMax != 32767 || l.RawMax != 16384 {
		t.Errorf("limits = %+v, want multiplier -9..9, value -32768..32767, raw 16384", l)
	}
	var headings []string
	for _, sec := range d.Sections {
		headings = append(headings, sec.Heading)
	}
	if !slices.Equal(headings, []string{"Publishing switch", "Recent sends", "Refusals"}) {
		t.Errorf("sections = %q", headings)
	}
	if strings.Contains(string(body), "null") {
		t.Errorf("sender data %s carries a null", body)
	}

	sw := rawPageData(t, s, switchPanelID)
	var w struct {
		Title    string        `json:"title"`
		On       bool          `json:"on"`
		Sections []wireSection `json:"sections"`
	}
	decodeJSON(t, sw, &w)
	if w.Title != "Bus publishing" || w.On || len(w.Sections) != 1 || w.Sections[0].Heading != "Publishing switch" || strings.Contains(string(sw), "null") {
		t.Errorf("switch data = %s, want the switch section alone, off, with no null", sw)
	}
	snd.SetPublishing(true, "x")
	decodeJSON(t, rawPageData(t, s, switchPanelID), &w)
	if !w.On {
		t.Error("switch data still reports off after the switch was turned on")
	}
}

// TestSenderPagesRefuseBadSubmissionsWithoutActing: every refusal the plane
// made for a panel action is made by the page route, and none of them
// reaches the sender: nothing is published and the switch is as it was.
func TestSenderPagesRefuseBadSubmissionsWithoutActing(t *testing.T) {
	t.Parallel()
	bus := &recordingBus{}
	snd := newTestSender(t, bus, true)
	s := senderServer(t, snd)
	h := s.Handler()

	cases := []struct {
		name, panel, route, body string
		field                    string
	}{
		{"unknown field", panelSender, "connect", `{"device":"_dev-a","connect":true,"extra":1}`, ""},
		{"missing field", panelSender, "connect", `{"device":"_dev-a"}`, ""},
		{"a field renamed", panelSender, "connect", `{"device":"_dev-a","other":true}`, "connect"},
		{"duplicate key", panelSender, "connect", `{"device":"_dev-a","connect":true,"connect":false}`, ""},
		{"two objects", panelSender, "connect", `{"device":"_dev-a","connect":true}{"device":"_dev-a","connect":false}`, ""},
		{"trailing text", panelSender, "connect", `{"device":"_dev-a","connect":true} x`, ""},
		{"an array", panelSender, "connect", `[{"device":"_dev-a","connect":true}]`, ""},
		{"null", panelSender, "connect", `null`, ""},
		{"empty body", panelSender, "connect", ``, ""},
		{"string for a boolean", panelSender, "connect", `{"device":"_dev-a","connect":"true"}`, "connect"},
		{"number for a boolean", panelSender, "energize", `{"device":"_dev-a","energize":1}`, "energize"},
		{"device not registered", panelSender, "connect", `{"device":"_nope","connect":true}`, "device"},
		{"device empty", panelSender, "connect", `{"device":"","connect":true}`, "device"},
		{"device a number", panelSender, "connect", `{"device":7,"connect":true}`, "device"},
		{"multiplier over", panelSender, "active-power", `{"device":"_dev-a","multiplier":10,"value":2}`, "multiplier"},
		{"multiplier under", panelSender, "reactive-power", `{"device":"_dev-a","multiplier":-10,"value":2}`, "multiplier"},
		{"value over", panelSender, "active-power", `{"device":"_dev-a","multiplier":1,"value":32768}`, "value"},
		{"value under", panelSender, "reactive-power", `{"device":"_dev-a","multiplier":1,"value":-32769}`, "value"},
		{"value a float", panelSender, "active-power", `{"device":"_dev-a","multiplier":1,"value":1.0}`, "value"},
		{"value an exponent", panelSender, "active-power", `{"device":"_dev-a","multiplier":1,"value":1e2}`, "value"},
		{"value quoted", panelSender, "active-power", `{"device":"_dev-a","multiplier":1,"value":"2"}`, "value"},
		{"raw empty", panelSender, "raw", `{"json":""}`, "json"},
		{"raw with a control byte", panelSender, "raw", `{"json":"{\u0000}"}`, "json"},
		{"raw with a bidi override", panelSender, "raw", `{"json":"{\u202e}"}`, "json"},
		{"raw not a string", panelSender, "raw", `{"json":{}}`, "json"},
		{"switch value a string", switchPanelID, "publishing", `{"on":"false"}`, "on"},
		{"switch value null", switchPanelID, "publishing", `{"on":null}`, "on"},
		{"switch unknown field", switchPanelID, "publishing", `{"on":false,"by":"me"}`, ""},
		{"switch two objects", switchPanelID, "publishing", `{"on":false}{"on":false}`, ""},
	}
	for _, tc := range cases {
		code, out := postPanelAction(t, h, tc.panel, tc.route, tc.body)
		if code != http.StatusBadRequest || out["error"] != "invalid action values" {
			t.Errorf("%s: %d %v, want 400 invalid action values", tc.name, code, out)
			continue
		}
		if got, _ := out["field"].(string); got != tc.field {
			t.Errorf("%s: field %q, want %q", tc.name, got, tc.field)
		}
	}
	if f := bus.sent(); len(f) != 0 {
		t.Errorf("refused submissions published %d frame(s)", len(f))
	}
	if st := snd.Publishing(); !st.On || st.ChangedBy != "start" {
		t.Errorf("switch = %+v after refused submissions, want it untouched", st)
	}
	if rows := snd.Recent(); len(rows) != 0 {
		t.Errorf("refused submissions left %d sender row(s)", len(rows))
	}
}

// TestSenderPagesBoundTheBodyAndTheContentType: a body over the cap is
// refused 413 whether or not its length is declared, and a body that is not
// JSON-typed is refused 415, both before the sender sees anything.
func TestSenderPagesBoundTheBodyAndTheContentType(t *testing.T) {
	t.Parallel()
	bus := &recordingBus{}
	snd := newTestSender(t, bus, true)
	s := senderServer(t, snd)

	post := func(body io.Reader, ctype string) int {
		req := httptest.NewRequest(http.MethodPost, senderPath(panelSender, "raw"), body)
		req.RemoteAddr = "127.0.0.1:40000"
		req.Host = "localhost"
		req.Header.Set("Authorization", "Bearer "+testKey)
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	over := strings.Repeat("x", 16<<10+1)
	if code := post(strings.NewReader(over), "application/json"); code != http.StatusRequestEntityTooLarge {
		t.Errorf("declared length over the cap = %d, want 413", code)
	}
	if code := post(io.MultiReader(strings.NewReader(over)), "application/json"); code != http.StatusRequestEntityTooLarge {
		t.Errorf("undeclared length over the cap = %d, want 413", code)
	}
	atCap := `{"json":"` + strings.Repeat("a", 16<<10-len(`{"json":""}`)) + `"}`
	if len(atCap) != 16<<10 {
		t.Fatalf("test body is %d bytes, want exactly the cap", len(atCap))
	}
	if code := post(strings.NewReader(atCap), "application/json"); code == http.StatusRequestEntityTooLarge {
		t.Errorf("a body exactly at the cap = %d, want it read", code)
	}
	good := `{"json":"{}"}`
	for _, ctype := range []string{"", "text/plain", "application/x-www-form-urlencoded", "application/jsonx", "application/json; charset=\"", "multipart/form-data"} {
		if code := post(strings.NewReader(good), ctype); code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q = %d, want 415", ctype, code)
		}
	}
	for _, ctype := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON"} {
		if code := post(strings.NewReader(good), ctype); code == http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q = 415, want it accepted", ctype)
		}
	}
	if f := bus.sent(); len(f) != 0 {
		t.Errorf("refused submissions published %d frame(s)", len(f))
	}
}

// TestSenderPagesMountOnlyThePostsTheyDeclare: a page route answers its own
// method alone, the old panel action routes are gone, and a write needs the
// credential.
func TestSenderPagesMountOnlyThePostsTheyDeclare(t *testing.T) {
	t.Parallel()
	bus := &recordingBus{}
	snd := newTestSender(t, bus, true)
	s := senderServer(t, snd)
	h := s.Handler()

	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, route := range []struct{ panel, name string }{{panelSender, "connect"}, {switchPanelID, "publishing"}} {
			if rec := doRequest(t, h, m, senderPath(route.panel, route.name), "Bearer "+testKey, "localhost"); rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 405", m, route.name, rec.Code)
			}
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if rec := doRequest(t, h, m, senderPath(panelSender, "data"), "Bearer "+testKey, "localhost"); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s data = %d, want 405", m, rec.Code)
		}
	}
	if rec := doRequest(t, h, http.MethodPost, senderPath(panelSender, "no-such-form"), "Bearer "+testKey, "localhost"); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST to an undeclared route = %d, want 404 or 405", rec.Code)
	}
	for _, old := range []string{
		"/api/ui/panels/" + panelSender + "/actions/connect",
		"/api/ui/panels/" + switchPanelID + "/actions/publishing",
		"/api/ui/panels/" + panelSender + "/actions",
	} {
		method := http.MethodPost
		if strings.HasSuffix(old, "/actions") {
			method = http.MethodGet
		}
		req := httptest.NewRequest(method, old, strings.NewReader(`{"device":"_dev-a","connect":true}`))
		req.RemoteAddr = "127.0.0.1:40000"
		req.Host = "localhost"
		req.Header.Set("Authorization", "Bearer "+testKey)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 now that panel actions are off", method, old, rec.Code)
		}
	}
	if f := bus.sent(); len(f) != 0 {
		t.Errorf("a refused route published %d frame(s)", len(f))
	}
	req := httptest.NewRequest(http.MethodPost, senderPath(panelSender, "connect"), strings.NewReader(`{"device":"_dev-a","connect":true}`))
	req.RemoteAddr = "127.0.0.1:40000"
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || len(bus.sent()) != 0 {
		t.Errorf("POST with no credential = %d, %d frame(s); want 401 and none", rec.Code, len(bus.sent()))
	}
}

// TestReadOnlyPlaneStillTakesSenderPosts: the bridge's plane is read only,
// which mounts none of its own write routes but leaves a page's POSTs, as
// it left panel actions. The control is a plane write route that the same
// setting does remove.
func TestReadOnlyPlaneStillTakesSenderPosts(t *testing.T) {
	t.Parallel()
	bus := &recordingBus{}
	snd := newTestSender(t, bus, true)
	s := senderServer(t, snd)

	if code, out := postAction(t, s.Handler(), "connect", `{"device":"_dev-a","connect":true}`); code != http.StatusOK {
		t.Fatalf("sender POST on the read only plane: %d %v, want 200", code, out)
	}
	if len(bus.sent()) != 1 {
		t.Errorf("frames = %d, want the one send", len(bus.sent()))
	}
	req := httptest.NewRequest(http.MethodPost, "/api/certs/info", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:40000"
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("plane write route on a read only plane = %d, want it not mounted", rec.Code)
	}
}

// TestNoPanelUsesADescriptorKind: every panel the bridge registers is a
// page, with no View, Picker, Stream or Action, so no tab needs a server-go
// descriptor kind.
func TestNoPanelUsesADescriptorKind(t *testing.T) {
	t.Parallel()
	mon, _ := newTestMonitor(t)
	src := testSources()
	src.Sender = newTestSender(t, &recordingBus{}, false)
	src.Monitor = mon
	s := newServer(t, Config{Key: testKey}, src)

	panels := s.panels()
	if len(panels) != 8 {
		t.Fatalf("panels = %d, want the 8 tabs of a bridge with a monitor and a sender", len(panels))
	}
	for _, p := range panels {
		if p.Page == nil || p.View != nil || p.Picker != nil || p.Stream != nil || len(p.Actions) != 0 || p.Assets != nil {
			t.Errorf("panel %s: page %t view %t picker %t stream %t actions %d, want a page alone", p.ID, p.Page != nil, p.View != nil, p.Picker != nil, p.Stream != nil, len(p.Actions))
		}
	}
	wantTail := []string{panelBusMonitor, panelPublishing, panelSender}
	for i, id := range wantTail {
		if got := panels[len(panels)-3+i].ID; got != id {
			t.Errorf("tab %d from the end = %s, want %s", 3-i, got, id)
		}
	}
	if labels := []string{panels[6].Label, panels[7].Label}; !slices.Equal(labels, []string{"Bus publishing", "Bus sender"}) {
		t.Errorf("sender tab labels = %q", labels)
	}
}

// TestSenderFormPublishesTheValuesSubmitted: a form reaches the input
// topic as one forward difference carrying the submitted device, attribute
// and value, and its row records who sent it.
func TestSenderFormPublishesTheValuesSubmitted(t *testing.T) {
	t.Parallel()
	bus := &recordingBus{}
	snd := newTestSender(t, bus, true)
	s := senderServer(t, snd)

	code, out := postAction(t, s.Handler(), "active-power", `{"device":"_dev-a","multiplier":2,"value":-150}`)
	if code != http.StatusOK || !strings.HasPrefix(out["message"].(string), "Published difference_mrid ") {
		t.Fatalf("active-power: %d %v", code, out)
	}
	code, _ = postAction(t, s.Handler(), "connect", `{"device":"_dev-b","connect":false}`)
	if code != http.StatusOK {
		t.Fatalf("connect: %d", code)
	}
	frames := bus.sent()
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	type delta struct {
		Object    string          `json:"object"`
		Attribute string          `json:"attribute"`
		Value     json.RawMessage `json:"value"`
	}
	var msgs [2]struct {
		Input struct {
			Message struct {
				MRID    string  `json:"difference_mrid"`
				Forward []delta `json:"forward_differences"`
			} `json:"message"`
		} `json:"input"`
	}
	for i, f := range frames {
		if f.dest != testInputTopic {
			t.Errorf("frame %d to %q", i, f.dest)
		}
		decodeJSON(t, f.body, &msgs[i])
	}
	wantDeltas := []delta{
		{"_dev-a", sender.AttrActivePower, json.RawMessage(`{"multiplier":2,"value":-150}`)},
		{"_dev-b", sender.AttrConnect, json.RawMessage(`false`)},
	}
	for i, w := range wantDeltas {
		fw := msgs[i].Input.Message.Forward
		if len(fw) != 1 || fw[0].Object != w.Object || fw[0].Attribute != w.Attribute || string(fw[0].Value) != string(w.Value) {
			t.Errorf("frame %d forward = %+v, want %+v", i, fw, w)
		}
	}
	if !strings.Contains(out["message"].(string), msgs[0].Input.Message.MRID) {
		t.Errorf("answer %q does not name the published mrid %q", out["message"], msgs[0].Input.Message.MRID)
	}
	rows := snd.Recent()
	if len(rows) != 2 || rows[1].Remote != "127.0.0.1:40000" || rows[1].DifferenceMRID != msgs[0].Input.Message.MRID {
		t.Errorf("rows = %+v, want the active power row from 127.0.0.1:40000", rows)
	}
}

// TestSenderRawAndRefusalsReachTheOperator: a valid raw body is sent byte
// for byte, a bad one is refused with its JSON path, and with the switch off
// nothing is published.
func TestSenderRawAndRefusalsReachTheOperator(t *testing.T) {
	t.Parallel()
	bus := &recordingBus{}
	snd := newTestSender(t, bus, false)
	s := senderServer(t, snd)

	// The surrounding whitespace must reach the bus as typed.
	raw := " " + `{"command":"update","input":{"message":{"timestamp":1,"difference_mrid":"raw-1","reverse_differences":[],"forward_differences":[{"object":"_dev-a","attribute":"DERControl.DERControlBase.opModEnergize","value":true}]}}}` + "\n"
	rawField, _ := json.Marshal(map[string]string{"json": raw})

	code, out := postAction(t, s.Handler(), "raw", string(rawField))
	if code != http.StatusUnprocessableEntity || out["error"] != "publishing is off" {
		t.Errorf("raw while off: %d %v, want 422 publishing is off", code, out)
	}
	if code, out := postPanelAction(t, s.Handler(), switchPanelID, "publishing", `{"on":true}`); code != http.StatusOK || out["message"] != "Publishing is ON." {
		t.Fatalf("switch on: %d %v", code, out)
	}
	if code, out := postAction(t, s.Handler(), "raw", string(rawField)); code != http.StatusOK {
		t.Fatalf("raw: %d %v", code, out)
	}
	if f := bus.sent(); len(f) != 1 || string(f[0].body) != raw {
		t.Errorf("published %+v, want the raw body byte for byte", f)
	}

	bad := strings.Replace(raw, `"value":true`, `"value":"yes"`, 1)
	bad = strings.Replace(bad, "raw-1", "raw-2", 1)
	badField, _ := json.Marshal(map[string]string{"json": bad})
	code, out = postAction(t, s.Handler(), "raw", string(badField))
	if code != http.StatusUnprocessableEntity || !strings.Contains(out["error"].(string), "forward_differences[0].value") {
		t.Errorf("bad raw: %d %v, want 422 naming forward_differences[0].value", code, out)
	}
	if len(bus.sent()) != 1 {
		t.Errorf("a refused body was published")
	}
	refusals := snd.Refusals()
	if len(refusals) != 2 || refusals[1].Reason != "publishing is off" || refusals[0].Remote != "127.0.0.1:40000" {
		t.Errorf("refusals = %+v", refusals)
	}
}

// TestSenderViewShowsTheTrueSwitchAndRows: the View's badge, time and
// remote are the sender's own state, and its tables are the sender's rows.
func TestSenderViewShowsTheTrueSwitchAndRows(t *testing.T) {
	t.Parallel()
	snd := newTestSender(t, &recordingBus{}, false)
	s := senderServer(t, snd)

	sw := entries(t, section(t, getPanel(t, s, panelSender), "Publishing switch"))
	assertBadge(t, "switch at start", sw["Publishing"], "neutral", "OFF")
	if sw["Changed from"].Text != "start" {
		t.Errorf("changed from = %+v, want start", sw["Changed from"])
	}

	postPanelAction(t, s.Handler(), switchPanelID, "publishing", `{"on":true}`)
	postAction(t, s.Handler(), "energize", `{"device":"_dev-a","energize":true}`)
	postAction(t, s.Handler(), "connect", `{"device":"_nope","connect":true}`) // not a choice: refused before the sender

	d := getPanel(t, s, panelSender)
	sw = entries(t, section(t, d, "Publishing switch"))
	st := snd.Publishing()
	assertBadge(t, "switch after flip", sw["Publishing"], "warn", "ON")
	if sw["Changed from"].Text != "127.0.0.1:40000" || sw["Changed from"].Text != st.ChangedBy ||
		sw["Last changed"].DateTime == "" || !strings.HasPrefix(sw["Last changed"].Text, st.ChangedAt.Format("2006-01-02T15:04:05")) {
		t.Errorf("switch = %+v, sender state %+v", sw, st)
	}
	recent := section(t, d, "Recent sends")
	assertColumns(t, recent, "Time", "Kind", "From", "difference_mrid", "Changes", "Outcome", "Reason")
	rows := snd.Recent()
	if len(recent.Body.Rows) != len(rows) || len(rows) != 2 {
		t.Fatalf("table rows %d, sender rows %d, want 2", len(recent.Body.Rows), len(rows))
	}
	send := texts(recent.Body.Rows[0])
	if send[1] != "form" || send[3] != rows[0].DifferenceMRID || send[4] != "_dev-a "+sender.AttrEnergize+"=true" {
		t.Errorf("send row = %q", send)
	}
	assertBadge(t, "send outcome", recent.Body.Rows[0][5], "warn", sender.OutcomePending)
	if flip := texts(recent.Body.Rows[1]); flip[1] != "switch" || flip[2] != "127.0.0.1:40000" {
		t.Errorf("flip row = %q", flip)
	}
	pub := getHealthData(t, s).Publishing
	if pub == nil || !pub.On || pub.ChangedBy != st.ChangedBy || pub.ChangedAt != st.ChangedAt.Format(timeFormat) {
		t.Errorf("health publishing = %+v, want on, changed by %q at %s", pub, st.ChangedBy, st.ChangedAt.Format(timeFormat))
	}
}

// TestSenderOffFlipShowsSendsStillInFlight: the off answer and the View
// both carry the in-flight count and the warning that such a send can
// still be delivered.
func TestSenderOffFlipShowsSendsStillInFlight(t *testing.T) {
	t.Parallel()
	bus := &recordingBus{hold: make(chan struct{}), entered: make(chan struct{}, 1)}
	snd := newTestSender(t, bus, true)
	s := senderServer(t, snd)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = snd.SendConnect(context.Background(), "a", "_dev-a", true)
	}()
	<-bus.entered
	code, out := postPanelAction(t, s.Handler(), switchPanelID, "publishing", `{"on":false}`)
	close(bus.hold)
	<-done
	msg, _ := out["message"].(string)
	if code != http.StatusOK || !strings.Contains(msg, "Publishing is OFF. 1 send(s) were still in flight") || !strings.Contains(msg, stillDeliverable) {
		t.Errorf("off answer = %d %q", code, msg)
	}
	sw := entries(t, section(t, getPanel(t, s, panelSender), "Publishing switch"))
	assertBadge(t, "still in flight", sw["Still in flight at the off flip"], "warn", "1 send(s); they may still be delivered")
}

// TestRunWaitsForARunningAction: shutdown tells a running action to stop
// and waits for it, so a send whose bus ignores cancellation has finished,
// and recorded its row, before Run returns.
func TestRunWaitsForARunningAction(t *testing.T) {
	t.Parallel()
	bus := &recordingBus{hold: make(chan struct{}), entered: make(chan struct{}, 1)}
	snd := newTestSender(t, bus, true)
	src := testSources()
	src.Sender = snd
	s := newServer(t, Config{Key: testKey}, src)
	s.timeouts.shutdown = 3 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	answer := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/apps/"+panelSender+"/energize",
			strings.NewReader(`{"device":"_dev-a","energize":false}`))
		req.Host = "localhost"
		req.Header.Set("Authorization", "Bearer "+testKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			answer <- err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		answer <- fmt.Sprintf("%d %s", resp.StatusCode, b)
	}()
	select {
	case <-bus.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the action never reached the bus")
	}
	cancel()
	time.AfterFunc(300*time.Millisecond, func() { close(bus.hold) })
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Run did not return")
	}
	if rows := snd.Recent(); len(rows) != 1 || rows[0].Kind != sender.KindForm {
		t.Errorf("rows when Run returned = %+v, want the finished send", rows)
	}
	// The operator was told the outcome is not known, not that it failed.
	if got := <-answer; !strings.HasPrefix(got, "503 ") || !strings.Contains(got, "may still complete") {
		t.Errorf("action answer = %q, want 503 saying it may still complete", got)
	}
}

// TestSwitchTurnsOffWhileAPanelSendIsBlocked: the off flip must reach the
// sender while a send made through the panel is held on the bus, which a
// panel answering one call at a time would refuse as busy. The flip then
// waits its bound for the held send and reports it.
func TestSwitchTurnsOffWhileAPanelSendIsBlocked(t *testing.T) {
	t.Parallel()
	bus := &recordingBus{hold: make(chan struct{}), entered: make(chan struct{}, 1)}
	snd := newTestSender(t, bus, true)
	s := senderServer(t, snd)

	sendDone := make(chan int, 1)
	go func() {
		code, _ := postAction(t, s.Handler(), "energize", `{"device":"_dev-a","energize":true}`)
		sendDone <- code
	}()
	<-bus.entered

	start := time.Now()
	code, out := postPanelAction(t, s.Handler(), switchPanelID, "publishing", `{"on":false}`)
	waited := time.Since(start)
	close(bus.hold)
	<-sendDone

	msg, _ := out["message"].(string)
	if code != http.StatusOK || !strings.Contains(msg, "Publishing is OFF. 1 send(s) were still in flight") {
		t.Fatalf("off flip during a held send: %d %v, want 200 reporting the held send", code, out)
	}
	if snd.Publishing().On {
		t.Error("the switch is still on")
	}
	if waited < 100*time.Millisecond {
		t.Errorf("the flip returned after %v, want it to wait its 100ms bound for the held send", waited)
	}
}

// TestBusFailureIsAFailureNotARefusal: a send the bus rejects is answered
// as a failed action, with no transport text in the answer, and its row
// keeps the cause for the operator.
func TestBusFailureIsAFailureNotARefusal(t *testing.T) {
	t.Parallel()
	bus := &recordingBus{err: errors.New("write tcp 10.0.0.9:61613: broken pipe")}
	snd := newTestSender(t, bus, true)
	s := senderServer(t, snd)

	code, out := postAction(t, s.Handler(), "connect", `{"device":"_dev-a","connect":true}`)
	if code != http.StatusInternalServerError || out["error"] != "action failed" {
		t.Errorf("bus failure: %d %v, want 500 action failed", code, out)
	}
	if strings.Contains(fmt.Sprint(out), "broken pipe") || strings.Contains(fmt.Sprint(out), "10.0.0.9") {
		t.Errorf("answer %v carries the transport error", out)
	}
	rows := snd.Recent()
	if len(rows) != 1 || rows[0].Outcome != sender.OutcomeFailed || !strings.Contains(rows[0].Reason, "broken pipe") {
		t.Errorf("rows = %+v, want one failed row naming the cause", rows)
	}
}

// TestShutdownStaysWithinOneBound: an action that never returns and a
// client that never finishes its request each hold one shutdown step; the
// two steps share the one configured bound instead of taking it twice.
func TestShutdownStaysWithinOneBound(t *testing.T) {
	t.Parallel()
	bus := &recordingBus{hold: make(chan struct{}), entered: make(chan struct{}, 1)}
	t.Cleanup(func() { close(bus.hold) })
	src := testSources()
	src.Sender = newTestSender(t, bus, true)
	s := newServer(t, Config{Key: testKey}, src)
	const bound = time.Second
	s.timeouts.shutdown = bound
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/apps/"+panelSender+"/energize",
			strings.NewReader(`{"device":"_dev-a","energize":true}`))
		req.Host = "localhost"
		req.Header.Set("Authorization", "Bearer "+testKey)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-bus.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the action never reached the bus")
	}
	// A request whose headers never finish keeps its connection active,
	// so Shutdown waits for it until its deadline.
	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write([]byte("GET /api/health HTTP/1.1\r\nHost: localhost\r\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	cancel()
	select {
	case err := <-runErr:
		t.Logf("Run returned %v after %v", err, time.Since(start))
		if d := time.Since(start); d > bound+bound/2 {
			t.Errorf("Run took %v with a %v shutdown bound", d, bound)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}
