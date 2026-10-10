package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/handlers/flow_reservation"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

// The watts control route against the real embedded server: the validator
// and the stores answer, not a fake, and the Response the status reports
// is one written to the store the protocol listener's POST writes.
func TestWattsControlRouteIssuesARealControlAndReportsItsState(t *testing.T) {
	const mrid = "bat-ctl-1"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg := registry.New()
	lfdi := strings.ToUpper(fmt.Sprintf("%x", sha1.Sum([]byte(mrid))))
	if err := reg.Add(registry.Entry{MRID: mrid, LFDI: lfdi}); err != nil {
		t.Fatalf("registry.Add: %v", err)
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

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/apps/soc/api/control", strings.NewReader(body))
		req.RemoteAddr, req.Host = "127.0.0.1:40000", "localhost"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	type status struct {
		ID               string `json:"id"`
		Watts            int64  `json:"watts"`
		DurationSeconds  int    `json:"durationSeconds"`
		ControlState     string `json:"controlState"`
		Received         bool   `json:"received"`
		ResponseStatuses []int  `json:"responseStatuses"`
		Verdict          string `json:"verdict"`
	}

	rec := post(`{"mrid":"` + mrid + `","watts":-1500,"durationSeconds":120}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("send = %d: %s", rec.Code, rec.Body)
	}
	var sent status
	if err := json.Unmarshal(rec.Body.Bytes(), &sent); err != nil {
		t.Fatalf("send body: %v", err)
	}
	if sent.Watts != -1500 || sent.DurationSeconds != 120 || sent.ControlState != "active" || sent.Received || sent.Verdict != "waiting" {
		t.Errorf("send status = %+v, want -1500 W, 120 s, active, not received, waiting", sent)
	}

	// The served control carries the commanded value.
	derps, err := emb.EndDevices(ctx)
	if err != nil || len(derps) != 1 {
		t.Fatalf("EndDevices = %v, %v", derps, err)
	}
	ctls, err := emb.DERControls(ctx, derps[0].ID, "1", "1")
	if err != nil {
		t.Fatalf("DERControls: %v", err)
	}
	var active *sep2embed.DERControlSnapshot
	for i := range ctls {
		if ctls[i].CurrentStatus == sep2.EventStatusActive {
			active = &ctls[i]
		}
	}
	if active == nil || active.Base == nil || active.Base.OpModTargetW == nil || active.Base.OpModTargetW.Value != -1500 {
		t.Fatalf("active control = %+v, want opModTargetW -1500", active)
	}

	// A Response the device posted for that event, through the protocol
	// listener's own POST handler and so under its own key, is reported as
	// received.
	received := sep2.ResponseStatusEventReceived
	rspBody, err := xml.Marshal(&sep2.DERControlResponse{Response: sep2.Response{Subject: active.MRID, Status: &received, EndDeviceLFDI: lfdi}})
	if err != nil {
		t.Fatal(err)
	}
	allow := func(*http.Request, string) (bool, string, error) { return true, lfdi, nil }
	rspMux := http.NewServeMux()
	rspMux.HandleFunc("POST /rsps/{rspsId}/rsp", flow_reservation.HandlePostResponse(emb.Stores().Responses, allow))
	prec := httptest.NewRecorder()
	rspMux.ServeHTTP(prec, httptest.NewRequest(http.MethodPost, "/rsps/1/rsp", bytes.NewReader(rspBody)))
	if prec.Code != http.StatusCreated {
		t.Fatalf("POST response = %d: %s", prec.Code, prec.Body)
	}
	get := httptest.NewRequest(http.MethodGet, "/apps/soc/api/control/"+sent.ID, nil)
	get.RemoteAddr, get.Host = "127.0.0.1:40000", "localhost"
	grec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(grec, get)
	var got status
	if err := json.Unmarshal(grec.Body.Bytes(), &got); err != nil || grec.Code != http.StatusOK {
		t.Fatalf("status = %d %s (%v)", grec.Code, grec.Body, err)
	}
	if !got.Received || len(got.ResponseStatuses) != 1 || got.ResponseStatuses[0] != 1 || got.ControlState != "active" {
		t.Errorf("status = %+v, want received with statuses [1] and active", got)
	}

	// Refusals come from the real validator and write nothing.
	before := len(ctls)
	if rec := post(`{"mrid":"` + mrid + `","watts":40000}`); rec.Code != http.StatusBadRequest {
		t.Errorf("40000 W = %d %s, want 400", rec.Code, rec.Body)
	}
	if rec := post(`{"mrid":"unregistered","watts":100}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown device = %d %s, want 404", rec.Code, rec.Body)
	}
	after, err := emb.DERControls(ctx, derps[0].ID, "1", "1")
	if err != nil || len(after) != before {
		t.Errorf("controls after refusals = %d (%v), want the %d from before", len(after), err, before)
	}
}
