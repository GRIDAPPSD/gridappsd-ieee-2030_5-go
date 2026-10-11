package adminui

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/socpage"
)

type pageCard struct {
	ClassName  string   `json:"className"`
	AriaLabel  string   `json:"ariaLabel"`
	Texts      []string `json:"texts"`
	HiddenText []string `json:"hiddenTexts"`
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

func wantTexts(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s visible text = %q, want %q", what, got, want)
	}
}

// A card shows the reading time as a timestamp, never an age or a countdown.
func assertNoAgo(t *testing.T, i int, v pageCard) {
	t.Helper()
	var all []string
	all = append(all, v.Texts...)
	all = append(all, v.HiddenText...)
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
	// The visible text is the title, the kind, the unlabeled tag, the state of
	// charge line and the watts in force; no other status row.
	wantTexts(t, "charging card", charging.Texts, []string{"pv_a", "battery", "unlabeled", "42.5% at 1970-01-01 00:16:35 UTC", "-1500 W"})
	if charging.Stop.Hidden {
		t.Errorf("charging card hides Stop: %+v", charging)
	}
	if charging.AriaLabel != "pv_a, CHARGING" {
		t.Errorf("charging aria-label = %q, want the stripe label kept for readers", charging.AriaLabel)
	}

	// No report and no control: only the no-report text shows, and the watts
	// line and Stop are hidden.
	noReport := first.Views[1]
	if noReport.ClassName != "card c-grey" {
		t.Errorf("no-report card class = %q, want grey", noReport.ClassName)
	}
	wantTexts(t, "no-report card", noReport.Texts, []string{"tl_b", "device", "unlabeled", "no report yet"})
	if !noReport.Stop.Hidden {
		t.Errorf("no-report card shows Stop: %+v", noReport)
	}

	// A stripe value outside the known five never becomes a class name.
	if first.Views[2].ClassName != "card c-grey" {
		t.Errorf("unknown stripe class = %q, want card c-grey", first.Views[2].ClassName)
	}

	discharge := first.Views[3]
	if discharge.ClassName != "card c-green" {
		t.Errorf("discharge card class = %q, want card c-green", discharge.ClassName)
	}
	if got := discharge.Texts[len(discharge.Texts)-2:]; !slices.Equal(got, []string{"80% at 1970-01-01 00:16:39 UTC", "+800 W"}) {
		t.Errorf("discharge card lines = %q, want the state of charge and +800 W", got)
	}

	hold := first.Views[4]
	if hold.ClassName != "card c-pink" {
		t.Errorf("hold card class = %q, want card c-pink", hold.ClassName)
	}
	if got := hold.Texts[len(hold.Texts)-1]; got != "0 W" {
		t.Errorf("hold watts line = %q, want 0 W", got)
	}

	stale := first.Views[5]
	if stale.ClassName != "card c-yellow" || !stale.Stop.Hidden {
		t.Errorf("stale card = %q stop hidden %v, want yellow with no Stop", stale.ClassName, stale.Stop.Hidden)
	}
	if got := stale.Texts[len(stale.Texts)-1]; got != "10% at 1970-01-01 00:13:20 UTC" {
		t.Errorf("stale card last line = %q, want its state of charge line and no watts", got)
	}

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
	if got := values.Views[0].Texts[3]; got != "50% at 1970-01-01 00:16:40 UTC" {
		t.Errorf("updated state of charge line = %q, want 50%% at 1970-01-01 00:16:40 UTC", got)
	}

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

// A status object that lacks every field reads as no report, and an empty
// device list shows the empty-state text.
func TestCardsPageSparseAndEmpty(t *testing.T) {
	t.Parallel()
	run := runCardsPage(t)
	sparse := pollNamed(t, run, "sparse")
	if len(sparse.Views) != 1 {
		t.Fatalf("sparse poll views = %+v, want one card", sparse.Views)
	}
	if got := sparse.Views[0].Texts[len(sparse.Views[0].Texts)-2]; got != "no report yet" {
		t.Errorf("sparse card state of charge line = %q, want no report yet", got)
	}
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
	if got := zero.Views[0].Texts[len(zero.Views[0].Texts)-2]; got != "42.5%" {
		t.Errorf("state of charge line for readingTime 0 = %q, want 42.5%% with no epoch time", got)
	}
}

// A connected battery card shows exactly its label, the state of charge line,
// and the signed watts line only while a control is in force.
func TestCardsPageBatteryCardsShowOnlySoC(t *testing.T) {
	t.Parallel()
	p := pollNamed(t, runCardsPage(t), "batteries")
	if len(p.Views) != 3 {
		t.Fatalf("battery cards = %d, want 3", len(p.Views))
	}
	wantTexts(t, "battery with control", p.Views[0].Texts, []string{"battery1", "42.5% at 1970-01-01 00:16:35 UTC", "-1500 W"})
	wantTexts(t, "battery without control", p.Views[1].Texts, []string{"battery2", "10% at 1970-01-01 00:13:20 UTC"})
	wantTexts(t, "battery discharging", p.Views[2].Texts, []string{"battery3", "80% at 1970-01-01 00:16:39 UTC", "+800 W"})
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
