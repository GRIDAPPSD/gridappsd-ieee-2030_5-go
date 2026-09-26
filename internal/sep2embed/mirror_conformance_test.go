package sep2embed

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// The tests in this file lock the wire form of the MirrorUsagePoint
// function set as the embedded server actually serves it, asserting on
// the SERVED BYTES rather than on a Go struct. They exist because a
// non-conformant MirrorUsagePoint element order shipped to a live
// federation and every device failed GET /mup with "parse error in
// message body": the served document was well-formed XML and carried
// the right root element, so any assertion short of byte-level order
// would have passed while the deployment stayed broken.
//
// Schema authority is sep.xsd (IEEE 2030.5). The sequence these tests
// pin, for MirrorUsagePoint, is the concatenation of its base types:
//
//	IdentifiedObject (sep.xsd:5324): mRID (min 1), description, version
//	UsagePointBase   (sep.xsd:6571): roleFlags (min 1),
//	                                 serviceCategoryKind (min 1),
//	                                 status (min 1)
//	MirrorUsagePoint (sep.xsd:6472): deviceLFDI (min 1),
//	                                 MirrorMeterReading (0..n),
//	                                 postRate (0..1)
//
// and for MirrorUsagePointList (sep.xsd:6494), which extends List
// (sep.xsd:5360): required attributes all and results, optional href
// from Resource (sep.xsd:5393), optional pollRate, then a repeated
// MirrorUsagePoint child.

// mupTestDevice is one device's mTLS client, the canonical LFDI its
// certificate derives to, and the opaque URL index the server addresses it
// by, so a test can reach the device's own resources and assert on
// ownership-scoped behavior.
//
// lfdi and edevID are deliberately separate fields: lfdi is IDENTITY (what
// the ownership gate matches the presented certificate against) and edevID is
// ADDRESSING (the {id} segment of the device's URLs). They stopped being the
// same value once the URL-index scheme was introduced, and a test that used one for the other
// would be asserting the coupling that change removed.
type mupTestDevice struct {
	client *http.Client
	lfdi   string
	edevID string
}

// newMUPTestServer stands up an Embed whose registry contains one entry
// per requested device serial, each keyed on that device certificate's
// own derived LFDI, and returns the base URL plus a client per device.
// The CA is minted once into certDir by a throwaway Embed so that every
// device certificate below chains to the CA the server will trust.
func newMUPTestServer(t *testing.T, serials ...string) (string, []mupTestDevice) {
	t.Helper()
	baseURL, devices, _, _ := newEmbedTestServer(t, nil, serials...)
	return baseURL, devices
}

// newEmbedTestServer is newMUPTestServer with three additions a control-flow
// test needs: the Embed itself and the Registry it was seeded from (together
// these are what ApplyControlDelta needs to drive the same stores the
// listener serves), and a hook to adjust the Config before New sees it.
//
// tune runs on the fully-populated Config and may be nil. It exists so a test
// can vary one policy field, the issued-control interval for instance,
// without either duplicating this eighty-line harness or widening the
// signature again the next time a field is added.
func newEmbedTestServer(t *testing.T, tune func(*Config), serials ...string) (string, []mupTestDevice, *Embed, *registry.Registry) {
	t.Helper()

	certDir := t.TempDir()

	// First Embed mints the CA and server material into certDir; it is
	// never run, only constructed, so nothing listens on its behalf.
	seedReg := registry.New()
	if err := seedReg.AddBatch(fixtureEntries()); err != nil {
		t.Fatalf("AddBatch (ca mint): %v", err)
	}
	mintCtx, mintCancel := context.WithCancel(context.Background())
	if _, err := New(mintCtx, Config{
		Addr: "127.0.0.1:0", CertDir: certDir, ShutdownTimeout: time.Second,
		ResolveRegistrationPIN: testResolvePIN,
	}, seedReg); err != nil {
		mintCancel()
		t.Fatalf("New (ca mint): %v", err)
	}
	mintCancel()

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

	devices := make([]mupTestDevice, 0, len(serials))
	entries := make([]registry.Entry, 0, len(serials))
	for _, serial := range serials {
		devCertPEM, devKeyPEM, err := sep2cert.GenerateDeviceCert(caCert, caKey, sep2cert.DeviceCertOptions{
			DeviceType:  sep2cert.DeviceTypeGeneric,
			HWSerialNum: serial,
			IsTestCert:  true,
		})
		if err != nil {
			t.Fatalf("GenerateDeviceCert(%s): %v", serial, err)
		}
		cfg, err := sepTLS.NewCCMClientConfigFromPEM(devCertPEM, devKeyPEM, caCertPEM)
		if err != nil {
			t.Fatalf("NewCCMClientConfigFromPEM(%s): %v", serial, err)
		}
		// Trust is pinned via RootCAs above; only the hostname match is
		// skipped, because the test dials 127.0.0.1 by address.
		cfg.InsecureSkipVerify = true //nolint:gosec // trust pinned via RootCAs

		pair, err := tls.X509KeyPair(devCertPEM, devKeyPEM)
		if err != nil {
			t.Fatalf("X509KeyPair(%s): %v", serial, err)
		}
		lfdi := sepTLS.LFDI(pair.Leaf)

		// gotlsHTTPClient (mtls_ccm_test.go): the embedded server serves
		// CCM-8 only since core v0.20.0, a suite stdlib crypto/tls's
		// TLSClientConfig cannot negotiate.
		devices = append(devices, mupTestDevice{
			client: gotlsHTTPClient(cfg),
			lfdi:   lfdi,
		})
		entries = append(entries, registry.Entry{
			MRID: "mrid-" + serial,
			Name: "Device " + serial,
			LFDI: lfdi,
		})
	}

	reg := registry.New()
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := Config{
		Addr: "127.0.0.1:0", CertDir: certDir, ShutdownTimeout: time.Second,
		ResolveRegistrationPIN: testResolvePIN,
		// The seeded DERProgram policy is supplied here so this harness
		// serves what a default deployment serves. Without it the served
		// program would carry primacy 0 and no description, and the
		// conformance tests over these bytes would be pinning a shape no
		// operator ever sees.
		DefaultControl: testDefaultControlSnapshot(),
		DefaultProgram: testProgramSeed,
		// Same reasoning for the issued-control interval: the harness serves
		// a usable window, so a test asserting on control bytes sees a real
		// deployment's shape rather than the unserviceable zero value.
		DERControl: testControlSeed,
	}
	if tune != nil {
		tune(&cfg)
	}
	e, err := New(ctx, cfg, reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(5 * time.Second):
			t.Error("Embed.Run did not return within 5s of cancel")
		}
	})

	// The URL index is only assigned once seeding has run inside New, so it
	// is backfilled here rather than at client-construction time above.
	for i := range devices {
		devices[i].edevID = embedURLIndex(t, e, "mrid-"+serials[i])
	}

	return "https://" + e.Addr(), devices, e, reg
}

// getSEP2 performs a GET with the Accept header the reference CSIP
// client sends, and returns the status and the raw served bytes.
func getSEP2(t *testing.T, d mupTestDevice, url string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest(%s): %v", url, err)
	}
	req.Header.Set("Accept", "application/sep+xml")
	resp, err := d.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", url, err)
	}
	return resp.StatusCode, body
}

// postMirrorUsagePoint POSTs a MirrorUsagePoint document and returns the
// status, the Location header, and the served bytes. claimLFDI is written
// into the document's deviceLFDI element so a test can attempt to claim
// another device's identity.
//
// Location is returned because IEEE 2030.5-2018 section 10.11.3 rule (a)(3)
// makes it the ONLY output of a successful POST: the 201 carries no body
// and the EPRI reference client's process_response never
// reads a POST response body, it follows Location with a fresh GET. A test
// that wants to inspect what was actually created must do the same.
func postMirrorUsagePoint(t *testing.T, d mupTestDevice, baseURL, mrid, claimLFDI string) (int, string, []byte) {
	t.Helper()
	doc := `<MirrorUsagePoint xmlns="urn:ieee:std:2030.5:ns">` +
		`<mRID>` + mrid + `</mRID>` +
		`<roleFlags>03</roleFlags>` +
		`<serviceCategoryKind>0</serviceCategoryKind>` +
		`<status>1</status>` +
		`<deviceLFDI>` + claimLFDI + `</deviceLFDI>` +
		`</MirrorUsagePoint>`
	req, err := http.NewRequest(http.MethodPost, baseURL+"/mup", bytes.NewBufferString(doc))
	if err != nil {
		t.Fatalf("NewRequest POST /mup: %v", err)
	}
	req.Header.Set("Content-Type", "application/sep+xml")
	resp, err := d.client.Do(req)
	if err != nil {
		t.Fatalf("POST /mup: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read POST /mup body: %v", err)
	}
	return resp.StatusCode, resp.Header.Get("Location"), body
}

// rootElement decodes only the first start element of served bytes and
// returns its namespace and local name. It deliberately inspects the
// wire document rather than unmarshalling into a typed struct: a struct
// with an XMLName tag would silently accept a document whose root
// element name is wrong, which is exactly the defect being guarded.
func rootElement(t *testing.T, body []byte) (space, local string) {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("no start element in served bytes: %v\nbody=%s", err, body)
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Space, se.Name.Local
		}
	}
}

// assertOrder asserts that each needle appears in body and that they
// appear in exactly the given order, which is how an XML Schema
// sequence constrains a document.
func assertOrder(t *testing.T, body []byte, needles ...string) {
	t.Helper()
	prev := -1
	for _, n := range needles {
		i := bytes.Index(body, []byte(n))
		if i < 0 {
			t.Fatalf("served bytes are missing %q\nbody=%s", n, body)
		}
		if i <= prev {
			t.Fatalf("served bytes have %q out of schema sequence order\nbody=%s", n, body)
		}
		prev = i
	}
}

// TestGETMirrorUsagePointListServesListRootWhenEmpty pins the empty-list
// case: the root element is MirrorUsagePointList (sep.xsd:6494) in the
// IEEE 2030.5 namespace, and the List-required counts (sep.xsd:5360) are
// both zero rather than absent.
func TestGETMirrorUsagePointListServesListRootWhenEmpty(t *testing.T) {
	t.Parallel()

	baseURL, devices := newMUPTestServer(t, "mup-empty-001")
	dev := devices[0]

	status, body := getSEP2(t, dev, baseURL+"/mup")
	if status != http.StatusOK {
		t.Fatalf("GET /mup status = %d, want 200; body=%s", status, body)
	}

	space, local := rootElement(t, body)
	if local != "MirrorUsagePointList" {
		t.Fatalf("GET /mup root element = %q, want %q; body=%s", local, "MirrorUsagePointList", body)
	}
	if space != "urn:ieee:std:2030.5:ns" {
		t.Fatalf("GET /mup root namespace = %q, want %q", space, "urn:ieee:std:2030.5:ns")
	}

	// List requires all and results (sep.xsd:5360). An empty list must
	// carry them as "0", not omit them.
	if !bytes.Contains(body, []byte(`all="0"`)) {
		t.Errorf(`GET /mup empty list is missing all="0"; body=%s`, body)
	}
	if !bytes.Contains(body, []byte(`results="0"`)) {
		t.Errorf(`GET /mup empty list is missing results="0"; body=%s`, body)
	}
	if !bytes.Contains(body, []byte(`href="/mup"`)) {
		t.Errorf(`GET /mup empty list is missing href="/mup"; body=%s`, body)
	}
	if bytes.Contains(body, []byte("<MirrorUsagePoint ")) || bytes.Contains(body, []byte("<MirrorUsagePoint>")) {
		t.Errorf("GET /mup empty list contains a MirrorUsagePoint child; body=%s", body)
	}
}

// TestGETMirrorUsagePointListChildFollowsSchemaSequence is the direct
// regression for the live failure. A MirrorUsagePoint whose child
// elements are well-formed but out of the sep.xsd sequence order is
// rejected by a schema-validating client even though the document
// parses as XML and carries the correct root element.
func TestGETMirrorUsagePointListChildFollowsSchemaSequence(t *testing.T) {
	t.Parallel()

	baseURL, devices := newMUPTestServer(t, "mup-order-001")
	dev := devices[0]

	const mrid = "4DA1B1B4B1D0D0D0D0D0D0D0D0D0D0D0"
	status, _, body := postMirrorUsagePoint(t, dev, baseURL, mrid, dev.lfdi)
	if status != http.StatusCreated {
		t.Fatalf("POST /mup status = %d, want 201; body=%s", status, body)
	}

	status, listBody := getSEP2(t, dev, baseURL+"/mup")
	if status != http.StatusOK {
		t.Fatalf("GET /mup status = %d, want 200; body=%s", status, listBody)
	}

	_, local := rootElement(t, listBody)
	if local != "MirrorUsagePointList" {
		t.Fatalf("GET /mup root element = %q, want MirrorUsagePointList; body=%s", local, listBody)
	}
	if !bytes.Contains(listBody, []byte(`all="1"`)) || !bytes.Contains(listBody, []byte(`results="1"`)) {
		t.Errorf(`GET /mup with one member is missing all="1"/results="1"; body=%s`, listBody)
	}

	// The sequence: IdentifiedObject mRID (sep.xsd:5324), then
	// UsagePointBase roleFlags, serviceCategoryKind, status
	// (sep.xsd:6571), then MirrorUsagePoint deviceLFDI (sep.xsd:6479).
	assertOrder(t, listBody,
		"<MirrorUsagePointList",
		"<MirrorUsagePoint ",
		"<mRID>",
		"<roleFlags>",
		"<serviceCategoryKind>",
		"<status>",
		"<deviceLFDI>",
	)

	// sep.xsd:6472-6493 defines no MirrorMeterReadingListLink child for
	// MirrorUsagePoint; readings are carried inline as MirrorMeterReading.
	// An element absent from the schema fails a validating parser.
	if bytes.Contains(listBody, []byte("MirrorMeterReadingListLink")) {
		t.Errorf("GET /mup emits MirrorMeterReadingListLink, which sep.xsd:6472-6493 does not define; body=%s", listBody)
	}

	// roleFlags is RoleFlagsType, which derives from HexBinary16, so the
	// value must be whole octets. A bare "3" is a single nibble and is
	// not legal hexBinary.
	if !bytes.Contains(listBody, []byte("<roleFlags>03</roleFlags>")) {
		t.Errorf("GET /mup roleFlags is not octet-paired hexBinary (want 03); body=%s", listBody)
	}

	// The device's own LFDI must be the value served, not some other
	// device's and not a placeholder.
	if !bytes.Contains(listBody, []byte("<deviceLFDI>"+dev.lfdi+"</deviceLFDI>")) {
		t.Errorf("GET /mup deviceLFDI is not the owning device's LFDI; body=%s", listBody)
	}
}

// TestMirrorUsagePointAndRegistrationRootElementsDiffer guards the exact
// confusion observed live, where the body served for /mup was mistaken
// for the immediately preceding /rg response because their lengths
// happened to match. Two different resources must never serve the same
// root element.
func TestMirrorUsagePointAndRegistrationRootElementsDiffer(t *testing.T) {
	t.Parallel()

	baseURL, devices := newMUPTestServer(t, "mup-vs-rg-001")
	dev := devices[0]

	mupStatus, mupBody := getSEP2(t, dev, baseURL+"/mup")
	if mupStatus != http.StatusOK {
		t.Fatalf("GET /mup status = %d, want 200; body=%s", mupStatus, mupBody)
	}
	rgStatus, rgBody := getSEP2(t, dev, baseURL+"/edev/"+dev.edevID+"/rg")
	if rgStatus != http.StatusOK {
		t.Fatalf("GET /edev/{id}/rg status = %d, want 200; body=%s", rgStatus, rgBody)
	}

	_, mupRoot := rootElement(t, mupBody)
	_, rgRoot := rootElement(t, rgBody)

	if mupRoot != "MirrorUsagePointList" {
		t.Errorf("/mup root element = %q, want MirrorUsagePointList", mupRoot)
	}
	if rgRoot != "Registration" {
		t.Errorf("/rg root element = %q, want Registration", rgRoot)
	}
	if mupRoot == rgRoot {
		t.Fatalf("/mup and /rg served the same root element %q; they are different resources", mupRoot)
	}
	// The live symptom was the /mup body being byte-identical to /rg.
	if bytes.Equal(mupBody, rgBody) {
		t.Fatalf("/mup and /rg served byte-identical bodies; body=%s", mupBody)
	}
}

// TestPOSTMirrorUsagePointStampsCallerLFDIOverClaimedValue asserts the
// ownership invariant that already governs this function set: the
// server derives deviceLFDI from the caller's verified client
// certificate and overrides whatever the document claimed. Without
// this, any device could publish metering data attributed to another
// device.
func TestPOSTMirrorUsagePointStampsCallerLFDIOverClaimedValue(t *testing.T) {
	t.Parallel()

	baseURL, devices := newMUPTestServer(t, "mup-owner-a", "mup-owner-b")
	deviceA, deviceB := devices[0], devices[1]

	if deviceA.lfdi == deviceB.lfdi {
		t.Fatalf("test setup is degenerate: both devices derived the same LFDI %q", deviceA.lfdi)
	}

	// Device B POSTs a MirrorUsagePoint claiming device A's LFDI.
	const mrid = "5EB2C2C5C2E1E1E1E1E1E1E1E1E1E1E1"
	status, location, body := postMirrorUsagePoint(t, deviceB, baseURL, mrid, deviceA.lfdi)
	if status != http.StatusCreated {
		t.Fatalf("POST /mup status = %d, want 201; body=%s", status, body)
	}

	// Section 10.11.3 rule (a)(3): the 201 carries the Location header and no
	// body. Assert both halves, so a regression that starts
	// echoing the created resource back is caught here rather than by a
	// strict client in the field.
	if len(bytes.TrimSpace(body)) != 0 {
		t.Errorf("POST /mup 201 carried a body, want none per section 10.11.3 rule (a)(3); body=%s", body)
	}
	if location == "" {
		t.Fatal("POST /mup 201 has no Location header; a client has no way to reach the created resource")
	}

	// The created resource must be attributed to device B, the actual
	// authenticated caller, never to the LFDI it tried to claim. Read it
	// back at its own URL, the way the EPRI client does, since the POST no
	// longer echoes it.
	status, created := getSEP2(t, deviceB, baseURL+location)
	if status != http.StatusOK {
		t.Fatalf("GET %s (Location from POST) status = %d, want 200; body=%s", location, status, created)
	}
	if !bytes.Contains(created, []byte("<deviceLFDI>"+deviceB.lfdi+"</deviceLFDI>")) {
		t.Errorf("created MirrorUsagePoint did not stamp the caller's own LFDI; body=%s", created)
	}
	if bytes.Contains(created, []byte(deviceA.lfdi)) {
		t.Errorf("created MirrorUsagePoint carries the claimed foreign LFDI, allowing identity spoofing; body=%s", created)
	}

	// The stored resource, read back, must carry the same attribution:
	// the override has to survive persistence, not just the response.
	status, listBody := getSEP2(t, deviceB, baseURL+"/mup")
	if status != http.StatusOK {
		t.Fatalf("GET /mup status = %d, want 200; body=%s", status, listBody)
	}
	if !bytes.Contains(listBody, []byte("<deviceLFDI>"+deviceB.lfdi+"</deviceLFDI>")) {
		t.Errorf("stored MirrorUsagePoint lost the caller-derived LFDI; body=%s", listBody)
	}
	if strings.Contains(string(listBody), deviceA.lfdi) {
		t.Errorf("stored MirrorUsagePoint is attributed to a device that did not create it; body=%s", listBody)
	}
}

// TestCrossDeviceEndDeviceScopedResourcesRemainOwnerGated confirms the
// /mup work did not weaken the existing per-device ownership gate on
// /edev/{id}-scoped resources. Device B must not be able to read device
// A's Registration, and the denial must not leak the resource body.
func TestCrossDeviceEndDeviceScopedResourcesRemainOwnerGated(t *testing.T) {
	t.Parallel()

	baseURL, devices := newMUPTestServer(t, "mup-acl-a", "mup-acl-b")
	deviceA, deviceB := devices[0], devices[1]

	status, body := getSEP2(t, deviceB, baseURL+"/edev/"+deviceA.edevID+"/rg")
	if status != http.StatusForbidden {
		t.Fatalf("cross-device GET /edev/{other}/rg status = %d, want 403; body=%s", status, body)
	}
	if bytes.Contains(body, []byte("<Registration")) || bytes.Contains(body, []byte("pIN")) {
		t.Fatalf("403 response leaked Registration content across a device boundary")
	}

	// The owner still reads its own Registration, so the gate denies by
	// ownership rather than by denying everyone.
	status, ownBody := getSEP2(t, deviceA, baseURL+"/edev/"+deviceA.edevID+"/rg")
	if status != http.StatusOK {
		t.Fatalf("owner GET of its own /rg status = %d, want 200; body=%s", status, ownBody)
	}
	if _, local := rootElement(t, ownBody); local != "Registration" {
		t.Fatalf("owner /rg root element = %q, want Registration", local)
	}
}
