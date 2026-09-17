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

// TestSubscriptionCreationThroughProductionWiringRefusesLoopback drives
// POST /edev/{id}/sub through the notifier sep2embed.New actually builds
// (embed.go's coresub.NewManager call, with no destination-policy
// option), not a hand-built Manager: a loopback notificationURI must be
// refused with 400 and stored nowhere, and a private-range URI must be
// accepted with 201 and stored, matching the zero-value DestinationPolicy
// that ships in production.
func TestSubscriptionCreationThroughProductionWiringRefusesLoopback(t *testing.T) {
	t.Parallel()

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
	callerCertPEM, callerKeyPEM, callerLFDI := mintDeviceIdentity(t, caCert, caKey, "test-serial-subwiring-001")

	reg := registry.New()
	if err := reg.AddBatch([]registry.Entry{{MRID: "mrid-subwiring", Name: "Sub Wiring Device", LFDI: callerLFDI}}); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e, err := New(ctx, Config{
		Addr:                   "127.0.0.1:0",
		CertDir:                certDir,
		ResolveRegistrationPIN: testResolvePIN,
		ShutdownTimeout:        time.Second,
	}, reg)
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
	client := deviceClient(t, callerCertPEM, callerKeyPEM, caCertPEM)

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
	edevHref := list.EndDevice[0].Href
	if edevHref == "" {
		t.Fatal("EndDevice[0].Href is empty")
	}
	subCreateURL := baseURL + edevHref + "/sub"

	post := func(uri string) *http.Response {
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

	loopbackResp := post("http://127.0.0.1:9/notify")
	_, _ = io.Copy(io.Discard, loopbackResp.Body)
	loopbackResp.Body.Close()
	if loopbackResp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST with loopback notificationURI: status = %d, want %d", loopbackResp.StatusCode, http.StatusBadRequest)
	}

	privateResp := post("http://10.0.0.5/notify")
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
