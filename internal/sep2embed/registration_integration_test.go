package sep2embed

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// TestRegistrationEndToEndOverMTLS walks the exact sequence the EPRI
// oeg_client performs and that failed live with "EndDevice does not
// contain RegistrationLink" on every device: GET /edev, read each
// EndDevice's RegistrationLink, then GET the resource behind that link.
//
// It asserts values rather than status codes alone: the advertised href
// string, the served Registration's fields, the cross-device denial, and
// the PIN's stability across two successive GETs of the same resource.
func TestRegistrationEndToEndOverMTLS(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()

	_, _, caFile, err := ensureServerIdentity(certDir)
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

	certA, keyA, lfdiA := mintDeviceIdentity(t, caCert, caKey, "test-serial-reg-a")
	certB, keyB, lfdiB := mintDeviceIdentity(t, caCert, caKey, "test-serial-reg-b")
	if lfdiA == lfdiB {
		t.Fatalf("minted devices A and B produced the same LFDI %q; test fixture is broken", lfdiA)
	}

	reg := registry.New()
	if err := reg.AddBatch([]registry.Entry{
		{MRID: "mrid-reg-a", Name: "Device A", LFDI: lfdiA},
		{MRID: "mrid-reg-b", Name: "Device B", LFDI: lfdiB},
	}); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Distinct per-device PINs, which is the provisioning shape IEEE
	// 2030.5 section 6.3.5 describes ("configurable on a device where
	// possible for registration purposes"). Both are obvious dummies that
	// satisfy 6.3.5's checksum rule: their six digits sum to a multiple
	// of ten. 123455 is the standard's own worked example.
	const pinA, pinB = uint32(123455), uint32(222220)
	pins := map[string]uint32{lfdiA: pinA, lfdiB: pinB}

	e, err := New(ctx, Config{
		Addr:    "127.0.0.1:0",
		CertDir: certDir,
		ResolveRegistrationPIN: func(lfdi string) (uint32, bool) {
			v, ok := pins[lfdi]
			return v, ok
		},
		ShutdownTimeout: time.Second,
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
			t.Error("Run did not return within 3s of ctx cancel (goroutine leak or unbounded shutdown)")
		}
	})

	baseURL := "https://" + e.Addr()
	clientA := deviceClient(t, certA, keyA, caCertPEM)
	clientB := deviceClient(t, certB, keyB, caCertPEM)

	// Addressing is the opaque URL index; lfdiA/lfdiB remain the identities
	// the ownership gate matches the presented certificates against.
	edevA := embedURLIndex(t, e, "mrid-reg-a")
	edevB := embedURLIndex(t, e, "mrid-reg-b")

	wantHrefA := "/edev/" + edevA + "/rg"

	// Step 1: the EndDeviceList the client walks must carry a
	// RegistrationLink for the calling device. This is the exact
	// condition the live client tested with se_exists(e, RegistrationLink)
	// and failed on. Assert against the raw XML, not a re-marshalled
	// struct, so an omitempty or ordering regression is visible.
	listStatus, listBody := getBody(t, clientA, baseURL+"/edev", "GET /edev as device A")
	if listStatus != http.StatusOK {
		t.Fatalf("GET /edev = %d, want 200", listStatus)
	}
	if !strings.Contains(listBody, "<RegistrationLink") {
		t.Fatalf("EndDeviceList carries no RegistrationLink element; this is the exact live failure (EPRI client aborts with \"EndDevice does not contain RegistrationLink\"):\n%s", listBody)
	}
	if wantAttr := `<RegistrationLink href="` + wantHrefA + `"`; !strings.Contains(listBody, wantAttr) {
		t.Errorf("EndDeviceList does not advertise device A's RegistrationLink href exactly as %q:\n%s", wantHrefA, listBody)
	}

	// The singleton EndDevice resource must advertise the same link.
	devStatus, devBody := getBody(t, clientA, baseURL+"/edev/"+edevA, "GET /edev/{a} as device A")
	if devStatus != http.StatusOK {
		t.Fatalf("GET /edev/{a} = %d, want 200", devStatus)
	}
	var dev sep2.EndDevice
	if err := xml.Unmarshal([]byte(devBody), &dev); err != nil {
		t.Fatalf("unmarshal EndDevice: %v\n%s", err, devBody)
	}
	if dev.RegistrationLink == nil {
		t.Fatalf("EndDevice singleton has no RegistrationLink:\n%s", devBody)
	}
	if dev.RegistrationLink.Href != wantHrefA {
		t.Errorf("EndDevice.RegistrationLink.Href = %q, want %q", dev.RegistrationLink.Href, wantHrefA)
	}

	// Step 2: follow the advertised link. The whole point of the link is
	// that it resolves, so drive the request from the served href rather
	// than a locally reconstructed path.
	regStatus, regBody := getBody(t, clientA, baseURL+dev.RegistrationLink.Href, "GET the advertised RegistrationLink as device A")
	if regStatus != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; the advertised link must resolve", dev.RegistrationLink.Href, regStatus)
	}
	var got sep2.Registration
	if err := xml.Unmarshal([]byte(regBody), &got); err != nil {
		t.Fatalf("unmarshal Registration: %v\n%s", err, regBody)
	}
	if got.Href != wantHrefA {
		t.Errorf("Registration.Href = %q, want %q", got.Href, wantHrefA)
	}
	if got.DateTimeRegistered <= 0 {
		t.Errorf("Registration.DateTimeRegistered = %d, want a positive epoch second", got.DateTimeRegistered)
	}
	if got.PIN > 999999 {
		t.Error("served Registration.PIN is outside the PINType range [0, 999999]")
	}
	// The served PIN must be the operator-supplied configured value,
	// stable across re-fetches. It must NOT be a function of the device's
	// LFDI: the LFDI is a public digest of the device certificate, so a
	// PIN derived from it would be computable by any peer.
	if got.PIN != pinA {
		t.Error("served Registration.PIN is not the value configured for this device")
	}
	// Sequence order on the served bytes, per sep.xsd complexType
	// "Registration": dateTimeRegistered then pIN.
	dtIdx := strings.Index(regBody, "<dateTimeRegistered>")
	pinIdx := strings.Index(regBody, "<pIN>")
	if dtIdx == -1 || pinIdx == -1 {
		t.Fatalf("served Registration is missing a required sequence element:\n%s", regBody)
	}
	if dtIdx > pinIdx {
		t.Errorf("served Registration element order violates the xsd:sequence (dateTimeRegistered must precede pIN):\n%s", regBody)
	}

	// Step 3: the PIN must not change between two GETs of the same
	// resource. A client that re-fetches (the EPRI client re-reads on
	// each poll cycle) must see a stable value.
	_, secondBody := getBody(t, clientA, baseURL+dev.RegistrationLink.Href, "second GET of device A's Registration")
	var second sep2.Registration
	if err := xml.Unmarshal([]byte(secondBody), &second); err != nil {
		t.Fatalf("unmarshal second Registration: %v", err)
	}
	if second.PIN != got.PIN {
		t.Error("Registration PIN changed between two successive GETs of the same resource")
	}
	if second.DateTimeRegistered != got.DateTimeRegistered {
		t.Errorf("Registration.DateTimeRegistered changed between two GETs: %d then %d", got.DateTimeRegistered, second.DateTimeRegistered)
	}

	// Step 4: ownership. Device B must not be able to read device A's
	// Registration, and the denial must not leak the resource.
	crossStatus, crossBody := getBody(t, clientB, baseURL+wantHrefA, "device B reading device A's Registration")
	if crossStatus != http.StatusForbidden {
		t.Errorf("cross-device GET of device A's Registration by device B = %d, want %d", crossStatus, http.StatusForbidden)
	}
	for _, leak := range []string{"<Registration", "<pIN>", "<dateTimeRegistered>"} {
		if strings.Contains(crossBody, leak) {
			t.Errorf("cross-device denial response leaked %q from the Registration resource", leak)
		}
	}
	// And the symmetric direction, so the gate is not one-sided.
	reverseStatus, reverseBody := getBody(t, clientA, baseURL+"/edev/"+edevB+"/rg", "device A reading device B's Registration")
	if reverseStatus != http.StatusForbidden {
		t.Errorf("cross-device GET of device B's Registration by device A = %d, want %d", reverseStatus, http.StatusForbidden)
	}
	if strings.Contains(reverseBody, "<Registration") {
		t.Error("reverse cross-device denial response leaked the Registration resource")
	}

	// Device B reading its OWN Registration still succeeds: the gate
	// denies the other device, it does not break the owner's access.
	ownBStatus, ownBBody := getBody(t, clientB, baseURL+"/edev/"+edevB+"/rg", "device B reading its own Registration")
	if ownBStatus != http.StatusOK {
		t.Fatalf("device B reading its own Registration = %d, want 200", ownBStatus)
	}
	var regB sep2.Registration
	if err := xml.Unmarshal([]byte(ownBBody), &regB); err != nil {
		t.Fatalf("unmarshal device B Registration: %v", err)
	}
	if regB.Href != "/edev/"+edevB+"/rg" {
		t.Errorf("device B Registration.Href = %q, want %q", regB.Href, "/edev/"+edevB+"/rg")
	}
	// Each device is served ITS OWN configured PIN. This is a per-device
	// configuration fact, not a derivation: section 6.3.5 exists because
	// the SFDI and LFDI "are derived from public information (i.e., a
	// Certificate), therefore can potentially be recreated by an
	// eavesdropper", so a PIN computed from device identity would defeat
	// the field's stated purpose.
	if regB.PIN != pinB {
		t.Error("device B was not served the PIN configured for device B")
	}
	if regB.PIN == got.PIN {
		t.Error("distinctly configured devices A and B were served the same registration PIN")
	}
}

// getBody issues a GET with client and returns the status code and the
// response body as a string. Body content is load-bearing in this file
// (href strings, element order, and absence-of-leak assertions all read
// the raw bytes), so it is returned rather than discarded.
func getBody(t *testing.T, client *http.Client, url, desc string) (int, string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("%s: build request: %v", desc, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", desc, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("%s: read body: %v", desc, err)
	}
	return resp.StatusCode, string(body)
}
