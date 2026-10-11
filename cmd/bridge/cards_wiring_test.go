package main

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/activity"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

// The pink stripe on a card is the embedded server's latch reached through the
// bridge's real adapter, so a direction change sent over the watts route shows.
func TestCardsShowPinkFromTheRealAdapter(t *testing.T) {
	const mrid = "bat-card-1"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg := registry.New()
	lfdi := strings.ToUpper(fmt.Sprintf("%x", sha1.Sum([]byte(mrid))))
	if err := reg.Add(registry.Entry{MRID: mrid, Name: "Battery 1", LFDI: lfdi}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}
	emb, err := newSEP2Embed(ctx, config{SEP2ServerAddr: "127.0.0.1:0", SEP2ServerCertDir: t.TempDir()},
		reg, testPolicyWithPIN(), nil, sep2embed.DeviceCertModeDevMint)
	if err != nil {
		t.Fatalf("newSEP2Embed: %v", err)
	}
	ctl := embedControl{embed: emb, reg: reg}
	send := func(watts float64) {
		t.Helper()
		_, err := ctl.ApplyControlFor(ctx, diff.Difference{
			Object:    mrid,
			Attribute: "DERControl.DERControlBase.opModTargetW",
			Value:     map[string]any{"multiplier": 0.0, "value": watts},
		}, 120)
		if err != nil {
			t.Fatalf("send %v W: %v", watts, err)
		}
	}
	if ctl.DirectionPink(mrid) {
		t.Fatal("pink before any send")
	}
	send(-1500)
	if ctl.DirectionPink(mrid) {
		t.Fatal("pink after a charge alone")
	}
	send(1500)
	if !ctl.DirectionPink(mrid) {
		t.Fatal("adapter DirectionPink = false after charge then discharge, want true")
	}

	seen := activity.New()
	seen.Record(lfdi)
	var store telemetryhistory.Store
	srv, err := adminui.New(adminui.Config{Addr: "127.0.0.1:0", Key: graphTestKey}, adminui.Sources{
		Registry: reg, Devices: emb, Programs: emb, Flow: &controlobs.Hook{}, Identity: emb,
		Stomp: stompUp{}, Clients: &connobs.Hook{}, Protocol: emb, History: &store,
		Control: ctl, Activity: seen, Status: emb,
	})
	if err != nil {
		t.Fatalf("adminui.New: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/apps/soc/api/cards", nil)
	req.RemoteAddr, req.Host = "127.0.0.1:40000", "localhost"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cards = %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Cards []struct {
			MRID        string `json:"mrid"`
			Stripe      string `json:"stripe"`
			StripeLabel string `json:"stripeLabel"`
		} `json:"cards"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Cards) != 1 {
		t.Fatalf("cards body = %s (%v), want one card", rec.Body, err)
	}
	if c := out.Cards[0]; c.MRID != mrid || c.Stripe != "pink" || c.StripeLabel != "CHARGE TO DISCHARGE" {
		t.Errorf("card = %+v, want pink CHARGE TO DISCHARGE", c)
	}
}

// run hands the admin UI the embedded server as its status source. The wiring
// sits inside run, which needs a live bus, so the test reads the composite
// literal rather than starting the bridge.
func TestRunWiresTheEmbedAsTheAdminUIStatusSource(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var sources *ast.CompositeLit
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if sel, ok := cl.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sources" {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "adminui" {
				sources = cl
			}
		}
		return true
	})
	if sources == nil {
		t.Fatal("no adminui.Sources literal in main.go")
	}
	fields := map[string]string{}
	for _, el := range sources.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, kok := kv.Key.(*ast.Ident)
		if !kok {
			continue
		}
		switch v := kv.Value.(type) {
		case *ast.Ident:
			fields[key.Name] = v.Name
		case *ast.CompositeLit:
			if id, ok := v.Type.(*ast.Ident); ok {
				fields[key.Name] = id.Name
			}
		}
	}
	if got := fields["Status"]; got != "embed" {
		t.Errorf("adminui.Sources.Status = %q in run, want embed", got)
	}
	if got := fields["Control"]; got != "embedControl" {
		t.Errorf("adminui.Sources.Control = %q in run, want embedControl", got)
	}
}
