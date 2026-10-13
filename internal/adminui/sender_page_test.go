package adminui

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/senderpage"
)

type senderPost struct {
	Status  int             `json:"status,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
	Reject  string          `json:"reject,omitempty"`
	NotJSON bool            `json:"notJSON,omitempty"`
}

type senderScenario struct {
	Body      json.RawMessage `json:"body,omitempty"`
	Body2     json.RawMessage `json:"body2,omitempty"`
	Status    int             `json:"status,omitempty"`
	Post      *senderPost     `json:"post,omitempty"`
	HoldPost  bool            `json:"holdPost,omitempty"`
	Hidden    bool            `json:"hidden,omitempty"`
	NoTimeout bool            `json:"noTimeout,omitempty"`
	Steps     []string        `json:"steps,omitempty"`
}

type senderNode struct {
	Text     string `json:"text"`
	Class    string `json:"cls"`
	Disabled bool   `json:"disabled"`
}

type senderRun struct {
	URLs  []string `json:"urls"`
	Posts []struct {
		URL         string `json:"url"`
		ContentType string `json:"contentType"`
		Body        string `json:"body"`
	} `json:"posts"`
	Title       string        `json:"title"`
	DocTitle    string        `json:"docTitle"`
	Status      string        `json:"status"`
	StatusClass string        `json:"statusClass"`
	Sections    []nodeSection `json:"sections"`
	Toggle      *senderNode   `json:"toggle"`
	Answer      *senderNode   `json:"answer"`
	Results     map[string]*senderNode
	Selects     map[string]struct {
		Value    string `json:"value"`
		Disabled bool   `json:"disabled"`
		Rebuilds int    `json:"rebuilds"`
		Options  []struct {
			Value string `json:"value"`
			Text  string `json:"text"`
		} `json:"options"`
	} `json:"selects"`
	Limits map[string]struct {
		Min string `json:"min"`
		Max string `json:"max"`
	} `json:"limits"`
	Steps []struct {
		Step    string `json:"step"`
		Fetches int    `json:"fetches"`
		Posts   int    `json:"posts"`
		Timers  int    `json:"timers"`
	} `json:"steps"`
	Timers       int `json:"timers"`
	Prevented    int `json:"prevented"`
	InnerHTML    int `json:"innerHTML"`
	Style        int `json:"style"`
	TimeoutCalls int `json:"timeoutCalls"`
	Controllers  int `json:"controllers"`
}

func senderPageFile(t *testing.T, name string) string {
	t.Helper()
	b, err := senderpage.Files.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// runSenderPage runs lib then source against the scenarios on a fake DOM
// (testdata/sender_page.js). A missing node skips the test locally and fails
// it under CI, as the other page tests do.
func runSenderPage(t *testing.T, lib, source string, scenarios map[string]senderScenario) map[string]senderRun {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("node not installed, and CI must run the page test: %v", err)
		}
		t.Skip("node not installed")
	}
	in, err := json.Marshal(map[string]any{"lib": lib, "source": source, "scenarios": scenarios})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "testdata/sender_page.js")
	cmd.Stdin = bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, stderr.String())
	}
	var runs map[string]senderRun
	if err := json.Unmarshal(out, &runs); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return runs
}

func senderPageBodies(t *testing.T) (off, on, sendr json.RawMessage) {
	t.Helper()
	offS := senderServer(t, newTestSender(t, &recordingBus{}, false))
	onS := senderServer(t, newTestSender(t, &recordingBus{}, true))
	return rawPageData(t, offS, switchPanelID), rawPageData(t, onS, switchPanelID), rawPageData(t, offS, panelSender)
}

func postBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("posted body %q is not a JSON object: %v", raw, err)
	}
	return m
}

func TestSwitchPageShowsTheStateAndSendsTheOpposite(t *testing.T) {
	t.Parallel()
	off, on, _ := senderPageBodies(t)
	lib, src := senderPageFile(t, "sections.js"), senderPageFile(t, "switch.js")
	runs := runSenderPage(t, lib, src, map[string]senderScenario{
		"off":    {Body: off, Steps: []string{"submit:switch"}},
		"on":     {Body: on, Steps: []string{"submit:switch"}},
		"idle":   {Body: off},
		"nodata": {Status: 500, Body: json.RawMessage(`{"error":"boom"}`), Steps: []string{"submit:switch"}},
	})

	o := runs["off"]
	if o.Title != "Bus publishing" || !strings.HasPrefix(o.Status, "updated ") || o.Toggle.Text != "Turn publishing on" {
		t.Errorf("off page: title %q, status %q, button %q", o.Title, o.Status, o.Toggle.Text)
	}
	if len(o.Sections) != 1 || o.Sections[0].Heading != "Publishing switch" || len(o.Sections[0].Prose) != 2 {
		t.Fatalf("sections = %+v, want the switch section with its two sentences", o.Sections)
	}
	e := o.Sections[0].Groups[0].Entries
	if e[0].Key != "Publishing" || e[0].Value.Text != "OFF" || e[0].Value.Class != "badge badge-neutral" || e[2].Key != "Changed from" || e[2].Value.Text != "start" {
		t.Errorf("switch entries = %+v", e)
	}
	if len(o.Posts) != 1 || o.Posts[0].URL != "publishing" || o.Posts[0].ContentType != "application/json" || o.Prevented != 1 {
		t.Fatalf("posts = %+v, prevented %d; want one JSON post to publishing", o.Posts, o.Prevented)
	}
	if got := postBody(t, o.Posts[0].Body); !jsonEqual(got, map[string]any{"on": true}) {
		t.Errorf("posted %v, want on true", got)
	}
	if o.Answer.Text != "Done." || o.Answer.Class != "result ok" {
		t.Errorf("answer = %+v", o.Answer)
	}
	if len(o.URLs) != 2 {
		t.Errorf("data fetched %d times, want a reload after the post (2)", len(o.URLs))
	}

	n := runs["on"]
	if n.Toggle.Text != "Turn publishing off" || len(n.Posts) != 1 || !jsonEqual(postBody(t, n.Posts[0].Body), map[string]any{"on": false}) {
		t.Errorf("on page: button %q, posts %+v; want an off post", n.Toggle.Text, n.Posts)
	}
	if runs["idle"].Toggle.Disabled || len(runs["idle"].Posts) != 0 {
		t.Errorf("idle page: button disabled %t, posts %d; want it ready and nothing sent", runs["idle"].Toggle.Disabled, len(runs["idle"].Posts))
	}
	nd := runs["nodata"]
	if !nd.Toggle.Disabled || len(nd.Posts) != 0 || !strings.Contains(nd.Status, "unavailable") || nd.StatusClass != "error" {
		t.Errorf("no data: button disabled %t, posts %d, status %q (%q); want it disabled and nothing sent", nd.Toggle.Disabled, len(nd.Posts), nd.Status, nd.StatusClass)
	}
}

func jsonEqual(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestSwitchPageTellsAnAnswerFromARefusalAndALostAnswer(t *testing.T) {
	t.Parallel()
	off, _, _ := senderPageBodies(t)
	lib, src := senderPageFile(t, "sections.js"), senderPageFile(t, "switch.js")
	runs := runSenderPage(t, lib, src, map[string]senderScenario{
		"refused":  {Body: off, Post: &senderPost{Status: 422, Body: json.RawMessage(`{"error":"publishing is off"}`)}, Steps: []string{"submit:switch"}},
		"invalid":  {Body: off, Post: &senderPost{Status: 400, Body: json.RawMessage(`{"error":"invalid action values","field":"on"}`)}, Steps: []string{"submit:switch"}},
		"failed":   {Body: off, Post: &senderPost{Status: 500, Body: json.RawMessage(`{"error":"action failed"}`)}, Steps: []string{"submit:switch"}},
		"proxy":    {Body: off, Post: &senderPost{Status: 502, NotJSON: true}, Steps: []string{"submit:switch"}},
		"lost":     {Body: off, Post: &senderPost{Reject: "network down"}, Steps: []string{"submit:switch"}},
		"held":     {Body: off, HoldPost: true, Steps: []string{"submit:switch", "submit:switch"}},
		"released": {Body: off, HoldPost: true, Steps: []string{"submit:switch", "release"}},
	})
	for name, want := range map[string]string{
		"refused": "publishing is off",
		"invalid": "invalid action values (on)",
		"failed":  "action failed",
		"proxy":   "HTTP 502",
	} {
		if a := runs[name].Answer; a.Text != want || a.Class != "result error" {
			t.Errorf("%s: answer %+v, want %q as an error", name, a, want)
		}
	}
	if a := runs["lost"].Answer; !strings.HasPrefix(a.Text, "no answer: network down") || !strings.Contains(a.Text, "Recent sends") || a.Class != "result error" {
		t.Errorf("lost: answer %+v, want a no-answer notice that sends the operator to the record", a)
	}
	h := runs["held"]
	if len(h.Posts) != 1 || !h.Toggle.Disabled || h.Answer.Text != "sending" {
		t.Errorf("held: %d posts, button disabled %t, answer %q; want one post and the button off while it is out", len(h.Posts), h.Toggle.Disabled, h.Answer.Text)
	}
	if r := runs["released"]; r.Toggle.Disabled || r.Answer.Text != "Done." {
		t.Errorf("released: button disabled %t, answer %q; want it ready again with the answer", r.Toggle.Disabled, r.Answer.Text)
	}
}

func TestSwitchPagePollsOnlyWhileVisibleAndFallsBackWithoutAbortTimeout(t *testing.T) {
	t.Parallel()
	off, _, _ := senderPageBodies(t)
	lib, src := senderPageFile(t, "sections.js"), senderPageFile(t, "switch.js")
	runs := runSenderPage(t, lib, src, switchPollScenarios(off))
	h := runs["hideLater"]
	if len(h.Steps) != 4 || h.Steps[1].Fetches != 1 || h.Steps[1].Timers != 0 || h.Steps[2].Fetches != 2 || h.Steps[2].Timers != 1 {
		t.Errorf("hide, tick, show: %+v, want no fetch while hidden and polling resumed at once on show", h.Steps)
	}
	hs := runs["hiddenStart"]
	if hs.Steps[0].Fetches != 0 || hs.Steps[1].Fetches != 1 {
		t.Errorf("hidden start: %+v, want 0 fetches while hidden and 1 after show", hs.Steps)
	}
	if old := runs["old"]; !strings.HasPrefix(old.Status, "updated ") || old.TimeoutCalls != 0 || old.Controllers != 1 {
		t.Errorf("without AbortSignal.timeout: status %q, %d timeout calls, %d controllers; want one controller", old.Status, old.TimeoutCalls, old.Controllers)
	}
}

func switchPollScenarios(body json.RawMessage) map[string]senderScenario {
	return map[string]senderScenario{
		"hideLater":   {Body: body, Steps: []string{"hide", "tick", "show", "tick"}},
		"hiddenStart": {Body: body, Hidden: true, Steps: []string{"tick", "show"}},
		"old":         {Body: body, NoTimeout: true},
	}
}

func TestSenderPageSendsEachFormAsDeclared(t *testing.T) {
	t.Parallel()
	_, _, body := senderPageBodies(t)
	lib, src := senderPageFile(t, "sections.js"), senderPageFile(t, "sender.js")
	runs := runSenderPage(t, lib, src, map[string]senderScenario{
		"active": {Body: body, Steps: []string{"set:active-power-device=_dev-b", "set:active-power-multiplier=2", "set:active-power-value=-150", "submit:active-power"}},
		"react":  {Body: body, Steps: []string{"set:reactive-power-multiplier=-3", "set:reactive-power-value= 7 ", "submit:reactive-power"}},
		"conn":   {Body: body, Steps: []string{"check:connect-connect=true", "submit:connect"}},
		"disc":   {Body: body, Steps: []string{"submit:connect"}},
		"ener":   {Body: body, Steps: []string{"set:energize-device=_dev-b", "check:energize-energize=true", "submit:energize"}},
		"raw":    {Body: body, Steps: []string{`set:raw-json={"a":"x=y"}`, "submit:raw"}},
	})
	want := map[string]struct {
		url  string
		body map[string]any
	}{
		"active": {"active-power", map[string]any{"device": "_dev-b", "multiplier": 2, "value": -150}},
		"react":  {"reactive-power", map[string]any{"device": "_dev-a", "multiplier": -3, "value": 7}},
		"conn":   {"connect", map[string]any{"device": "_dev-a", "connect": true}},
		"disc":   {"connect", map[string]any{"device": "_dev-a", "connect": false}},
		"ener":   {"energize", map[string]any{"device": "_dev-b", "energize": true}},
		"raw":    {"raw", map[string]any{"json": `{"a":"x=y"}`}},
	}
	for name, w := range want {
		r := runs[name]
		if len(r.Posts) != 1 || r.Posts[0].URL != w.url || r.Posts[0].ContentType != "application/json" {
			t.Errorf("%s: posts %+v, want one JSON post to %s", name, r.Posts, w.url)
			continue
		}
		if got := postBody(t, r.Posts[0].Body); !jsonEqual(got, w.body) {
			t.Errorf("%s: posted %v, want %v", name, got, w.body)
		}
	}
	if r := runs["active"]; r.Prevented != 1 || r.Results["active-power"].Text != "Done." || r.Results["active-power"].Class != "result ok" || len(r.URLs) != 2 {
		t.Errorf("active: prevented %d, result %+v, %d data fetches; want the answer shown and a reload", r.Prevented, r.Results["active-power"], len(r.URLs))
	}
}

func TestSenderPageRefusesBadInputBeforePosting(t *testing.T) {
	t.Parallel()
	_, _, body := senderPageBodies(t)
	empty := rawPageData(t, senderServer(t, newTestSender(t, &recordingBus{}, true)), panelSender)
	var noDev struct{ Devices []any }
	decodeJSON(t, empty, &noDev)
	lib, src := senderPageFile(t, "sections.js"), senderPageFile(t, "sender.js")
	scenarios := map[string]senderScenario{}
	for name, v := range map[string]string{"fraction": "1.5", "word": "abc", "blank": "", "exponent": "1e3", "unsafe": "9007199254740993", "hex": "0x10", "plus": "+1"} {
		scenarios[name] = senderScenario{Body: body, Steps: []string{"set:active-power-multiplier=" + v, "submit:active-power"}}
	}
	scenarios["nodevice"] = senderScenario{Body: json.RawMessage(`{"title":"Bus sender","on":false,"devices":[],"limits":{"multiplierMin":-9,"multiplierMax":9,"valueMin":-32768,"valueMax":32767,"rawMax":16384},"sections":[]}`),
		Steps: []string{"submit:connect"}}
	runs := runSenderPage(t, lib, src, scenarios)
	for name := range scenarios {
		r := runs[name]
		res := r.Results["active-power"]
		if name == "nodevice" {
			res = r.Results["connect"]
		}
		if len(r.Posts) != 0 || res == nil || res.Class != "result error" || res.Text == "" {
			t.Errorf("%s: %d posts, result %+v; want nothing sent and an error shown", name, len(r.Posts), res)
		}
	}
	if got := runs["nodevice"].Results["connect"].Text; got != "Pick a device." {
		t.Errorf("no device registered: result %q, want Pick a device.", got)
	}
	if got := runs["fraction"].Results["active-power"].Text; got != "multiplier must be a whole number." {
		t.Errorf("fraction: result %q", got)
	}
}

func TestSenderPageListsDevicesKeepsTheChoiceAndAppliesTheLimits(t *testing.T) {
	t.Parallel()
	_, _, body := senderPageBodies(t)
	lib, src := senderPageFile(t, "sections.js"), senderPageFile(t, "sender.js")
	withDevices := func(devices string) json.RawMessage {
		return json.RawMessage(`{"title":"Bus sender","on":false,"devices":` + devices + `,"limits":{"multiplierMin":-9,"multiplierMax":9,"valueMin":-32768,"valueMax":32767,"rawMax":16384},"sections":[]}`)
	}
	moreDevices := withDevices(`[{"id":"_dev-a","label":"Alpha"},{"id":"_dev-b","label":"Bravo"},{"id":"_dev-c","label":"Charlie"}]`)
	onlyAlpha := withDevices(`[{"id":"_dev-a","label":"Alpha"}]`)
	runs := runSenderPage(t, lib, src, map[string]senderScenario{
		"listed": {Body: body},
		"poll":   {Body: body, Steps: []string{"set:connect-device=_dev-b", "tick"}},
		"change": {Body: body, Body2: moreDevices, Steps: []string{"set:connect-device=_dev-b", "swap", "tick"}},
		"gone":   {Body: body, Body2: onlyAlpha, Steps: []string{"set:connect-device=_dev-b", "swap", "tick"}},
		"none":   {Body: json.RawMessage(`{"title":"Bus sender","on":false,"devices":[],"limits":{"multiplierMin":-9,"multiplierMax":9,"valueMin":-32768,"valueMax":32767,"rawMax":16384},"sections":[]}`)},
	})
	l := runs["listed"]
	sel := l.Selects["connect-device"]
	if len(sel.Options) != 2 || sel.Options[0].Value != "_dev-a" || sel.Options[0].Text != "Alpha" || sel.Options[1].Value != "_dev-b" || sel.Options[1].Text != "Bravo" || sel.Value != "_dev-a" || sel.Disabled {
		t.Errorf("device list = %+v", sel)
	}
	for _, id := range []string{"active-power-device", "reactive-power-device", "connect-device", "energize-device"} {
		if len(l.Selects[id].Options) != 2 {
			t.Errorf("%s has %d options, want 2", id, len(l.Selects[id].Options))
		}
	}
	for _, id := range []string{"active-power", "reactive-power"} {
		if m, v := l.Limits[id+"-multiplier"], l.Limits[id+"-value"]; m.Min != "-9" || m.Max != "9" || v.Min != "-32768" || v.Max != "32767" {
			t.Errorf("%s limits: multiplier %+v, value %+v", id, m, v)
		}
	}
	if p := runs["poll"].Selects["connect-device"]; p.Value != "_dev-b" || p.Rebuilds != 1 {
		t.Errorf("after an unchanged poll: chosen device %q, list built %d times; want _dev-b kept and no rebuild", p.Value, p.Rebuilds)
	}
	if c := runs["change"].Selects["connect-device"]; c.Value != "_dev-b" || len(c.Options) != 3 || c.Rebuilds != 2 {
		t.Errorf("after a device was added: chosen %q, %d options, built %d times; want _dev-b kept in the new list", c.Value, len(c.Options), c.Rebuilds)
	}
	if g := runs["gone"].Selects["connect-device"]; g.Value != "_dev-a" || len(g.Options) != 1 {
		t.Errorf("after the chosen device left: %+v, want the list's first device", g)
	}
	if n := runs["none"].Selects["connect-device"]; !n.Disabled || len(n.Options) != 1 || n.Options[0].Text != "No devices registered" || n.Value != "" {
		t.Errorf("no devices: %+v, want a disabled list that says so", n)
	}
}

// TestSenderPageShowsTheSwitchAndTheTablesFromTheData: the sections the
// server sends are drawn as they came.
func TestSenderPageShowsTheSwitchAndTheTablesFromTheData(t *testing.T) {
	t.Parallel()
	bus := &recordingBus{}
	snd := newTestSender(t, bus, true)
	s := senderServer(t, snd)
	postAction(t, s.Handler(), "connect", `{"device":"_dev-a","connect":true}`)
	postAction(t, s.Handler(), "raw", `{"json":"{}"}`)
	lib, src := senderPageFile(t, "sections.js"), senderPageFile(t, "sender.js")
	r := runSenderPage(t, lib, src, map[string]senderScenario{"page": {Body: rawPageData(t, s, panelSender)}})["page"]

	if len(r.Sections) != 3 || r.Sections[0].Heading != "Publishing switch" || r.Sections[1].Heading != "Recent sends" || r.Sections[2].Heading != "Refusals" {
		t.Fatalf("sections = %+v", r.Sections)
	}
	if !slices.Equal(r.Sections[1].Columns, []string{"Time", "Kind", "From", "difference_mrid", "Changes", "Outcome", "Reason"}) || len(r.Sections[1].Rows) != 1 {
		t.Fatalf("recent = %+v", r.Sections[1])
	}
	row := r.Sections[1].Rows[0]
	if row[1].Text != "form" || row[2].Text != "127.0.0.1:40000" || !strings.Contains(row[4].Text, "_dev-a DERControl.DERControlBase.opModConnect=true") || row[5].Text != "pending" || row[5].Class != "badge badge-warn" || row[0].DateTime == "" {
		t.Errorf("recent row = %+v", row)
	}
	if rows := r.Sections[2].Rows; len(rows) != 1 || rows[0][1].Text != "raw" {
		t.Errorf("refusals = %+v, want the one empty raw body", rows)
	}
	if e := r.Sections[0].Groups[0].Entries[0]; e.Value.Text != "ON" || e.Value.Class != "badge badge-warn" {
		t.Errorf("switch badge = %+v", e)
	}
}

const hostileSender = `{"title":"<b>t</b>","on":true,"devices":[{"id":"_d","label":"<img src=x onerror=alert(1)>"}],
"limits":{"multiplierMin":-9,"multiplierMax":9,"valueMin":-32768,"valueMax":32767,"rawMax":16384},
"sections":[{"kind":"table","heading":"<u>h</u>","prose":["<svg onload=alert(1)>"],"empty":"","body":{"columns":["<i>c</i>"],"rows":[[{"kind":"badge","text":"<b>x</b>","badge":"evil\"onclick=\"x"}]]}}]}`

func TestSenderPageWritesServerStringsAsText(t *testing.T) {
	t.Parallel()
	lib, src := senderPageFile(t, "sections.js"), senderPageFile(t, "sender.js")
	r := runSenderPage(t, lib, src, map[string]senderScenario{"hostile": {
		Body:  json.RawMessage(hostileSender),
		Post:  &senderPost{Status: 422, Body: json.RawMessage(`{"error":"<script>alert(1)</script>"}`)},
		Steps: []string{"submit:connect"},
	}})["hostile"]
	if r.InnerHTML != 0 || r.Style != 0 {
		t.Errorf("innerHTML writes %d, style writes %d, want none", r.InnerHTML, r.Style)
	}
	if r.Title != "<b>t</b>" || r.Selects["connect-device"].Options[0].Text != "<img src=x onerror=alert(1)>" || r.Sections[0].Heading != "<u>h</u>" ||
		r.Sections[0].Rows[0][0].Text != "<b>x</b>" || r.Sections[0].Rows[0][0].Class != "badge" {
		t.Errorf("hostile page = title %q, option %+v, section %+v; want raw strings as text and an unknown badge left as a plain badge", r.Title, r.Selects["connect-device"], r.Sections[0])
	}
	if got := r.Results["connect"].Text; got != "<script>alert(1)</script>" {
		t.Errorf("refusal text = %q, want it as text", got)
	}
}

func TestSenderPageDoubleSubmitSendsOnceAndPollsOnlyWhileVisible(t *testing.T) {
	t.Parallel()
	_, _, body := senderPageBodies(t)
	lib, src := senderPageFile(t, "sections.js"), senderPageFile(t, "sender.js")
	runs := runSenderPage(t, lib, src, map[string]senderScenario{
		"twice":    {Body: body, HoldPost: true, Steps: []string{"submit:energize", "submit:energize"}},
		"other":    {Body: body, HoldPost: true, Steps: []string{"submit:energize", "submit:connect"}},
		"released": {Body: body, HoldPost: true, Steps: []string{"submit:energize", "release"}},
		"again":    {Body: body, HoldPost: true, Steps: []string{"submit:energize", "release", "submit:energize"}},
	})
	tw := runs["twice"]
	if len(tw.Posts) != 1 || !tw.Results["energize:submit"].Disabled {
		t.Errorf("twice: %d posts, button disabled %t; want one post and the button off while it is out", len(tw.Posts), tw.Results["energize:submit"].Disabled)
	}
	if len(runs["other"].Posts) != 2 {
		t.Errorf("another form while one is out: %d posts, want 2", len(runs["other"].Posts))
	}
	if r := runs["released"]; len(r.Posts) != 1 || r.Results["energize:submit"].Disabled || r.Results["energize"].Text != "Done." {
		t.Errorf("after the answer: %d posts, button disabled %t, result %q; want the form usable again", len(r.Posts), r.Results["energize:submit"].Disabled, r.Results["energize"].Text)
	}
	if len(runs["again"].Posts) != 2 {
		t.Errorf("a second send after the first was answered: %d posts, want 2", len(runs["again"].Posts))
	}
	poll := runSenderPage(t, lib, src, switchPollScenarios(body))
	if h := poll["hideLater"]; len(h.Steps) != 4 || h.Steps[1].Fetches != 1 || h.Steps[2].Fetches != 2 {
		t.Errorf("hide, tick, show: %+v", h.Steps)
	}
	if hs := poll["hiddenStart"]; hs.Steps[0].Fetches != 0 || hs.Steps[1].Fetches != 1 {
		t.Errorf("hidden start: %+v", hs.Steps)
	}
}

func TestSenderPagesServeTheirFilesUnderThePageCSP(t *testing.T) {
	t.Parallel()
	s := senderServer(t, newTestSender(t, &recordingBus{}, false))
	for _, panel := range []string{panelPublishing, panelSender} {
		page := map[string]string{panelPublishing: "switch", panelSender: "sender"}[panel]
		for _, f := range []struct{ path, ctype, marker string }{
			{"", "text/html", `src="` + page + `.js"`},
			{page + ".js", "text/javascript", "BridgeSections"},
			{"sections.js", "text/javascript", "textContent"},
			{"style.css", "text/css", "body"},
		} {
			rec := doRequest(t, s.Handler(), "GET", senderPath(panel, f.path), "Bearer "+testKey, "localhost")
			if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), f.ctype) || !strings.Contains(rec.Body.String(), f.marker) {
				t.Errorf("%s GET %q: status %d, Content-Type %q, marker %q present %t", panel, f.path, rec.Code, rec.Header().Get("Content-Type"), f.marker, strings.Contains(rec.Body.String(), f.marker))
			}
			csp := strings.Join(rec.Header().Values("Content-Security-Policy"), "; ")
			for _, want := range []string{"script-src 'self'", "style-src 'self'", "connect-src 'self'", "form-action 'none'", "frame-ancestors 'self'"} {
				if !strings.Contains(csp, want) {
					t.Errorf("%s GET %q: CSP %q lacks %q", panel, f.path, csp, want)
				}
			}
		}
	}
}

// TestSenderPageMutantsAreCaught proves the page tests can fail: each mutant
// breaks one property of the scripts, and the check that guards it must see
// it.
func TestSenderPageMutantsAreCaught(t *testing.T) {
	t.Parallel()
	off, on, body := senderPageBodies(t)
	lib := senderPageFile(t, "sections.js")
	sender := senderPageFile(t, "sender.js")
	swtch := senderPageFile(t, "switch.js")
	mutate := func(src, name, old, repl string) string {
		if strings.Count(src, old) != 1 {
			t.Fatalf("mutant target %q occurs %d times in %s, want 1", old, strings.Count(src, old), name)
		}
		return strings.Replace(src, old, repl, 1)
	}
	active := map[string]senderScenario{"a": {Body: body, Steps: []string{"set:active-power-multiplier=2", "set:active-power-value=-150", "submit:active-power"}}}
	hostile := map[string]senderScenario{"h": {Body: json.RawMessage(hostileSender)}}

	t.Run("posts a number as a string", func(t *testing.T) {
		t.Parallel()
		r := runSenderPage(t, lib, mutate(sender, "sender.js", "body[name] = n;", "body[name] = text;"), active)["a"]
		if got := postBody(t, r.Posts[0].Body); got["multiplier"] == "2" {
			return
		}
		t.Error("the typed-body check passed against a page that posts numbers as strings")
	})
	t.Run("posts without the JSON content type", func(t *testing.T) {
		t.Parallel()
		r := runSenderPage(t, mutate(lib, "sections.js", `headers: { "Content-Type": "application/json" },`, `headers: {},`), sender, active)["a"]
		if r.Posts[0].ContentType == "application/json" {
			t.Error("the content-type check passed against a page that sends no type")
		}
	})
	t.Run("allows a second submit while one is out", func(t *testing.T) {
		t.Parallel()
		r := runSenderPage(t, lib, mutate(sender, "sender.js", "if (submitting.has(form.id)) return;", ""), map[string]senderScenario{"t": {Body: body, HoldPost: true, Steps: []string{"submit:energize", "submit:energize"}}})["t"]
		if len(r.Posts) == 1 {
			t.Error("the double-submit check passed against a page that sends twice")
		}
	})
	t.Run("posts a value that is not a whole number", func(t *testing.T) {
		t.Parallel()
		mutant := mutate(sender, "sender.js", `if (!/^-?[0-9]+$/.test(text) || !Number.isSafeInteger(n)) return`, `if (false) return`)
		r := runSenderPage(t, lib, mutant, map[string]senderScenario{"b": {Body: body, Steps: []string{"set:active-power-multiplier=abc", "submit:active-power"}}})["b"]
		if len(r.Posts) == 0 {
			t.Error("the whole-number check passed against a page that posts anything")
		}
	})
	t.Run("writes markup with innerHTML", func(t *testing.T) {
		t.Parallel()
		r := runSenderPage(t, mutate(lib, "sections.js", "e.textContent = text;", "e.innerHTML = text;"), sender, hostile)["h"]
		if r.InnerHTML == 0 {
			t.Error("the text-only check passed against a page that writes innerHTML")
		}
	})
	t.Run("polls while hidden", func(t *testing.T) {
		t.Parallel()
		r := runSenderPage(t, lib, mutate(sender, "sender.js", "if (document.hidden) return;", ""), switchPollScenarios(body))
		if r["hideLater"].Steps[1].Fetches != 1 || r["hiddenStart"].Steps[0].Fetches != 0 {
			return
		}
		t.Error("the polling check passed against a page that ignores document.hidden")
	})
	t.Run("rebuilds the device list on every poll", func(t *testing.T) {
		t.Parallel()
		r := runSenderPage(t, lib, mutate(sender, "sender.js", "if (sig === devicesSig) return;", ""), map[string]senderScenario{"p": {Body: body, Steps: []string{"tick"}}})["p"]
		if r.Selects["connect-device"].Rebuilds == 1 {
			t.Error("the no-rebuild check passed against a page that rebuilds the list each poll")
		}
	})
	t.Run("forgets the device choice when the list changes", func(t *testing.T) {
		t.Parallel()
		more := json.RawMessage(`{"title":"Bus sender","on":false,"devices":[{"id":"_dev-a","label":"Alpha"},{"id":"_dev-b","label":"Bravo"},{"id":"_dev-c","label":"Charlie"}],"limits":{"multiplierMin":-9,"multiplierMax":9,"valueMin":-32768,"valueMax":32767,"rawMax":16384},"sections":[]}`)
		mutant := mutate(sender, "sender.js", "if (devices.some((d) => d.id === kept)) select.value = kept;", "")
		r := runSenderPage(t, lib, mutant, map[string]senderScenario{"c": {Body: body, Body2: more, Steps: []string{"set:connect-device=_dev-b", "swap", "tick"}}})["c"]
		if r.Selects["connect-device"].Value == "_dev-b" {
			t.Error("the kept-choice check passed against a page that never restores the choice")
		}
	})
	t.Run("does not reload after a send", func(t *testing.T) {
		t.Parallel()
		r := runSenderPage(t, lib, mutate(sender, "sender.js", "submitting.delete(form.id);\n    reload();", "submitting.delete(form.id);"), active)["a"]
		if len(r.URLs) == 2 {
			t.Error("the reload check passed against a page that never reloads")
		}
	})
	t.Run("calls AbortSignal.timeout without a fallback", func(t *testing.T) {
		t.Parallel()
		mutant := mutate(lib, "sections.js", `if (typeof AbortSignal !== "undefined" && typeof AbortSignal.timeout === "function") {`, `if (true) {`)
		if old := runSenderPage(t, mutant, sender, map[string]senderScenario{"o": {Body: body, NoTimeout: true}})["o"]; strings.HasPrefix(old.Status, "updated ") {
			t.Error("the fallback check passed against a page that always calls AbortSignal.timeout")
		}
	})
	t.Run("switch sends the state it read", func(t *testing.T) {
		t.Parallel()
		r := runSenderPage(t, lib, mutate(swtch, "switch.js", "{ on: !on }", "{ on: on }"), map[string]senderScenario{"o": {Body: off, Steps: []string{"submit:switch"}}})["o"]
		if jsonEqual(postBody(t, r.Posts[0].Body), map[string]any{"on": true}) {
			t.Error("the opposite-state check passed against a page that repeats the state")
		}
	})
	t.Run("switch button is live before the first read", func(t *testing.T) {
		t.Parallel()
		r := runSenderPage(t, lib, mutate(swtch, "switch.js", "b.disabled = busy || on === null;", "b.disabled = busy;"), map[string]senderScenario{"n": {Status: 500, Body: json.RawMessage(`{}`)}})["n"]
		if r.Toggle.Disabled {
			t.Error("the no-data check passed against a button that is live without a state")
		}
	})
	t.Run("switch posts again while one is out", func(t *testing.T) {
		t.Parallel()
		r := runSenderPage(t, lib, mutate(swtch, "switch.js", "if (busy || on === null) return;", "if (on === null) return;"), map[string]senderScenario{"t": {Body: on, HoldPost: true, Steps: []string{"submit:switch", "submit:switch"}}})["t"]
		if len(r.Posts) == 1 {
			t.Error("the switch double-submit check passed against a page that sends twice")
		}
	})
}
