package main

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-go/fieldbus"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/gridappsdclient"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// fakeControlBus is a minimal fieldbus.MessageBus test double: Subscribe
// records the handler and, once armed via deliver, invokes it exactly
// once with a caller-supplied body. Every other method is a no-op;
// runControlSubscriber only exercises Subscribe/Unsubscribe (via
// gridappsdclient.Subscriber).
type fakeControlBus struct {
	mu      sync.Mutex
	handler fieldbus.Handler
	tok     fieldbus.Token
}

var _ fieldbus.MessageBus = (*fakeControlBus)(nil)

func (f *fakeControlBus) Connect(context.Context) error { return nil }
func (f *fakeControlBus) Disconnect() error             { return nil }
func (f *fakeControlBus) IsConnected() bool             { return true }

func (f *fakeControlBus) Subscribe(_ context.Context, _ string, h fieldbus.Handler) (fieldbus.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tok++
	f.handler = h
	return f.tok, nil
}

func (f *fakeControlBus) Unsubscribe(context.Context, string, fieldbus.Token) error { return nil }

func (f *fakeControlBus) Send(context.Context, string, string, []byte) error { return nil }

func (f *fakeControlBus) GetResponse(context.Context, string, string, []byte) ([]byte, error) {
	return nil, errors.New("fakeControlBus: GetResponse not used by this test")
}

// deliver invokes the last-registered Subscribe handler with body, as if
// the broker had delivered a frame.
func (f *fakeControlBus) deliver(body []byte) {
	f.mu.Lock()
	h := f.handler
	f.mu.Unlock()
	if h == nil {
		return
	}
	h(nil, body)
}

// waitFor polls check every 10ms until it returns true or timeout
// elapses. The caller still asserts final state afterward so a failure
// message carries the actually-observed value, not just "timed out".
func waitFor(timeout time.Duration, check func() bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRunControlSubscriberAppliesDeltaToOwningDevice is the
// end-to-end wiring test: a diff.Message delivered on the control
// subscriber's destination is decoded, and its single forward
// difference is applied via embed.ApplyControlDelta, landing a real
// DERControl on the owning device's own embedded server, verified over
// live mTLS with that device's own certificate.
//
// The device's cert-derived LFDI cannot be chosen up front (LFDI
// derivation is one-way), so this test mints the CA first (by booting a
// throwaway embed against an empty registry, exactly as
// TestBridgeWiresSEP2EmbedServesSeededDeviceOverMTLS's certDir setup
// does), mints a device cert against that CA, derives its real LFDI,
// and only then builds the real embed seeded with a registry entry
// carrying that exact LFDI.
func TestRunControlSubscriberAppliesDeltaToOwningDevice(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()

	// Step 1: provision the CA by booting a throwaway embed against an
	// empty registry, then stop it immediately.
	{
		bootstrapCtx, bootstrapCancel := context.WithCancel(context.Background())
		bootstrapEmbed, err := newSEP2Embed(bootstrapCtx, config{
			SEP2ServerAddr:    "127.0.0.1:0",
			SEP2ServerCertDir: certDir,
		}, registry.New(), testPolicyWithPIN(), nil)
		if err != nil {
			bootstrapCancel()
			t.Fatalf("newSEP2Embed (CA bootstrap): %v", err)
		}
		bootstrapDone := make(chan struct{})
		go func() {
			_ = bootstrapEmbed.Run(bootstrapCtx)
			close(bootstrapDone)
		}()
		bootstrapCancel()
		select {
		case <-bootstrapDone:
		case <-time.After(3 * time.Second):
			t.Fatal("CA-bootstrap embed did not shut down within 3s")
		}
	}

	// Step 2: mint a device cert against that CA and derive its real LFDI.
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
		HWSerialNum: "test-serial-control-subscriber-001",
		IsTestCert:  true,
	})
	if err != nil {
		t.Fatalf("GenerateDeviceCert: %v", err)
	}
	devLeaf, err := sep2cert.ParseCertificatePEM(devCertPEM)
	if err != nil {
		t.Fatalf("ParseCertificatePEM(device cert): %v", err)
	}
	deviceLFDI := sepTLS.LFDI(devLeaf)

	// Step 3: build the real embed seeded with a registry entry using
	// the minted device's exact LFDI, and run it.
	reg := registry.New()
	const deviceMRID = "mrid-control-test-1"
	if err := reg.Add(registry.Entry{MRID: deviceMRID, LFDI: deviceLFDI}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	embed, err := newSEP2Embed(ctx, config{
		SEP2ServerAddr:    "127.0.0.1:0",
		SEP2ServerCertDir: certDir,
	}, reg, testPolicyWithPIN(), nil)
	if err != nil {
		t.Fatalf("newSEP2Embed: %v", err)
	}
	embedRunErr := make(chan error, 1)
	go func() { embedRunErr <- embed.Run(ctx) }()

	bus := &fakeControlBus{}
	subErr := make(chan error, 1)
	var hook controlobs.Hook
	// The unsupervised gridappsdclient.Subscriber is deliberate here:
	// this test covers control-delta decode and apply, not subscription
	// health. Supervisor behavior is covered by the Supervisor tests
	// in internal/gridappsdclient.
	go func() {
		subErr <- runControlSubscriber(ctx, gridappsdclient.NewSubscriber(bus), embed, reg, "sim-1", &hook)
	}()

	waitFor(2*time.Second, func() bool {
		bus.mu.Lock()
		defer bus.mu.Unlock()
		return bus.handler != nil
	})
	bus.mu.Lock()
	registered := bus.handler != nil
	bus.mu.Unlock()
	if !registered {
		t.Fatal("control subscriber never called Subscribe")
	}

	b := diff.NewBuilder("sim-1")
	if err := b.AddDifference(deviceMRID, "DERControl.DERControlBase.opModTargetW",
		map[string]any{"multiplier": 0.0, "value": 4200.0},
		map[string]any{"multiplier": 0.0, "value": 4200.0},
	); err != nil {
		t.Fatalf("AddDifference: %v", err)
	}
	body, err := b.BytesNow()
	if err != nil {
		t.Fatalf("BytesNow: %v", err)
	}
	bus.deliver(body)

	// Verify the DERControl actually landed, over real mTLS as the
	// owning device would see it.
	tlsCfg, err := sepTLS.NewClientTLSConfigFromPEM(devCertPEM, devKeyPEM, caCertPEM)
	if err != nil {
		t.Fatalf("NewClientTLSConfigFromPEM: %v", err)
	}
	tlsCfg.InsecureSkipVerify = true //nolint:gosec // trust pinned via RootCAs above; only hostname match is skipped, matching this package's other mTLS test clients
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}, Timeout: 5 * time.Second}

	baseURL := "https://" + embed.Addr()

	// Resource URLs address a device by its opaque server-assigned index
	// rather than by its LFDI, so the {id} segment has to
	// be discovered rather than built from deviceLFDI. The EndDevices
	// snapshot is the supported way out of this package: it carries the
	// device's addressing ID alongside its identity LFDI.
	snaps, err := embed.EndDevices(context.Background())
	if err != nil {
		t.Fatalf("EndDevices: %v", err)
	}
	edevID := ""
	for _, s := range snaps {
		if s.LFDI == deviceLFDI {
			edevID = s.ID
			break
		}
	}
	if edevID == "" {
		t.Fatalf("no seeded EndDevice for LFDI %q; cannot address its resources", deviceLFDI)
	}

	var list sep2.DERControlList
	waitFor(3*time.Second, func() bool {
		resp, err := client.Get(baseURL + "/edev/" + edevID + "/fsa/1/derp/1/derc")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return false
		}
		if err := xml.Unmarshal(respBody, &list); err != nil {
			return false
		}
		return len(list.DERControl) == 1
	})

	if len(list.DERControl) != 1 {
		t.Fatalf("DERControlList has %d entries, want 1", len(list.DERControl))
	}
	base := list.DERControl[0].DERControlBase
	if base == nil || base.OpModTargetW == nil || base.OpModTargetW.Value != 4200 {
		t.Errorf("applied control OpModTargetW = %+v, want Value=4200", base)
	}

	// The observation hook must have recorded this same delta
	// as applied, with zero skips, since the delta is well formed and
	// targets a real, registered device.
	snap := hook.Snapshot()
	if snap.Applied != 1 {
		t.Errorf("hook.Snapshot().Applied = %d, want 1", snap.Applied)
	}
	if snap.Skipped != 0 {
		t.Errorf("hook.Snapshot().Skipped = %d, want 0", snap.Skipped)
	}
	if snap.Last == nil || snap.Last.Object != deviceMRID {
		t.Errorf("hook.Snapshot().Last = %+v, want Object=%q", snap.Last, deviceMRID)
	}

	cancel()
	select {
	case err := <-subErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("runControlSubscriber returned non-graceful error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runControlSubscriber did not return within 3s of ctx cancel")
	}
	select {
	case err := <-embedRunErr:
		if err != nil {
			t.Errorf("embed.Run returned error after ctx cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("embed.Run did not return within 3s of ctx cancel")
	}
}
