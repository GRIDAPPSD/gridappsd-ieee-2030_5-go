package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

type stompUp struct{}

func (stompUp) IsConnected() bool { return true }

const graphTestKey = "graph-test-admin-key"

func graphGet(t *testing.T, h http.Handler, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/apps/gridappsd-graph-input/data", nil)
	req.RemoteAddr = "127.0.0.1:40000"
	req.Host = "localhost"
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestGraphPanelShowsStateOfChargeThePublisherSent feeds state of
// charge through the publisher's history hook, the only path it reaches
// the history by in the bridge, and
// reads the data the graph page reads. Writing into the store directly would
// pass while the publisher fed nothing.
func TestGraphPanelShowsStateOfChargeThePublisherSent(t *testing.T) {
	var store telemetryhistory.Store
	h := newSinkHarness(t, &store, "bat-1", "bat-2")
	publishToHistory(t, h.sink, h.reg, socSnapshot("bat-1", 6500), socSnapshot("bat-2", 3000))
	// Later samples, at their own times, through the same hook.
	h.observe(statusFrame(t, "bat-1", 6400, frameEpoch+15))

	ctx, cancel := context.WithCancel(context.Background())
	emb, err := newSEP2Embed(ctx, config{
		SEP2ServerAddr:    "127.0.0.1:0",
		SEP2ServerCertDir: t.TempDir(),
	}, h.reg, testPolicyWithPIN(), nil, sep2embed.DeviceCertModeDevMint)
	if err != nil {
		cancel()
		t.Fatalf("newSEP2Embed: %v", err)
	}
	srv, err := adminui.New(adminui.Config{Addr: "127.0.0.1:0", Key: graphTestKey}, adminui.Sources{
		Registry: h.reg,
		Devices:  emb,
		Programs: emb,
		Flow:     &controlobs.Hook{},
		Identity: emb,
		Stomp:    stompUp{},
		Clients:  &connobs.Hook{},
		Protocol: emb,
		History:  &store,
	})
	if err != nil {
		cancel()
		t.Fatalf("adminui.New: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("admin listener did not stop")
		}
	})

	if rec := graphGet(t, srv.Handler(), ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no credential: %d, want 401", rec.Code)
	}
	rec := graphGet(t, srv.Handler(), "Bearer "+graphTestKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("panel: %d %s", rec.Code, rec.Body.String())
	}
	var d struct {
		Unit   string `json:"unit"`
		Series []struct {
			Name   string       `json:"name"`
			Points [][2]float64 `json:"points"`
		} `json:"series"`
		Latest struct {
			Body struct {
				Rows [][]struct {
					Text string `json:"text"`
				} `json:"rows"`
			} `json:"body"`
		} `json:"latest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.Unit != "%" || len(d.Series) != 2 {
		t.Fatalf("graph data = %s, want unit %% and two series", rec.Body.String())
	}
	b1, b2 := d.Series[0], d.Series[1]
	if b1.Name != "bat-1" || len(b1.Points) != 2 ||
		b1.Points[0] != [2]float64{float64(frameEpoch), 65} ||
		b1.Points[1] != [2]float64{float64(frameEpoch + 15), 64} {
		t.Errorf("bat-1 = %+v, want 65 then 64 at the envelope times", b1)
	}
	if b2.Name != "bat-2" || len(b2.Points) != 1 || b2.Points[0] != [2]float64{float64(frameEpoch), 30} {
		t.Errorf("bat-2 = %+v, want 30 at the envelope time", b2)
	}
	rows := d.Latest.Body.Rows
	if len(rows) != 2 || rows[0][0].Text != "bat-1" || rows[0][1].Text != "64" || rows[1][1].Text != "30" {
		t.Errorf("latest rows = %+v, want bat-1 64 and bat-2 30", rows)
	}
}
