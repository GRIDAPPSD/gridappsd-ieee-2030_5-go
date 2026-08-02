package sep2embed

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	coresub "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/handlers/subscription"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// fixtureEntries are three deterministic registry entries (mirroring
// three of the bridge's real inverter/battery/solar categories) used
// across the embed_test.go scenarios below.
func fixtureEntries() []registry.Entry {
	return []registry.Entry{
		{MRID: "mrid-inv-1", Name: "Inverter 1", LFDI: "111111111111111111111111111111111111AAAA", Placeholder: true},
		{MRID: "mrid-bat-1", Name: "Battery 1", LFDI: "222222222222222222222222222222222222BBBB", Placeholder: true},
		{MRID: "mrid-sol-1", Name: "Solar 1", LFDI: "333333333333333333333333333333333333CCCC", Placeholder: true},
	}
}

// mintTestDeviceClient builds an mTLS-capable *http.Client trusting the
// Embed's own CA (read back from certDir) and presenting a freshly
// minted device cert signed by that same CA, mirroring how a real CSIP
// client authenticates against the embedded server.
func mintTestDeviceClient(t *testing.T, certDir string) *http.Client {
	t.Helper()

	caCertPEM, err := os.ReadFile(filepath.Join(certDir, caCertFileName))
	if err != nil {
		t.Fatalf("read ca.pem: %v", err)
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(certDir, caKeyFileName))
	if err != nil {
		t.Fatalf("read ca-key.pem: %v", err)
	}
	caCert, caKey, err := parseCAPair(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("parseCAPair: %v", err)
	}

	devCertPEM, devKeyPEM, err := sep2cert.GenerateDeviceCert(caCert, caKey, sep2cert.DeviceCertOptions{
		DeviceType:  sep2cert.DeviceTypeGeneric,
		HWSerialNum: "test-serial-embed-001",
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
	// pins trust to the minted CA. Full hostname verification is
	// exercised by the "localhost"/"127.0.0.1" SANs the server cert
	// carries; skip only the hostname match, not chain trust.
	tlsCfg.InsecureSkipVerify = true //nolint:gosec // trust pinned via RootCAs above; only hostname match is skipped

	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
		Timeout:   5 * time.Second,
	}
}

func TestEmbedServesSeededDevicesOverMTLS(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	entries := fixtureEntries()
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	certDir := t.TempDir()
	cfg := Config{
		Addr:                   "127.0.0.1:0",
		CertDir:                certDir,
		ResolveRegistrationPIN: testResolvePIN,
		ShutdownTimeout:        time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())

	e, err := New(ctx, cfg, reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runErr := make(chan error, 1)
	go func() {
		runErr <- e.Run(ctx)
	}()

	baseURL := "https://" + e.Addr()
	client := mintTestDeviceClient(t, certDir)

	// GET /dcap: the discovery root every CSIP client hits first.
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

	// GET /edev: assert the parsed list contains exactly the seeded
	// devices, by LFDI/SFDI value, not just a 200 and a non-empty body.
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

	if int(list.All) != len(entries) {
		t.Fatalf("EndDeviceList.All = %d, want %d", list.All, len(entries))
	}
	if len(list.EndDevice) != len(entries) {
		t.Fatalf("EndDeviceList.EndDevice has %d items, want %d", len(list.EndDevice), len(entries))
	}

	wantLFDIs := make([]string, len(entries))
	for i, e := range entries {
		wantLFDIs[i] = e.LFDI
	}
	gotLFDIs := make([]string, len(list.EndDevice))
	for i, d := range list.EndDevice {
		gotLFDIs[i] = d.LFDI
		if d.SFDI == "" {
			t.Errorf("EndDevice[%d] (LFDI %q) has empty SFDI", i, d.LFDI)
		}
		if !sepTLS.ValidateSFDI(d.SFDI) {
			t.Errorf("EndDevice[%d] (LFDI %q) SFDI %q fails ValidateSFDI", i, d.LFDI, d.SFDI)
		}
	}
	sort.Strings(wantLFDIs)
	sort.Strings(gotLFDIs)
	for i := range wantLFDIs {
		if gotLFDIs[i] != wantLFDIs[i] {
			t.Fatalf("EndDeviceList LFDIs = %v, want %v", gotLFDIs, wantLFDIs)
		}
	}

	// A no-cert client must be rejected at the TLS handshake, before any
	// HTTP status is even produced.
	caCertPEM, err := os.ReadFile(filepath.Join(certDir, caCertFileName))
	if err != nil {
		t.Fatalf("read ca.pem: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCertPEM) {
		t.Fatalf("AppendCertsFromPEM(ca.pem) failed")
	}
	noCertClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: caPool, InsecureSkipVerify: true}}, //nolint:gosec // trust pinned via RootCAs; hostname match skipped for the same reason as mintTestDeviceClient
		Timeout:   5 * time.Second,
	}
	if _, err := noCertClient.Get(baseURL + "/dcap"); err == nil {
		t.Fatalf("no-cert client GET /dcap succeeded, want a TLS handshake error")
	}

	// Cancel the single ctx root and confirm Run returns cleanly, within
	// bounds. Run blocks until both the listener goroutine AND the
	// subscription notifier's worker pool have exited (see Embed.Run),
	// so a bounded return here is the no-goroutine-leak assertion: if
	// either had leaked, this select would hit the timeout branch.
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

func TestEmbedIdentityMatchesServerLeafCertificate(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	if err := reg.AddBatch(fixtureEntries()); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	certDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	e, err := New(ctx, Config{Addr: "127.0.0.1:0", CertDir: certDir, ShutdownTimeout: time.Second, ResolveRegistrationPIN: testResolvePIN}, reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id := e.Identity()
	if id.SFDI == "" {
		t.Error("Embed.Identity().SFDI is empty")
	}
	if id.LFDI == "" {
		t.Error("Embed.Identity().LFDI is empty")
	}

	// Independently re-derive the expected SFDI/LFDI from the leaf
	// certificate New wrote to disk, rather than reaching into Embed's
	// unexported srv field (now an interface; see protocolServer).
	serverCertPEM, err := os.ReadFile(filepath.Join(certDir, serverCertFileName))
	if err != nil {
		t.Fatalf("read server.pem: %v", err)
	}
	leaf, err := sep2cert.ParseCertificatePEM(serverCertPEM)
	if err != nil {
		t.Fatalf("ParseCertificatePEM(server.pem): %v", err)
	}
	wantSFDI := sepTLS.SFDI(leaf)
	wantLFDI := sepTLS.LFDI(leaf)
	if id.SFDI != wantSFDI {
		t.Errorf("Embed.Identity().SFDI = %q, want %q (derived from server.pem)", id.SFDI, wantSFDI)
	}
	if id.LFDI != wantLFDI {
		t.Errorf("Embed.Identity().LFDI = %q, want %q (derived from server.pem)", id.LFDI, wantLFDI)
	}

	// Run and immediately cancel so the bound listener from New doesn't
	// outlive the test.
	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3s of ctx cancel")
	}
}

func TestEmbedRejectsMissingRequiredConfig(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	ctx := context.Background()

	if _, err := New(ctx, Config{CertDir: t.TempDir()}, reg); err == nil {
		t.Error("New with empty Addr: want error, got nil")
	}
	if _, err := New(ctx, Config{Addr: "127.0.0.1:0"}, reg); err == nil {
		t.Error("New with empty CertDir: want error, got nil")
	}
	if _, err := New(ctx, Config{Addr: "127.0.0.1:0", CertDir: t.TempDir()}, nil); err == nil {
		t.Error("New with nil registry: want error, got nil")
	}
}

// TestNewFailsClosedWhenObserverSetWithCCMEnabled proves the
// errObserverRequiresGCM fail-closed guard actually fires: New with
// both Config.Observer non-nil and Config.EnableCCM true must return an
// error satisfying errors.Is(err, errObserverRequiresGCM), rather than
// silently building a CCM listener with no handshake observation (the
// invisible-gap outcome mtls.go's doc comment on errObserverRequiresGCM
// explicitly rejects).
func TestNewFailsClosedWhenObserverSetWithCCMEnabled(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	if err := reg.AddBatch(fixtureEntries()); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	var hook connobs.Hook
	_, err := New(context.Background(), Config{
		Addr:                   "127.0.0.1:0",
		CertDir:                t.TempDir(),
		ResolveRegistrationPIN: testResolvePIN,
		EnableCCM:              true,
		Observer:               &hook,
	}, reg)

	if err == nil {
		t.Fatal("New with Observer set and EnableCCM true: want error, got nil")
	}
	if !errors.Is(err, errObserverRequiresGCM) {
		t.Errorf("New error = %v, want errors.Is(err, errObserverRequiresGCM)", err)
	}
}

// fakeProtocolServer is a protocolServer whose Run returns immediately
// with a fixed error, independent of ctx: it simulates sep2srv.Server.Run
// exiting via its own errCh branch (a Serve failure unrelated to
// shutdown), which is NOT observable through the real listener from a
// test without reaching into core-internal fields. Used only to prove
// Embed.Run's notifier teardown does not depend on ctx ever being
// cancelled by the caller.
type fakeProtocolServer struct {
	addr string
	err  error
}

func (f *fakeProtocolServer) Run(_ context.Context) error {
	return f.err
}

func (f *fakeProtocolServer) Addr() string {
	return f.addr
}

// TestRunTearsDownNotifierWhenServeFailsIndependentOfCtxCancel is the
// regression test for the HIGH deadlock finding: previously, Run passed
// the caller's ctx directly to notifier.Start, so if srv.Run returned via
// its errCh branch (a Serve failure) rather than ctx.Done, the notifier's
// internal `<-ctx.Done()` never unblocked and Run hung forever on
// `<-notifierDone`. Run now derives notifyCtx := context.WithCancel(ctx)
// and cancels it immediately after srv.Run returns, on every path, so
// the notifier tears down regardless of why srv.Run exited.
func TestRunTearsDownNotifierWhenServeFailsIndependentOfCtxCancel(t *testing.T) {
	t.Parallel()

	stores := newStores()
	notifier := coresub.NewManager(stores.Subscriptions, 1, 1)

	wantErr := errors.New("fake serve failure, unrelated to ctx cancellation")
	e := &Embed{
		srv:      &fakeProtocolServer{addr: "127.0.0.1:0", err: wantErr},
		notifier: notifier,
		stores:   stores,
	}

	// ctx is deliberately never cancelled by this test: if Embed.Run
	// still depended on ctx.Done() to tear the notifier down, this
	// would hang until the select's timeout branch fires.
	ctx := context.Background()

	runErr := make(chan error, 1)
	go func() {
		runErr <- e.Run(ctx)
	}()

	select {
	case err := <-runErr:
		if !errors.Is(err, wantErr) {
			t.Fatalf("Run() error = %v, want %v", err, wantErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s when srv.Run failed independent of ctx cancel (notifier teardown deadlock regression)")
	}
}
