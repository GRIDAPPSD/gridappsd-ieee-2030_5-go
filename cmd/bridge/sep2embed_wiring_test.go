package main

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// The CA file names below are the documented, public contract of
// sep2embed.Config.CertDir ("ca.pem, ca-key.pem, server.pem,
// server-key.pem"), not an internal detail of the sep2embed package;
// cmd/bridge relies on that doc-commented layout the same way an
// operator standing up preprovisioned material would.
const (
	testCACertFileName = "ca.pem"
	testCAKeyFileName  = "ca-key.pem"
)

// mintTestDeviceClient builds an mTLS-capable *http.Client trusting the
// embed's dev-minted CA (read back from certDir) and presenting a
// freshly minted device cert signed by that same CA, mirroring how a
// real CSIP client authenticates against the embedded server. This
// mirrors internal/sep2embed/embed_test.go's helper of the same name;
// duplicated rather than exported because it reaches into
// package-private CA file names that are an sep2embed test fixture, not
// a public export.
func mintTestDeviceClient(t *testing.T, certDir string) *http.Client {
	t.Helper()

	caCertPEM, err := os.ReadFile(filepath.Join(certDir, testCACertFileName))
	if err != nil {
		t.Fatalf("read ca.pem: %v", err)
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(certDir, testCAKeyFileName))
	if err != nil {
		t.Fatalf("read ca-key.pem: %v", err)
	}
	caCert, err := sep2cert.ParseCertificatePEM(caCertPEM)
	if err != nil {
		t.Fatalf("ParseCertificatePEM(ca.pem): %v", err)
	}
	caKey, err := sep2cert.ParseKeyPEM(caKeyPEM)
	if err != nil {
		t.Fatalf("ParseKeyPEM(ca-key.pem): %v", err)
	}

	devCertPEM, devKeyPEM, err := sep2cert.GenerateDeviceCert(caCert, caKey, sep2cert.DeviceCertOptions{
		DeviceType:  sep2cert.DeviceTypeGeneric,
		HWSerialNum: "test-serial-bridge-wiring-001",
		IsTestCert:  true,
	})
	if err != nil {
		t.Fatalf("GenerateDeviceCert: %v", err)
	}

	tlsCfg, err := sepTLS.NewClientTLSConfigFromPEM(devCertPEM, devKeyPEM, caCertPEM)
	if err != nil {
		t.Fatalf("NewClientTLSConfigFromPEM: %v", err)
	}
	// InsecureSkipVerify is safe here: the test dials by IP/port, not by
	// the server cert's SAN hostname, and RootCAs (set above) already
	// pins trust to the minted CA. Only the hostname match is skipped,
	// matching sep2embed's own embed_test.go pattern.
	tlsCfg.InsecureSkipVerify = true //nolint:gosec // trust pinned via RootCAs above; only hostname match is skipped

	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
		Timeout:   5 * time.Second,
	}
}

// TestBridgeWiresSEP2EmbedServesSeededDeviceOverMTLS proves the bridge's
// own wiring (sep2EmbedConfig, newSEP2Embed) produces a working, seeded
// IEEE 2030.5 mTLS server, distinct from sep2embed's own package tests
// which prove the package works in isolation. It does not require a
// live GridAPPS-D broker: the embed seeds from a registry built
// in-memory here, the same way run() seeds it from bootstrapRegistry's
// output.
func TestBridgeWiresSEP2EmbedServesSeededDeviceOverMTLS(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	entry := registry.Entry{
		MRID:        "mrid-wiring-test-1",
		Name:        "Wiring Test Inverter",
		LFDI:        "999999999999999999999999999999999999DEAD",
		Placeholder: true,
	}
	if err := reg.Add(entry); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	cfg := config{
		SEP2ServerAddr:    "127.0.0.1:0",
		SEP2ServerCertDir: t.TempDir(),
	}

	ctx, cancel := context.WithCancel(context.Background())

	embed, err := newSEP2Embed(ctx, cfg, reg, testPolicyWithPIN(), nil, sep2embed.DeviceCertModeDevMint)
	if err != nil {
		t.Fatalf("newSEP2Embed: %v", err)
	}

	runErr := make(chan error, 1)
	go func() {
		runErr <- embed.Run(ctx)
	}()

	client := mintTestDeviceClient(t, cfg.SEP2ServerCertDir)
	baseURL := "https://" + embed.Addr()

	dcapResp, err := client.Get(baseURL + "/dcap")
	if err != nil {
		t.Fatalf("GET /dcap: %v", err)
	}
	dcapBody, err := io.ReadAll(dcapResp.Body)
	dcapResp.Body.Close()
	if err != nil {
		t.Fatalf("read /dcap body: %v", err)
	}
	if dcapResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /dcap status = %d, want 200; body=%s", dcapResp.StatusCode, dcapBody)
	}

	edevResp, err := client.Get(baseURL + "/edev")
	if err != nil {
		t.Fatalf("GET /edev: %v", err)
	}
	edevBody, err := io.ReadAll(edevResp.Body)
	edevResp.Body.Close()
	if err != nil {
		t.Fatalf("read /edev body: %v", err)
	}
	if edevResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /edev status = %d, want 200; body=%s", edevResp.StatusCode, edevBody)
	}

	var list sep2.EndDeviceList
	if err := xml.Unmarshal(edevBody, &list); err != nil {
		t.Fatalf("unmarshal EndDeviceList: %v\nbody=%s", err, edevBody)
	}
	if int(list.All) != 1 {
		t.Fatalf("EndDeviceList.All = %d, want 1", list.All)
	}
	if len(list.EndDevice) != 1 {
		t.Fatalf("EndDeviceList.EndDevice has %d items, want 1", len(list.EndDevice))
	}
	if list.EndDevice[0].LFDI != entry.LFDI {
		t.Errorf("EndDevice[0].LFDI = %q, want the seeded registry entry's LFDI %q", list.EndDevice[0].LFDI, entry.LFDI)
	}

	// Cancel and confirm Run returns within bounds: the no-goroutine-leak
	// assertion for the bridge's own wiring path.
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned error after ctx cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3s of ctx cancel (goroutine leak or unbounded shutdown)")
	}
}
