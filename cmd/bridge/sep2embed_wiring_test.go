package main

import (
	"bytes"
	"context"
	"encoding/pem"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// deviceClient builds an mTLS *http.Client presenting certPEM/keyPEM,
// trusting caCertPEM, mirroring mintTestDeviceClient's TLS config shape
// but for a specific, caller-supplied device identity rather than an
// arbitrary throwaway one.
func deviceClient(t *testing.T, certPEM, keyPEM, caCertPEM []byte) *http.Client {
	t.Helper()

	tlsCfg, err := sepTLS.NewClientTLSConfigFromPEM(certPEM, keyPEM, caCertPEM)
	if err != nil {
		t.Fatalf("NewClientTLSConfigFromPEM: %v", err)
	}
	// InsecureSkipVerify is safe here for the same reason as
	// mintTestDeviceClient above.
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

	certDir := t.TempDir()
	const mrid = "mrid-wiring-test-1"
	// A second, non-caller device: seeded so the owner-case assertions
	// below (list.All == 1, one item, that item's LFDI) can distinguish
	// the caller's own scoped listing from a full-fleet listing. With
	// only one device ever seeded, All == 1 would also be what a
	// full-fleet regression serves, and the test would not notice.
	const mridOther = "mrid-wiring-test-2"

	// Mint a real, certificate-derived identity for the seeded device
	// the same way bootstrapRegistry does in production (main.go): this
	// creates the embed's CA under certDir as a side effect and signs
	// mrid's device cert against it. The registry is seeded with that
	// LFDI BEFORE newSEP2Embed runs, because Embed.New (via seedStores)
	// snapshots the registry once at construction; a later reg.Add would
	// never reach the served EndDeviceList. GET /edev under the
	// server-go issue 354 ownership split then returns exactly this
	// device to its own certificate.
	identities, err := sep2embed.EnsureDeviceIdentities(certDir, sep2embed.DeviceCertModeDevMint, []string{mrid, mridOther})
	if err != nil {
		t.Fatalf("EnsureDeviceIdentities: %v", err)
	}

	reg := registry.New()
	entry := registry.Entry{
		MRID: mrid,
		Name: "Wiring Test Inverter",
		LFDI: identities[mrid].LFDI,
	}
	if err := reg.Add(entry); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}
	otherEntry := registry.Entry{
		MRID: mridOther,
		Name: "Wiring Test Battery",
		LFDI: identities[mridOther].LFDI,
	}
	if err := reg.Add(otherEntry); err != nil {
		t.Fatalf("registry.Add (second device): %v", err)
	}

	cfg := config{
		SEP2ServerAddr:    "127.0.0.1:0",
		SEP2ServerCertDir: certDir,
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

	// Present the exact certificate EnsureDeviceIdentities minted for
	// mrid above, using the same certDir/devices glob helper
	// TestBootstrapRegistryDerivesRealCertBackedIdentities already uses
	// to locate it. The cert is stored as raw DER (see
	// sep2embed.EnsureDeviceIdentities' doc comment); the key is already
	// PEM at the sibling ".pem" path.
	certFile := deviceCertFileForTest(t, certDir, mrid)
	certDER, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("read device cert %q: %v", certFile, err)
	}
	callerKeyPEM, err := os.ReadFile(strings.TrimSuffix(certFile, ".x509") + ".pem")
	if err != nil {
		t.Fatalf("read device key for %q: %v", certFile, err)
	}
	callerCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	caCertPEM, err := os.ReadFile(filepath.Join(certDir, testCACertFileName))
	if err != nil {
		t.Fatalf("read ca.pem: %v", err)
	}

	client := deviceClient(t, callerCertPEM, callerKeyPEM, caCertPEM)
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

	// GET /edev: the caller's own certificate lists exactly its own
	// EndDevice, by field value.
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
	got := list.EndDevice[0]
	if got.LFDI != entry.LFDI {
		t.Errorf("EndDevice[0].LFDI = %q, want the caller's own %q", got.LFDI, entry.LFDI)
	}
	if got.LFDI == otherEntry.LFDI {
		t.Fatalf("EndDevice[0].LFDI = %q, the second seeded device's LFDI: owner-case listing leaked another caller's device", got.LFDI)
	}
	if got.Href == "" {
		t.Errorf("EndDevice[0].Href is empty, want the resource's own href")
	}
	if !sepTLS.ValidateSFDI(got.SFDI) {
		t.Errorf("EndDevice[0].SFDI %q fails ValidateSFDI", got.SFDI)
	}

	// A certificate owning no seeded device gets an empty list rather
	// than the fleet or a 403: /edev is a common resource any
	// authenticated device may read.
	otherClient := mintTestDeviceClient(t, cfg.SEP2ServerCertDir)
	otherResp, err := otherClient.Get(baseURL + "/edev")
	if err != nil {
		t.Fatalf("GET /edev (certificate owning no seeded device): %v", err)
	}
	otherBody, err := io.ReadAll(otherResp.Body)
	otherResp.Body.Close()
	if err != nil {
		t.Fatalf("read /edev body (certificate owning no seeded device): %v", err)
	}
	if otherResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /edev (certificate owning no seeded device) status = %d, want 200; body=%s", otherResp.StatusCode, otherBody)
	}
	var otherList sep2.EndDeviceList
	if err := xml.Unmarshal(otherBody, &otherList); err != nil {
		t.Fatalf("unmarshal EndDeviceList (certificate owning no seeded device): %v\nbody=%s", err, otherBody)
	}
	if otherList.All != 0 || len(otherList.EndDevice) != 0 {
		t.Fatalf("EndDeviceList (certificate owning no seeded device) = {All: %d, len(EndDevice): %d}, want {0, 0}", otherList.All, len(otherList.EndDevice))
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

// TestBridgeServesConfiguredOpModMaxLimWOverMTLS is the end-to-end proof for
// issue #111: an operator-written -sep2-default-control-file reaches the
// wire, not just the parsed config struct. It goes through the same
// loadConfig/buildSEP2Policy path run() uses, unlike
// TestBridgeWiresSEP2EmbedServesSeededDeviceOverMTLS above, which builds its
// config struct by hand.
//
// The href is discovered via Embed.DERPrograms (an in-process accessor,
// not the property under test) so this test does not depend on the fixed
// FSA/DERProgram ids sep2embed's own package-internal tests use, which are
// unexported across the package boundary. The served bytes themselves are
// read the same way a device would: a real mTLS GET.
func TestBridgeServesConfiguredOpModMaxLimWOverMTLS(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()
	const mrid = "mrid-maxlimw-test-1"

	identities, err := sep2embed.EnsureDeviceIdentities(certDir, sep2embed.DeviceCertModeDevMint, []string{mrid})
	if err != nil {
		t.Fatalf("EnsureDeviceIdentities: %v", err)
	}
	reg := registry.New()
	entry := registry.Entry{MRID: mrid, Name: "MaxLimW Test Inverter", LFDI: identities[mrid].LFDI}
	if err := reg.Add(entry); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	controlPath := writeDefaultControlFile(t, `{"opModMaxLimW": 5000}`)
	cfg, err := loadConfig([]string{
		"-sep2-server-addr=127.0.0.1:0",
		"-sep2-server-cert-dir=" + certDir,
		"-sep2-default-control-file=" + controlPath,
		// A valid checksum PIN (see pinhelper_test.go's testPolicyWithPIN
		// doc comment): buildSEP2Policy's validation gate does not care
		// which value, only that it passes 6.3.5's checksum rule.
		"-sep2-registration-pin=123455",
	})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	policy, err := buildSEP2Policy(cfg)
	if err != nil {
		t.Fatalf("buildSEP2Policy: %v", err)
	}
	if policy.DefaultControl.DERControlBase == nil || policy.DefaultControl.DERControlBase.OpModMaxLimW == nil ||
		*policy.DefaultControl.DERControlBase.OpModMaxLimW != sep2.PerCent(5000) {
		t.Fatalf("policy.DefaultControl.DERControlBase.OpModMaxLimW = %+v, want a configured 5000; "+
			"the wire assertion below cannot be meaningful if the value never reached policy", policy.DefaultControl.DERControlBase)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	embed, err := newSEP2Embed(ctx, cfg, reg, policy, nil, sep2embed.DeviceCertModeDevMint)
	if err != nil {
		t.Fatalf("newSEP2Embed: %v", err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- embed.Run(ctx) }()

	edevs, err := embed.EndDevices(ctx)
	if err != nil {
		t.Fatalf("EndDevices: %v", err)
	}
	if len(edevs) != 1 {
		t.Fatalf("EndDevices = %d devices, want 1", len(edevs))
	}
	programs, err := embed.DERPrograms(ctx, edevs[0].ID)
	if err != nil {
		t.Fatalf("DERPrograms: %v", err)
	}
	if len(programs) != 1 || programs[0].DefaultDERControlLink == "" {
		t.Fatalf("DERPrograms = %+v, want exactly one program with a DefaultDERControlLink", programs)
	}

	certFile := deviceCertFileForTest(t, certDir, mrid)
	certDER, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("read device cert %q: %v", certFile, err)
	}
	callerKeyPEM, err := os.ReadFile(strings.TrimSuffix(certFile, ".x509") + ".pem")
	if err != nil {
		t.Fatalf("read device key for %q: %v", certFile, err)
	}
	callerCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	caCertPEM, err := os.ReadFile(filepath.Join(certDir, testCACertFileName))
	if err != nil {
		t.Fatalf("read ca.pem: %v", err)
	}
	client := deviceClient(t, callerCertPEM, callerKeyPEM, caCertPEM)

	resp, err := client.Get("https://" + embed.Addr() + programs[0].DefaultDERControlLink)
	if err != nil {
		t.Fatalf("GET DefaultDERControl: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read DefaultDERControl body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET DefaultDERControl status = %d, want 200\nbody=%s", resp.StatusCode, body)
	}

	// The bare-element, schema-order form: PerCent marshals as element text,
	// never multiplier/value children (unlike ActivePower), and this is the
	// wire proof that the value the operator wrote actually reached it.
	const want = `<opModMaxLimW>5000</opModMaxLimW>`
	if !bytes.Contains(body, []byte(want)) {
		t.Errorf("served DefaultDERControl does not carry %s\nbody=%s", want, body)
	}

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
