package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/activity"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

const commsTestKey = "comms-test-admin-key"

func commsGet(t *testing.T, h http.Handler, path string) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "127.0.0.1:40000"
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer "+commsTestKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body)
	}
	return rec.Body.Bytes()
}

// TestRequestThroughEmbedMarksDeviceOnlineInDevicesPayload sends a real
// mTLS request to the embedded server and reads the Devices payload and
// /api/clients the way the admin UI does: one recorder, written by the
// router, must show the device Online with the same last request in both.
func TestRequestThroughEmbedMarksDeviceOnlineInDevicesPayload(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()
	const mrid = "mrid-comms-test-1"
	identities, err := sep2embed.EnsureDeviceIdentities(certDir, sep2embed.DeviceCertModeDevMint, []string{mrid})
	if err != nil {
		t.Fatalf("EnsureDeviceIdentities: %v", err)
	}
	lfdi := identities[mrid].LFDI
	reg := registry.New()
	if err := reg.Add(registry.Entry{MRID: mrid, Name: "Comms Test", LFDI: lfdi}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	rec := activity.New()
	var hook connobs.Hook
	ctx, cancel := context.WithCancel(context.Background())
	embed, err := newSEP2EmbedWithActivity(ctx, config{
		SEP2ServerAddr:    "127.0.0.1:0",
		SEP2ServerCertDir: certDir,
	}, reg, testPolicyWithPIN(), &hook, sep2embed.DeviceCertModeDevMint, rec)
	if err != nil {
		cancel()
		t.Fatalf("newSEP2EmbedWithActivity: %v", err)
	}
	embedDone := make(chan error, 1)
	go func() { embedDone <- embed.Run(ctx) }()

	cfg := config{}
	cfg.Tuning.AdminClientIdleAfter = 90 * time.Second
	acfg := adminUIConfig(cfg)
	acfg.Addr, acfg.Key = "127.0.0.1:0", commsTestKey
	srv, err := adminui.New(acfg, adminui.Sources{
		Registry: reg,
		Devices:  embed,
		Programs: embed,
		Flow:     &controlobs.Hook{},
		Identity: embed,
		Stomp:    stompUp{},
		Clients:  &hook,
		Activity: rec,
		Protocol: embed,
		History:  &telemetryhistory.Store{},
	})
	if err != nil {
		cancel()
		t.Fatalf("adminui.New: %v", err)
	}
	adminDone := make(chan error, 1)
	go func() { adminDone <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		for _, ch := range []chan error{embedDone, adminDone} {
			select {
			case <-ch:
			case <-time.After(10 * time.Second):
				t.Error("listener did not stop")
			}
		}
	})

	type devices struct {
		After   float64 `json:"commsOfflineAfterSeconds"`
		Devices []struct {
			LFDI        string  `json:"lfdi"`
			LastRequest *string `json:"lastRequest"`
			Comms       string  `json:"comms"`
		} `json:"devices"`
	}
	read := func() devices {
		var d devices
		if err := json.Unmarshal(commsGet(t, srv.Handler(), "/dashboard/data"), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	before := read()
	if len(before.Devices) != 1 || before.Devices[0].Comms != "not_seen" || before.Devices[0].LastRequest != nil {
		t.Fatalf("before any request: %+v, want one device not_seen with no lastRequest", before.Devices)
	}
	if before.After != 90 {
		t.Errorf("commsOfflineAfterSeconds = %v, want the 90s idle setting", before.After)
	}

	certFile := deviceCertFileForTest(t, certDir, mrid)
	certDER, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(strings.TrimSuffix(certFile, ".x509") + ".pem")
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(certDir + "/" + testCACertFileName)
	if err != nil {
		t.Fatal(err)
	}
	client := deviceClient(t, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), keyPEM, caPEM)
	resp, err := client.Get("https://" + embed.Addr() + "/dcap")
	if err != nil {
		t.Fatalf("GET /dcap: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /dcap = %d, want 200", resp.StatusCode)
	}

	after := read()
	if len(after.Devices) != 1 || after.Devices[0].LFDI != lfdi || after.Devices[0].Comms != "online" || after.Devices[0].LastRequest == nil {
		t.Fatalf("after a request: %+v, want %s online with a lastRequest", after.Devices, lfdi)
	}

	var clients struct {
		Clients []struct {
			LFDI         string `json:"lfdi"`
			LastSeen     string `json:"lastSeen"`
			RequestCount uint64 `json:"requestCount"`
			Connected    bool   `json:"connected"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(commsGet(t, srv.Handler(), "/api/clients"), &clients); err != nil {
		t.Fatal(err)
	}
	if len(clients.Clients) != 1 || !clients.Clients[0].Connected {
		t.Fatalf("/api/clients = %+v, want one connected client", clients.Clients)
	}
	devAt, err := time.Parse(time.RFC3339, *after.Devices[0].LastRequest)
	if err != nil {
		t.Fatal(err)
	}
	cliAt, err := time.Parse("2006-01-02T15:04:05.000Z07:00", clients.Clients[0].LastSeen)
	if err != nil {
		t.Fatal(err)
	}
	if !cliAt.Truncate(time.Second).Equal(devAt) {
		t.Errorf("/api/clients lastSeen %s vs devices lastRequest %s", clients.Clients[0].LastSeen, *after.Devices[0].LastRequest)
	}
	if _, n, ok := rec.Last(lfdi); !ok || n != clients.Clients[0].RequestCount {
		t.Errorf("recorder count %d (ok=%v) vs /api/clients requestCount %d", n, ok, clients.Clients[0].RequestCount)
	}
}
