package sep2embed

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// setupProductionWiringTest mints a device identity, seeds the registry with
// it, starts the embed under cfg (Addr, CertDir, ResolveRegistrationPIN and
// ShutdownTimeout are set here; the caller sets any other field, such as
// NotifyAllowLoopback), and registers the resulting client's single
// EndDevice. It returns the authenticated client, the EndDevice's href (the
// relative resource path postSubscription needs for SubscribedResource) and
// the subscription creation URL, leaving the subscription-creation
// assertions to the caller.
func setupProductionWiringTest(t *testing.T, cfg Config, serial, mrid, name string) (client *http.Client, edevHref, subCreateURL string) {
	t.Helper()

	certDir := t.TempDir()
	_, _, caFile, err := ensureServerIdentity(certDir, DeviceCertModeDevMint)
	if err != nil {
		t.Fatalf("ensureServerIdentity: %v", err)
	}
	caCertPEM, err := os.ReadFile(caFile)
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
	callerCertPEM, callerKeyPEM, callerLFDI := mintDeviceIdentity(t, caCert, caKey, serial)

	reg := registry.New()
	if err := reg.AddBatch([]registry.Entry{{MRID: mrid, Name: name, LFDI: callerLFDI}}); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	cfg.Addr = "127.0.0.1:0"
	cfg.CertDir = certDir
	cfg.ResolveRegistrationPIN = testResolvePIN
	cfg.ShutdownTimeout = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	e, err := New(ctx, cfg, reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runErr:
			if err != nil {
				t.Errorf("Run returned error after ctx cancel: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Run did not return within 3s of ctx cancel")
		}
	})

	baseURL := "https://" + e.Addr()
	client = deviceClient(t, callerCertPEM, callerKeyPEM, caCertPEM)

	edevResp, err := client.Get(baseURL + "/edev")
	if err != nil {
		t.Fatalf("GET /edev: %v", err)
	}
	edevBody, err := io.ReadAll(edevResp.Body)
	edevResp.Body.Close()
	if err != nil {
		t.Fatalf("read /edev body: %v", err)
	}
	var list sep2.EndDeviceList
	if err := xml.Unmarshal(edevBody, &list); err != nil {
		t.Fatalf("unmarshal EndDeviceList: %v\nbody=%s", err, edevBody)
	}
	if len(list.EndDevice) != 1 {
		t.Fatalf("EndDeviceList has %d items, want 1", len(list.EndDevice))
	}
	edevHref = list.EndDevice[0].Href
	if edevHref == "" {
		t.Fatal("EndDevice[0].Href is empty")
	}
	subCreateURL = baseURL + edevHref + "/sub"
	return client, edevHref, subCreateURL
}

// postSubscription POSTs a Subscription naming uri as its notificationURI to
// subCreateURL, with edevHref (the relative resource path setupProductionWiringTest
// returned) as SubscribedResource, and returns the raw response. Shared by
// both production-wiring tests below.
func postSubscription(t *testing.T, client *http.Client, subCreateURL, edevHref, uri string) *http.Response {
	t.Helper()
	body, err := xml.Marshal(&sep2.Subscription{
		SubscribedResource: edevHref,
		NotificationURI:    uri,
		Encoding:           sep2.EncodingXML,
	})
	if err != nil {
		t.Fatalf("marshal Subscription: %v", err)
	}
	resp, err := client.Post(subCreateURL, "application/sep+xml", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", subCreateURL, err)
	}
	return resp
}

// TestSubscriptionCreationThroughProductionWiringRefusesLoopback drives
// POST /edev/{id}/sub through the notifier sep2embed.New actually builds
// (embed.go's buildNotifier call, which always passes a DestinationPolicy via
// coresub.WithDestinationPolicy), not a hand-built Manager: with the switch
// off, the policy's AllowLoopback is false, so a loopback notificationURI
// must be refused with 400 and stored nowhere, and a private-range URI must
// be accepted with 201 and stored.
func TestSubscriptionCreationThroughProductionWiringRefusesLoopback(t *testing.T) {
	t.Parallel()

	client, edevHref, subCreateURL := setupProductionWiringTest(t, Config{}, "test-serial-subwiring-001", "mrid-subwiring", "Sub Wiring Device")

	loopbackResp := postSubscription(t, client, subCreateURL, edevHref, "http://127.0.0.1:9/notify")
	_, _ = io.Copy(io.Discard, loopbackResp.Body)
	loopbackResp.Body.Close()
	if loopbackResp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST with loopback notificationURI: status = %d, want %d", loopbackResp.StatusCode, http.StatusBadRequest)
	}

	privateResp := postSubscription(t, client, subCreateURL, edevHref, "http://10.0.0.5/notify")
	privateBody, err := io.ReadAll(privateResp.Body)
	privateResp.Body.Close()
	if err != nil {
		t.Fatalf("read POST (private-range) body: %v", err)
	}
	if privateResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST with private-range notificationURI: status = %d, want %d; body=%s", privateResp.StatusCode, http.StatusCreated, privateBody)
	}
	var created sep2.Subscription
	if err := xml.Unmarshal(privateBody, &created); err != nil {
		t.Fatalf("unmarshal created Subscription: %v\nbody=%s", err, privateBody)
	}
	if created.NotificationURI != "http://10.0.0.5/notify" {
		t.Errorf("created Subscription.NotificationURI = %q, want %q", created.NotificationURI, "http://10.0.0.5/notify")
	}

	listResp, err := client.Get(subCreateURL)
	if err != nil {
		t.Fatalf("GET %s: %v", subCreateURL, err)
	}
	listBody, err := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if err != nil {
		t.Fatalf("read subscription list body: %v", err)
	}
	var subList sep2.SubscriptionList
	if err := xml.Unmarshal(listBody, &subList); err != nil {
		t.Fatalf("unmarshal SubscriptionList: %v\nbody=%s", err, listBody)
	}
	if int(subList.All) != 1 {
		t.Fatalf("SubscriptionList.All = %d, want 1 (only the accepted private-range subscription is stored)", subList.All)
	}
	if len(subList.Subscription) != 1 {
		t.Fatalf("SubscriptionList has %d items, want 1", len(subList.Subscription))
	}
	if subList.Subscription[0].NotificationURI != "http://10.0.0.5/notify" {
		t.Errorf("stored Subscription.NotificationURI = %q, want %q", subList.Subscription[0].NotificationURI, "http://10.0.0.5/notify")
	}
}

// TestSubscriptionCreationThroughProductionWiringAllowsLoopbackWhenConfigured
// drives POST /edev/{id}/sub through the production notifier wiring with
// Config.NotifyAllowLoopback set, mirroring
// TestSubscriptionCreationThroughProductionWiringRefusesLoopback for the
// opposite state: a loopback notificationURI must be accepted (201) and
// stored, and a private-range URI must still be accepted (201) alongside
// it, per issue 86's acceptance criteria.
func TestSubscriptionCreationThroughProductionWiringAllowsLoopbackWhenConfigured(t *testing.T) {
	t.Parallel()

	client, edevHref, subCreateURL := setupProductionWiringTest(t, Config{NotifyAllowLoopback: true}, "test-serial-subwiring-002", "mrid-subwiring-2", "Sub Wiring Device 2")

	loopbackResp := postSubscription(t, client, subCreateURL, edevHref, "http://127.0.0.1:9/notify")
	loopbackBody, err := io.ReadAll(loopbackResp.Body)
	loopbackResp.Body.Close()
	if err != nil {
		t.Fatalf("read POST (loopback) body: %v", err)
	}
	if loopbackResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST with loopback notificationURI, NotifyAllowLoopback=true: status = %d, want %d; body=%s",
			loopbackResp.StatusCode, http.StatusCreated, loopbackBody)
	}
	var createdLoopback sep2.Subscription
	if err := xml.Unmarshal(loopbackBody, &createdLoopback); err != nil {
		t.Fatalf("unmarshal created (loopback) Subscription: %v\nbody=%s", err, loopbackBody)
	}
	if createdLoopback.NotificationURI != "http://127.0.0.1:9/notify" {
		t.Errorf("created (loopback) Subscription.NotificationURI = %q, want %q",
			createdLoopback.NotificationURI, "http://127.0.0.1:9/notify")
	}

	privateResp := postSubscription(t, client, subCreateURL, edevHref, "http://10.0.0.5/notify")
	privateBody, err := io.ReadAll(privateResp.Body)
	privateResp.Body.Close()
	if err != nil {
		t.Fatalf("read POST (private-range) body: %v", err)
	}
	if privateResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST with private-range notificationURI, NotifyAllowLoopback=true: status = %d, want %d; body=%s",
			privateResp.StatusCode, http.StatusCreated, privateBody)
	}
	var createdPrivate sep2.Subscription
	if err := xml.Unmarshal(privateBody, &createdPrivate); err != nil {
		t.Fatalf("unmarshal created (private-range) Subscription: %v\nbody=%s", err, privateBody)
	}
	if createdPrivate.NotificationURI != "http://10.0.0.5/notify" {
		t.Errorf("created (private-range) Subscription.NotificationURI = %q, want %q",
			createdPrivate.NotificationURI, "http://10.0.0.5/notify")
	}

	listResp, err := client.Get(subCreateURL)
	if err != nil {
		t.Fatalf("GET %s: %v", subCreateURL, err)
	}
	listBody, err := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if err != nil {
		t.Fatalf("read subscription list body: %v", err)
	}
	var subList sep2.SubscriptionList
	if err := xml.Unmarshal(listBody, &subList); err != nil {
		t.Fatalf("unmarshal SubscriptionList: %v\nbody=%s", err, listBody)
	}
	if int(subList.All) != 2 {
		t.Fatalf("SubscriptionList.All = %d, want 2 (both the loopback and the private-range subscription are stored)", subList.All)
	}
	stored := make(map[string]bool, len(subList.Subscription))
	for _, s := range subList.Subscription {
		stored[s.NotificationURI] = true
	}
	if !stored["http://127.0.0.1:9/notify"] {
		t.Errorf("stored subscriptions %v missing the loopback notificationURI %q", subList.Subscription, "http://127.0.0.1:9/notify")
	}
	if !stored["http://10.0.0.5/notify"] {
		t.Errorf("stored subscriptions %v missing the private-range notificationURI %q", subList.Subscription, "http://10.0.0.5/notify")
	}
}
