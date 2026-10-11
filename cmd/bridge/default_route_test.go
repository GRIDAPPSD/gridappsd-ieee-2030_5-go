package main

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

// The default route against the real embedded server: a set cancels the page
// send and its follow-on, leaves the bus control on another device alone, and
// the cards read the stored default back.
func TestDefaultRouteSetsTheStoredDefaultAndCancelsOnlyPageControls(t *testing.T) {
	const page, bus = "bat-def-1", "bat-def-2"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg := registry.New()
	for _, m := range []string{page, bus} {
		lfdi := strings.ToUpper(fmt.Sprintf("%x", sha1.Sum([]byte(m))))
		if err := reg.Add(registry.Entry{MRID: m, Name: m, LFDI: lfdi}); err != nil {
			t.Fatalf("registry.Add: %v", err)
		}
	}
	emb, err := newSEP2Embed(ctx, config{SEP2ServerAddr: "127.0.0.1:0", SEP2ServerCertDir: t.TempDir()},
		reg, testPolicyWithPIN(), nil, sep2embed.DeviceCertModeDevMint)
	if err != nil {
		t.Fatalf("newSEP2Embed: %v", err)
	}
	var store telemetryhistory.Store
	srv, err := adminui.New(adminui.Config{Addr: "127.0.0.1:0", Key: graphTestKey}, adminui.Sources{
		Registry: reg, Devices: emb, Programs: emb, Flow: &controlobs.Hook{}, Identity: emb,
		Stomp: stompUp{}, Clients: &connobs.Hook{}, Protocol: emb, History: &store,
		Control: embedControl{embed: emb, reg: reg},
	})
	if err != nil {
		t.Fatalf("adminui.New: %v", err)
	}
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr, req.Host = "127.0.0.1:40000", "localhost"
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	if rec := do(http.MethodPost, "/apps/soc/api/control", `{"mrid":"`+page+`","watts":-1500,"durationSeconds":120}`); rec.Code != http.StatusOK {
		t.Fatalf("page send = %d: %s", rec.Code, rec.Body)
	}
	if err := emb.ApplyControlDelta(ctx, reg, diff.Difference{
		Object: bus, Attribute: "DERControl.DERControlBase.opModTargetW",
		Value: map[string]any{"multiplier": 0.0, "value": 900.0},
	}); err != nil {
		t.Fatalf("bus delta: %v", err)
	}
	busBefore, err := emb.ControlsFor(ctx, bus)
	if err != nil || len(busBefore) != 1 {
		t.Fatalf("bus controls = %+v, %v; want 1", busBefore, err)
	}

	rec := do(http.MethodPost, "/apps/soc/api/default", `{"mrid":"`+page+`","watts":-800}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("default = %d: %s", rec.Code, rec.Body)
	}
	var set struct {
		Default           *float64 `json:"default"`
		CancelledControls int      `json:"cancelledControls"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &set); err != nil {
		t.Fatalf("default body: %v", err)
	}
	if set.Default == nil || *set.Default != -800 || set.CancelledControls != 2 {
		t.Errorf("default body = %s, want default -800 and 2 cancelled", rec.Body)
	}

	snaps, err := emb.ControlsFor(ctx, page)
	if err != nil || len(snaps) != 2 {
		t.Fatalf("page controls = %+v, %v; want 2", snaps, err)
	}
	for _, s := range snaps {
		if s.CurrentStatus != sep2.EventStatusCancelled {
			t.Errorf("page control %s status %d, want Cancelled", s.ID, s.CurrentStatus)
		}
	}
	// A set on the bus device with the page ledger in hand still leaves its
	// bus control running.
	if rec := do(http.MethodPost, "/apps/soc/api/default", `{"mrid":"`+bus+`","watts":100}`); rec.Code != http.StatusOK {
		t.Fatalf("bus device default = %d: %s", rec.Code, rec.Body)
	}
	busAfter, err := emb.ControlsFor(ctx, bus)
	if err != nil || len(busAfter) != 1 || busAfter[0].CurrentStatus != sep2.EventStatusActive {
		t.Errorf("bus controls after a default = %+v, %v; want the one Active control", busAfter, err)
	}

	rec = do(http.MethodGet, "/apps/soc/api/cards", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("cards = %d: %s", rec.Code, rec.Body)
	}
	var cards struct {
		Cards []struct {
			MRID    string   `json:"mrid"`
			Default *float64 `json:"default"`
			Control *struct {
				Watts float64 `json:"watts"`
			} `json:"control"`
			InForce *struct {
				Kind  string  `json:"kind"`
				Watts float64 `json:"watts"`
			} `json:"inForce"`
		} `json:"cards"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cards); err != nil || len(cards.Cards) != 2 {
		t.Fatalf("cards body = %s (%v), want two cards", rec.Body, err)
	}
	for _, c := range cards.Cards {
		switch c.MRID {
		case page:
			if c.Default == nil || *c.Default != -800 || c.Control != nil || c.InForce == nil || c.InForce.Kind != "default" || c.InForce.Watts != -800 {
				t.Errorf("page card = %+v, want default -800 in force and no control", c)
			}
		case bus:
			if c.Default == nil || *c.Default != 100 || c.InForce == nil || c.InForce.Kind != "event" || c.InForce.Watts != 900 {
				t.Errorf("bus card = %+v, want default 100 and the 900 W event in force", c)
			}
		}
	}

	if rec := do(http.MethodPost, "/apps/soc/api/default", `{"mrid":"`+page+`","clear":true}`); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"default":null`) {
		t.Errorf("clear = %d %s, want 200 with default null", rec.Code, rec.Body)
	}
}
