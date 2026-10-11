package adminui

import (
	"encoding/json"
	"io/fs"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/socpage"
)

type pageCard struct {
	ClassName  string   `json:"className"`
	Texts      []string `json:"texts"`
	HiddenText []string `json:"hiddenTexts"`
	Stop       struct {
		Hidden bool `json:"hidden"`
	} `json:"stop"`
}

type pageRun struct {
	Views     []pageCard `json:"views"`
	Reused    bool       `json:"reused"`
	StopIndex int        `json:"stopIndex"`
	Posts     []struct {
		URL  string         `json:"url"`
		Body map[string]any `json:"body"`
	} `json:"posts"`
}

// TestCardsPageRendersAndStops runs the shipped app.js against a fake DOM and
// fake fetch (testdata/cards_page.js) and reads what each card shows.
func TestCardsPageRendersAndStops(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
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
	if len(run.Views) != 3 {
		t.Fatalf("cards = %d, want 3", len(run.Views))
	}

	charging := run.Views[0]
	if charging.ClassName != "card c-blue" {
		t.Errorf("charging card class = %q, want %q", charging.ClassName, "card c-blue")
	}
	for _, want := range []string{"pv_a", "battery", "CHARGING", "42.5%", "Tracking MPPT", "Charge 1500 W", "page", "2 min 5 s", "yes"} {
		if !slices.Contains(charging.Texts, want) {
			t.Errorf("charging card lacks %q: %q", want, charging.Texts)
		}
	}
	if slices.Contains(charging.Texts, "no report yet") || !slices.Contains(charging.HiddenText, "no report yet") {
		t.Errorf("charging card shows no-report text: shown %q hidden %q", charging.Texts, charging.HiddenText)
	}
	if charging.Stop.Hidden {
		t.Error("charging card has no Stop button showing")
	}

	noReport := run.Views[1]
	if noReport.ClassName != "card c-grey" || !slices.Contains(noReport.Texts, "NOT CONNECTED") {
		t.Errorf("no-report card = %q %q, want grey NOT CONNECTED", noReport.ClassName, noReport.Texts)
	}
	if !slices.Contains(noReport.Texts, "no report yet") || !slices.Contains(noReport.Texts, "no request yet") {
		t.Errorf("no-report card lacks no-report text: %q", noReport.Texts)
	}
	if !noReport.Stop.Hidden {
		t.Error("card with no control shows Stop")
	}

	// A stripe value outside the known five never becomes a class name.
	if run.Views[2].ClassName != "card c-grey" {
		t.Errorf("unknown stripe class = %q, want card c-grey", run.Views[2].ClassName)
	}

	if !run.Reused {
		t.Error("cards were rebuilt by the second poll")
	}
	if run.StopIndex != 0 || len(run.Posts) != 1 {
		t.Fatalf("stop index %d, posts %d; want 0, 1", run.StopIndex, len(run.Posts))
	}
	p := run.Posts[0]
	if p.URL != "/apps/soc/api/control" || p.Body["mrid"] != "M-A" || p.Body["stop"] != true || len(p.Body) != 2 {
		t.Errorf("Stop posted %s %v, want /apps/soc/api/control {mrid:M-A, stop:true}", p.URL, p.Body)
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
