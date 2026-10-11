package adminui

import (
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/socpage"
)

type pageList struct {
	Hidden bool              `json:"hidden"`
	Fields map[string]string `json:"fields"`
}

type pageCard struct {
	ClassName  string     `json:"className"`
	AriaLabel  string     `json:"ariaLabel"`
	Texts      []string   `json:"texts"`
	HiddenText []string   `json:"hiddenTexts"`
	Lists      []pageList `json:"lists"`
	Stop       struct {
		Hidden   bool `json:"hidden"`
		Disabled bool `json:"disabled"`
	} `json:"stop"`
}

type pageStatus struct {
	Text  string `json:"text"`
	Error bool   `json:"error"`
}

type pagePoll struct {
	Name       string     `json:"name"`
	Replaced   int        `json:"replaced"`
	Order      []string   `json:"order"`
	Views      []pageCard `json:"views"`
	FirstIndex []int      `json:"firstIndex"`
	Empty      bool       `json:"empty"`
	Status     pageStatus `json:"status"`
}

type pageStop struct {
	Label string `json:"label"`
	Posts []struct {
		URL  string         `json:"url"`
		Body map[string]any `json:"body"`
	} `json:"posts"`
	Error struct {
		Text   string `json:"text"`
		Hidden bool   `json:"hidden"`
	} `json:"error"`
	Disabled bool `json:"disabled"`
}

type pageSend struct {
	Input string `json:"input"`
	Posts []struct {
		URL  string         `json:"url"`
		Body map[string]any `json:"body"`
	} `json:"posts"`
	Error struct {
		Text   string `json:"text"`
		Hidden bool   `json:"hidden"`
	} `json:"error"`
}

type pageRun struct {
	Polls []pagePoll `json:"polls"`
	Stops []pageStop `json:"stops"`
	Sends []pageSend `json:"sends"`
	Alls  []pageAll  `json:"alls"`
}

type pageAll struct {
	Label string `json:"label"`
	Posts []struct {
		URL  string         `json:"url"`
		Body map[string]any `json:"body"`
	} `json:"posts"`
	Result struct {
		Text   string `json:"text"`
		Hidden bool   `json:"hidden"`
		Warn   bool   `json:"warn"`
	} `json:"result"`
	Error struct {
		Text   string `json:"text"`
		Hidden bool   `json:"hidden"`
	} `json:"error"`
}

// runCardsPage runs the shipped app.js against a fake DOM and fake fetch
// (testdata/cards_page.js). A missing node skips the test locally and fails it
// under CI, where a skip would hide that the page was never exercised.
func runCardsPage(t *testing.T) pageRun {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("node not installed, and CI must run the page test: %v", err)
		}
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "testdata/cards_page.js", "socpage/app.js", "testdata/cards_fixture.json").CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var run pageRun
	if err := json.Unmarshal(out, &run); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return run
}

func pollNamed(t *testing.T, run pageRun, name string) pagePoll {
	t.Helper()
	for _, p := range run.Polls {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no poll %q", name)
	return pagePoll{}
}

func wantFields(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if !maps.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// A card shows the reading time as a timestamp, never an age or a countdown.
func assertNoAgo(t *testing.T, i int, v pageCard) {
	t.Helper()
	var all []string
	all = append(all, v.Texts...)
	all = append(all, v.HiddenText...)
	for _, l := range v.Lists {
		for k, val := range l.Fields {
			all = append(all, k, val)
		}
	}
	for _, s := range all {
		if strings.Contains(s, " ago") || strings.Contains(s, "last request") || s == "Time left" || s == "Reported" {
			t.Errorf("card %d shows age or countdown text %q", i, s)
		}
	}
}

func TestCardsPageRenders(t *testing.T) {
	t.Parallel()
	first := pollNamed(t, runCardsPage(t), "initial")
	if len(first.Views) != 6 {
		t.Fatalf("cards = %d, want 6", len(first.Views))
	}
	if !first.Empty {
		t.Error("empty-state text shown with six cards")
	}
	if first.Status.Error || !regexp.MustCompile(`^devices: 6, updated [^.]+$`).MatchString(first.Status.Text) {
		t.Errorf("summary = %+v, want devices: 6 with no warning", first.Status)
	}

	charging := first.Views[0]
	if charging.ClassName != "card c-blue" {
		t.Errorf("charging card class = %q, want %q", charging.ClassName, "card c-blue")
	}
	for _, want := range []string{"pv_a", "battery", "CHARGING"} {
		if !slices.Contains(charging.Texts, want) {
			t.Errorf("charging card lacks %q: %q", want, charging.Texts)
		}
	}
	wantFields(t, "charging status", charging.Lists[0].Fields, map[string]string{
		"State of charge": "42.5%", "Inverter": "Tracking MPPT", "Operational mode": "Operational",
		"Storage mode": "Charging", "Generator connection": "Connected", "Storage connection": "Connected, Operating",
		"Alarm": "0", "Reading time": "1970-01-01 00:16:35 UTC",
	})
	wantFields(t, "charging control", charging.Lists[1].Fields, map[string]string{
		"Control": "Charge 1500 W", "Source": "page", "Received by the device": "yes",
	})
	if charging.Lists[0].Hidden || charging.Lists[1].Hidden || charging.Stop.Hidden {
		t.Errorf("charging card hides its lists or Stop: %+v", charging)
	}
	if slices.Contains(charging.Texts, "no report yet") || !slices.Contains(charging.HiddenText, "no report yet") {
		t.Errorf("charging card shows no-report text: shown %q hidden %q", charging.Texts, charging.HiddenText)
	}

	// No report and no control: both lists, their "-" rows and Stop are hidden
	// by the card, and only the no-report text shows.
	noReport := first.Views[1]
	if noReport.ClassName != "card c-grey" || !slices.Contains(noReport.Texts, "NOT CONNECTED") {
		t.Errorf("no-report card = %q %q, want grey NOT CONNECTED", noReport.ClassName, noReport.Texts)
	}
	if !slices.Contains(noReport.Texts, "no report yet") {
		t.Errorf("no-report card lacks no-report text: %q", noReport.Texts)
	}
	if !noReport.Lists[0].Hidden || !noReport.Lists[1].Hidden || !noReport.Stop.Hidden {
		t.Errorf("no-report card shows a list or Stop: %+v", noReport)
	}
	if slices.Contains(noReport.Texts, "-") {
		t.Errorf("no-report card shows a placeholder row: %q", noReport.Texts)
	}
	if !slices.Contains(noReport.HiddenText, "-") {
		t.Errorf("no-report card has no hidden placeholder rows: %q", noReport.HiddenText)
	}

	// A stripe value outside the known five never becomes a class name.
	if first.Views[2].ClassName != "card c-grey" {
		t.Errorf("unknown stripe class = %q, want card c-grey", first.Views[2].ClassName)
	}

	discharge := first.Views[3]
	if discharge.ClassName != "card c-green" {
		t.Errorf("discharge card class = %q, want card c-green", discharge.ClassName)
	}
	wantFields(t, "discharge control", discharge.Lists[1].Fields, map[string]string{
		"Control": "Discharge 800 W", "Source": "server", "Received by the device": "not yet",
	})
	wantFields(t, "discharge status", discharge.Lists[0].Fields, map[string]string{
		"State of charge": "80%", "Inverter": "Idle", "Operational mode": "Off",
		"Storage mode": "Discharging", "Generator connection": "Disconnected", "Storage connection": "Connected",
		"Alarm": "1", "Reading time": "1970-01-01 00:16:39 UTC",
	})

	hold := first.Views[4]
	wantFields(t, "hold control", hold.Lists[1].Fields, map[string]string{
		"Control": "0 W (hold)", "Source": "page", "Received by the device": "yes",
	})
	if hold.ClassName != "card c-pink" {
		t.Errorf("hold card class = %q, want card c-pink", hold.ClassName)
	}
	if hold.Lists[1].Hidden || !hold.Lists[0].Hidden {
		t.Errorf("hold card lists hidden = %v %v, want control shown and status hidden", hold.Lists[0].Hidden, hold.Lists[1].Hidden)
	}

	stale := first.Views[5]
	if stale.ClassName != "card c-yellow" || !stale.Lists[1].Hidden || !stale.Stop.Hidden || stale.Lists[0].Hidden {
		t.Errorf("stale card = %q lists %+v stop hidden %v, want yellow with status only", stale.ClassName, stale.Lists, stale.Stop.Hidden)
	}
	wantFields(t, "stale status", stale.Lists[0].Fields, map[string]string{
		"State of charge": "10%", "Inverter": "Fault", "Operational mode": "Operational",
		"Storage mode": "Holding", "Generator connection": "Connected", "Storage connection": "Disconnected",
		"Alarm": "2", "Reading time": "1970-01-01 00:13:20 UTC",
	})

	for i, v := range first.Views {
		assertNoAgo(t, i, v)
	}
}

// The page polls pollCards itself: values update in place, a device that
// disappears or moves keeps the elements of the cards that stay, and the
// summary line carries each warning.
func TestCardsPagePolls(t *testing.T) {
	t.Parallel()
	run := runCardsPage(t)

	values := pollNamed(t, run, "values")
	if !slices.Equal(values.FirstIndex, []int{0, 1, 2, 3, 4, 5}) {
		t.Errorf("same-order poll element indexes = %v, want the six first elements", values.FirstIndex)
	}
	wantFields(t, "updated status", map[string]string{
		"State of charge": values.Views[0].Lists[0].Fields["State of charge"],
		"Reading time":    values.Views[0].Lists[0].Fields["Reading time"],
	}, map[string]string{"State of charge": "50%", "Reading time": "1970-01-01 00:16:40 UTC"})

	// An unchanged order leaves the list alone, so a focused Stop survives.
	if initial := pollNamed(t, run, "initial"); values.Replaced != initial.Replaced {
		t.Errorf("same-order poll replaced the list children (%d -> %d)", initial.Replaced, values.Replaced)
	}

	reorder := pollNamed(t, run, "reorder")
	if reorder.Replaced != values.Replaced+1 {
		t.Errorf("reorder replaced the list %d times, want one more than %d", reorder.Replaced, values.Replaced)
	}
	wantOrder := []string{"pv_f, STALE", "pv_a, CHARGING", "tl_c, CHARGE TO DISCHARGE", "bat_d, DISCHARGING", "hold_e, HOLDING"}
	if !slices.Equal(reorder.Order, wantOrder) {
		t.Errorf("order after removing tl_b and moving pv_f = %q, want %q", reorder.Order, wantOrder)
	}
	if !slices.Equal(reorder.FirstIndex, []int{5, 0, 2, 3, 4}) {
		t.Errorf("element indexes after reorder = %v, want [5 0 2 3 4]", reorder.FirstIndex)
	}
	if !strings.HasPrefix(reorder.Status.Text, "devices: 5, ") {
		t.Errorf("summary after reorder = %q, want devices: 5", reorder.Status.Text)
	}

	warn := regexp.MustCompile(`^devices: 5, updated [^.]+\. Showing 5 of 9\. 2 devices could not be read\. DERStatus could not be read$`)
	if w := pollNamed(t, run, "warnings").Status; !w.Error || !warn.MatchString(w.Text) {
		t.Errorf("warning summary = %+v, want all three warnings and the error flag", w)
	}
	if b := pollNamed(t, run, "busy").Status; !b.Error || b.Text != "cards: server busy, retrying" {
		t.Errorf("503 status = %+v", b)
	}
	if h := pollNamed(t, run, "http500").Status; !h.Error || h.Text != "cards: HTTP 500" {
		t.Errorf("500 status = %+v", h)
	}
	for _, name := range []string{"warnTruncated", "warnSkipped", "warnUnavailable"} {
		if w := pollNamed(t, run, name).Status; !w.Error || !strings.HasPrefix(w.Text, "devices: 5, ") || !strings.Contains(w.Text, ". ") {
			t.Errorf("%s status = %+v, want the error flag", name, w)
		}
	}
	if f := pollNamed(t, run, "fetchFail").Status; !f.Error || f.Text != "cards: offline" {
		t.Errorf("failed fetch status = %+v, want cards: offline with the error flag", f)
	}
	if r := pollNamed(t, run, "recovered").Status; r.Error || !strings.HasPrefix(r.Text, "devices: 5, ") {
		t.Errorf("recovered status = %+v, want no error flag", r)
	}
}

// A status object that lacks every field shows "-" for each, and an empty
// device list shows the empty-state text.
func TestCardsPageSparseAndEmpty(t *testing.T) {
	t.Parallel()
	run := runCardsPage(t)
	sparse := pollNamed(t, run, "sparse")
	if len(sparse.Views) != 1 || sparse.Views[0].Lists[0].Hidden {
		t.Fatalf("sparse poll views = %+v, want one card with its status list shown", sparse.Views)
	}
	wantFields(t, "sparse status", sparse.Views[0].Lists[0].Fields, map[string]string{
		"State of charge": "-", "Inverter": "-", "Operational mode": "-",
		"Storage mode": "-", "Generator connection": "-", "Storage connection": "-",
		"Alarm": "-", "Reading time": "-",
	})
	none := pollNamed(t, run, "none")
	if len(none.Views) != 0 {
		t.Errorf("empty poll left %d cards", len(none.Views))
	}
	// Empty reports the empty-state text as hidden.
	if none.Empty {
		t.Error("empty-state text hidden with zero cards")
	}
	if !strings.HasPrefix(none.Status.Text, "devices: 0, ") || none.Status.Error {
		t.Errorf("empty summary = %+v, want devices: 0 with no warning", none.Status)
	}
}

// A reading time of 0 is "sent none", not the epoch.
func TestCardsPageZeroReadingTime(t *testing.T) {
	t.Parallel()
	zero := pollNamed(t, runCardsPage(t), "zeroTime")
	if len(zero.Views) != 1 {
		t.Fatalf("zeroTime cards = %d, want 1", len(zero.Views))
	}
	if got := zero.Views[0].Lists[0].Fields["Reading time"]; got != "-" {
		t.Errorf("Reading time for readingTime 0 = %q, want -", got)
	}
}

// A Stop posts the card's own device; a failed Stop shows its error and
// re-enables the button, and a later good Stop clears the error.
func TestCardsPageStop(t *testing.T) {
	t.Parallel()
	run := runCardsPage(t)
	if len(run.Stops) != 3 {
		t.Fatalf("stops = %d, want 3", len(run.Stops))
	}
	for i, tc := range []struct {
		label, mrid, errText string
		errHidden            bool
	}{
		{"Stop pv_a", "M-A", "Stop failed: HTTP 500: boom (retry after 7 s)", false},
		{"Stop pv_a", "M-A", "No answer to the Stop (offline). Whether it was applied is unknown; press Stop again to retry.", false},
		{"Stop bat_d", "M-D", "", true},
	} {
		s := run.Stops[i]
		if s.Label != tc.label || len(s.Posts) != 1 {
			t.Fatalf("stop %d = %q with %d posts, want %q with 1", i, s.Label, len(s.Posts), tc.label)
		}
		p := s.Posts[0]
		if p.URL != "/apps/soc/api/control" || p.Body["mrid"] != tc.mrid || p.Body["stop"] != true || len(p.Body) != 2 {
			t.Errorf("stop %d posted %s %v, want /apps/soc/api/control {mrid:%s, stop:true}", i, p.URL, p.Body, tc.mrid)
		}
		if s.Error.Text != tc.errText || s.Error.Hidden != tc.errHidden {
			t.Errorf("stop %d error = %+v, want text %q hidden %v", i, s.Error, tc.errText, tc.errHidden)
		}
		if s.Disabled {
			t.Errorf("stop %d left the button disabled", i)
		}
	}
}

// Server-supplied text reaches the page only through textContent, and the
// stripe colours are the five of the log UI.
func TestCardsPageAssets(t *testing.T) {
	t.Parallel()
	read := func(name string) string {
		b, err := fs.ReadFile(socpage.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	js, css, html := read("app.js"), read("style.css"), read("index.html")
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("} {
		if strings.Contains(js, banned) {
			t.Errorf("app.js contains %q", banned)
		}
	}
	for _, want := range []string{
		".c-grey { border-right-color: #8b949e; }", ".c-green { border-right-color: #3fb950; }",
		".c-blue { border-right-color: #388bfd; }", ".c-yellow { border-right-color: #e3b341; }",
		".c-pink { border-right-color: #f778ba; }",
		// Beats .ctl { display: grid }, so a hidden list stays hidden.
		".card [hidden] { display: none; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css lacks %q", want)
		}
	}
	for _, id := range []string{`id="cards"`, `id="cards-status"`, `id="cards-error"`, `id="cards-empty"`} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html lacks %s", id)
		}
	}
}

// The form takes one signed watts number and posts it as typed: negative
// charges, positive discharges, zero is refused in favor of Stop.
func TestCardsPageSendSignedWatts(t *testing.T) {
	t.Parallel()
	run := runCardsPage(t)
	byInput := map[string]pageSend{}
	for _, s := range run.Sends {
		byInput[s.Input] = s
	}
	for _, tc := range []struct {
		input string
		watts float64
	}{{"-500", -500}, {"500", 500}} {
		s, ok := byInput[tc.input]
		if !ok || len(s.Posts) != 1 {
			t.Fatalf("send %q = %+v, want one post", tc.input, s)
		}
		p := s.Posts[0]
		if p.URL != "/apps/soc/api/control" || p.Body["mrid"] != "M-A" || p.Body["watts"] != tc.watts || p.Body["durationSeconds"] != float64(300) || len(p.Body) != 3 {
			t.Errorf("send %q posted %s %v, want {mrid:M-A, watts:%v, durationSeconds:300}", tc.input, p.URL, p.Body, tc.watts)
		}
		if !s.Error.Hidden {
			t.Errorf("send %q left an error: %q", tc.input, s.Error.Text)
		}
	}
	for _, bad := range []string{"0", "12.5", ""} {
		s, ok := byInput[bad]
		if !ok || len(s.Posts) != 0 || s.Error.Hidden || !strings.Contains(s.Error.Text, "whole number") {
			t.Errorf("send %q = %+v, want no post and a whole-number error", bad, s)
		}
	}
}

// "All devices" posts the same watts and duration to the all route, with no
// mrid, and says how many devices took it and which did not.
func TestCardsPageSendToAllDevices(t *testing.T) {
	t.Parallel()
	byLabel := map[string]pageAll{}
	for _, a := range runCardsPage(t).Alls {
		byLabel[a.Label] = a
	}
	send := byLabel["send"]
	if len(send.Posts) != 1 || send.Posts[0].URL != "/apps/soc/api/control/all" ||
		send.Posts[0].Body["watts"] != float64(100) || send.Posts[0].Body["durationSeconds"] != float64(300) || len(send.Posts[0].Body) != 2 {
		t.Fatalf("all send posted %+v, want {watts:100, durationSeconds:300} to the all route", send.Posts)
	}
	if send.Result.Hidden || send.Result.Warn || send.Result.Text != "100 W for 300 s sent to 2 of 2 devices." || !send.Error.Hidden {
		t.Errorf("all send result = %+v error = %+v", send.Result, send.Error)
	}
	partial := byLabel["partial"]
	if !partial.Result.Warn || partial.Result.Text != "100 W for 300 s sent to 1 of 2 devices. Failed: pv_b (unknown device)." {
		t.Errorf("partial result = %+v, want the failed device named", partial.Result)
	}
	if h := byLabel["http"]; !h.Result.Hidden || !strings.Contains(h.Error.Text, "HTTP 500") || !strings.Contains(h.Error.Text, "retry after 7") {
		t.Errorf("http failure = %+v", h)
	}
	stop := byLabel["stop"]
	if len(stop.Posts) != 1 || stop.Posts[0].URL != "/apps/soc/api/control/all" || stop.Posts[0].Body["stop"] != true || len(stop.Posts[0].Body) != 1 {
		t.Errorf("stop all posted %+v, want {stop:true} to the all route", stop.Posts)
	}
	if !strings.HasPrefix(stop.Result.Text, "Stop (0 W) sent to 2 of 2 devices.") {
		t.Errorf("stop result = %q", stop.Result.Text)
	}
}

// A labelled card is titled by its label with the CIM name beside it, the
// order the server sent is kept (n2 before n10, n40 before battery1), and only
// the unmapped card shows the unlabeled tag.
func TestCardsPageTitlesByLabelInServerOrder(t *testing.T) {
	t.Parallel()
	p := pollNamed(t, runCardsPage(t), "labels")
	wantOrder := []string{"n2, NOT CONNECTED", "n10, NOT CONNECTED", "n40, NOT CONNECTED",
		"battery1, NOT CONNECTED", "battery9, NOT CONNECTED", "alpha, NOT CONNECTED"}
	if !slices.Equal(p.Order, wantOrder) {
		t.Fatalf("aria order = %q, want %q", p.Order, wantOrder)
	}
	titles := []string{"n2", "n10", "n40", "battery1", "battery9", "alpha"}
	subs := []string{"tl_two", "tl_ten", "tl_forty", "pv_one", "pv_nine", ""}
	for i, v := range p.Views {
		if len(v.Texts) == 0 || v.Texts[0] != titles[i] {
			t.Errorf("card %d texts = %q, want title %q first", i, v.Texts, titles[i])
		}
		battery := i == 3 || i == 4
		if battery {
			if len(v.Texts) == 0 || v.Texts[0] != titles[i] {
				t.Errorf("battery card %d title = %q, want exactly %q", i, v.Texts, titles[i])
			}
			for _, absent := range []string{subs[i], "device"} {
				if slices.Contains(v.Texts, absent) {
					t.Errorf("battery card %d shows %q beside its label: %q", i, absent, v.Texts)
				}
			}
		} else if subs[i] != "" && !slices.Contains(v.Texts, subs[i]) {
			t.Errorf("card %d lacks CIM name subtitle %q: %q", i, subs[i], v.Texts)
		}
		tagged := slices.Contains(v.Texts, "unlabeled")
		if tagged != (i == 5) {
			t.Errorf("card %d unlabeled tag shown = %v, want %v", i, tagged, i == 5)
		}
	}
}
