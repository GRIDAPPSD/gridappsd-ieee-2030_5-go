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
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

type stompUp struct{}

func (stompUp) IsConnected() bool { return true }

const graphTestKey = "graph-test-admin-key"

func graphGet(t *testing.T, h http.Handler, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/ui/panels/gridappsd-graph-input", nil)
	req.RemoteAddr = "127.0.0.1:40000"
	req.Host = "localhost"
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestGraphPanelShowsStateOfChargeDeliveredToTheSubscriber feeds state of
// charge through runControlSubscriber, the only path it reaches the
// history by in the bridge, and reads the panel the shell reads. Writing
// into the store directly would pass while the subscriber fed nothing.
func TestGraphPanelShowsStateOfChargeDeliveredToTheSubscriber(t *testing.T) {
	var store telemetryhistory.Store
	h := startHistoryHarness(t, &store)
	h.register(t, "bat-1", "bat-2")

	h.bus.deliver(socFrame(t, "bat-1", 6500, frameEpoch))
	h.bus.deliver(socFrame(t, "bat-2", 3000, frameEpoch+5))
	h.bus.deliver(socFrame(t, "bat-1", 6400, frameEpoch+15))
	h.waitFrames(t, 3)

	ctx, cancel := context.WithCancel(context.Background())
	srv, err := adminui.New(adminui.Config{Addr: "127.0.0.1:0", Key: graphTestKey}, adminui.Sources{
		Registry: h.reg,
		Devices:  h.embed,
		Programs: h.embed,
		Flow:     &controlobs.Hook{},
		Identity: h.embed,
		Stomp:    stompUp{},
		Clients:  &connobs.Hook{},
		Protocol: h.embed,
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
		Sections []struct {
			Kind string `json:"kind"`
			Body struct {
				Unit   string `json:"unit"`
				Series []struct {
					Name   string       `json:"name"`
					Points [][2]float64 `json:"points"`
				} `json:"series"`
				Rows [][]struct {
					Text string `json:"text"`
				} `json:"rows"`
			} `json:"body"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(d.Sections) != 2 || d.Sections[0].Kind != "chart" || d.Sections[1].Kind != "table" {
		t.Fatalf("sections = %s, want a chart then a table", rec.Body.String())
	}
	chart := d.Sections[0].Body
	if chart.Unit != "%" || len(chart.Series) != 2 {
		t.Fatalf("chart = %+v, want unit %% and two series", chart)
	}
	b1, b2 := chart.Series[0], chart.Series[1]
	if b1.Name != "bat-1" || len(b1.Points) != 2 ||
		b1.Points[0] != [2]float64{float64(frameEpoch) * 1000, 65} ||
		b1.Points[1] != [2]float64{float64(frameEpoch+15) * 1000, 64} {
		t.Errorf("bat-1 = %+v, want 65 then 64 at the envelope times", b1)
	}
	if b2.Name != "bat-2" || len(b2.Points) != 1 || b2.Points[0] != [2]float64{float64(frameEpoch+5) * 1000, 30} {
		t.Errorf("bat-2 = %+v, want 30 at its envelope time", b2)
	}
	rows := d.Sections[1].Body.Rows
	if len(rows) != 2 || rows[0][0].Text != "bat-1" || rows[0][1].Text != "64" || rows[1][1].Text != "30" {
		t.Errorf("latest rows = %+v, want bat-1 64 and bat-2 30", rows)
	}
}
