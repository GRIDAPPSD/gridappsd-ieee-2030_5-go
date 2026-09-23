package sep2embed

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

func TestEnsureServerIdentityMintsWhenAbsent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certDir := filepath.Join(dir, "certs") // does not exist yet; MkdirAll must create it

	certFile, keyFile, caFile, err := ensureServerIdentity(certDir, DeviceCertModeDevMint)
	if err != nil {
		t.Fatalf("ensureServerIdentity: %v", err)
	}

	if certFile != filepath.Join(certDir, serverCertFileName) {
		t.Errorf("certFile = %q, want %q", certFile, filepath.Join(certDir, serverCertFileName))
	}
	if keyFile != filepath.Join(certDir, serverKeyFileName) {
		t.Errorf("keyFile = %q, want %q", keyFile, filepath.Join(certDir, serverKeyFileName))
	}
	if caFile != filepath.Join(certDir, caCertFileName) {
		t.Errorf("caFile = %q, want %q", caFile, filepath.Join(certDir, caCertFileName))
	}

	// All six files exist (the two CA key files are the ones not
	// returned): #118 mints a device CA and a serving CA, not one CA.
	caKeyFile := filepath.Join(certDir, caKeyFileName)
	servingCAFile := filepath.Join(certDir, servingCACertFileName)
	servingCAKeyFile := filepath.Join(certDir, servingCAKeyFileName)
	for _, p := range []string{certFile, keyFile, caFile, caKeyFile, servingCAFile, servingCAKeyFile} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("Stat(%q): %v", p, err)
		}
		if perm := info.Mode().Perm(); perm != certFilePerm {
			t.Errorf("Stat(%q).Mode().Perm() = %o, want %o", p, perm, certFilePerm)
		}
	}

	dirInfo, err := os.Stat(certDir)
	if err != nil {
		t.Fatalf("Stat(certDir): %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != certDirPerm {
		t.Errorf("Stat(certDir).Mode().Perm() = %o, want %o", perm, certDirPerm)
	}

	// The minted material must actually parse as valid CA certificates
	// and a CA-signed server leaf: load-bearing for New's downstream
	// sep2tls.NewServerTLSConfig call, not just "files exist".
	caCertPEM, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("ReadFile(caFile): %v", err)
	}
	caCert, err := sep2cert.ParseCertificatePEM(caCertPEM)
	if err != nil {
		t.Fatalf("ParseCertificatePEM(ca): %v", err)
	}
	if !caCert.IsCA {
		t.Errorf("minted device CA cert has IsCA = false")
	}

	servingCACertPEM, err := os.ReadFile(servingCAFile)
	if err != nil {
		t.Fatalf("ReadFile(servingCAFile): %v", err)
	}
	servingCACert, err := sep2cert.ParseCertificatePEM(servingCACertPEM)
	if err != nil {
		t.Fatalf("ParseCertificatePEM(servingCA): %v", err)
	}
	if !servingCACert.IsCA {
		t.Errorf("minted serving CA cert has IsCA = false")
	}

	serverCertPEM, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("ReadFile(certFile): %v", err)
	}
	serverCert, err := sep2cert.ParseCertificatePEM(serverCertPEM)
	if err != nil {
		t.Fatalf("ParseCertificatePEM(server): %v", err)
	}
	// #118: the server's own leaf is signed by the SERVING CA, not the
	// device CA that caFile returns; see
	// TestEnsureServerIdentityMintsCrossCheckedCAs for the negative half
	// of this assertion (the leaf must NOT verify under the device CA).
	if err := serverCert.CheckSignatureFrom(servingCACert); err != nil {
		t.Errorf("minted server cert is not signed by the minted serving CA: %v", err)
	}
}

// TestEnsureServerIdentityMintsCrossCheckedCAs is the regression test
// for #118 done-when criterion 2: a directory holding both CAs produces
// a serving certificate under the serving CA and device certificates
// under the device CA, each asserted against the CA that did NOT sign
// it, so a regression back to one CA signing everything fails this test
// even though TestEnsureServerIdentityMintsWhenAbsent's positive checks
// alone would not catch it.
func TestEnsureServerIdentityMintsCrossCheckedCAs(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()

	certFile, _, caFile, err := ensureServerIdentity(certDir, DeviceCertModeDevMint)
	if err != nil {
		t.Fatalf("ensureServerIdentity: %v", err)
	}
	identities, err := EnsureDeviceIdentities(certDir, DeviceCertModeDevMint, []string{"device-1"})
	if err != nil {
		t.Fatalf("EnsureDeviceIdentities: %v", err)
	}
	if _, ok := identities["device-1"]; !ok {
		t.Fatalf("EnsureDeviceIdentities did not mint device-1")
	}

	deviceCACert := readCert(t, caFile)
	servingCACert := readCert(t, filepath.Join(certDir, servingCACertFileName))
	serverCert := readCert(t, certFile)
	deviceBase, err := deviceCertFileBase("device-1")
	if err != nil {
		t.Fatalf("deviceCertFileBase: %v", err)
	}
	deviceCert := readDERCert(t, filepath.Join(certDir, deviceCertDirName, deviceBase+".x509"))

	if err := serverCert.CheckSignatureFrom(servingCACert); err != nil {
		t.Errorf("server cert must verify under the serving CA: %v", err)
	}
	if err := serverCert.CheckSignatureFrom(deviceCACert); err == nil {
		t.Errorf("server cert must NOT verify under the device CA once the CAs are split")
	}

	if err := deviceCert.CheckSignatureFrom(deviceCACert); err != nil {
		t.Errorf("device cert must verify under the device CA: %v", err)
	}
	if err := deviceCert.CheckSignatureFrom(servingCACert); err == nil {
		t.Errorf("device cert must NOT verify under the serving CA")
	}
}

// TestFreshMintServerAuthNeedsServingCAAnchor is #118 item 1's decision,
// pinned by a REAL TLS handshake against the production listener
// construction (New, the same one cmd/bridge runs), not by an offline
// CheckSignatureFrom chain check: it proves what a client actually
// experiences, with certificate verification genuinely on.
//
// It deliberately does not reuse dialCapturingServerLeaf or
// mintTestDeviceClient's InsecureSkipVerify pattern: the security lane's
// review of this PR found that flag disables ALL chain verification, not
// only the hostname match its comment claims, which is a pre-existing,
// out-of-scope defect (P6) and exactly why no in-repo test could see
// this issue. This test's tls.Config never sets it.
//
// The decision: a fresh mint keeps minting two distinct CAs (matching
// the design's section 11.5 and the existing cross-checked-CA test
// above), rather than collapsing to one CA on mint. A client holding
// only the device CA (ca.pem) genuinely cannot verify a freshly minted
// bridge; that is asserted below as the expected, not silent, half of
// this test. What #118 P2 required was making that impossible to miss:
// ensureServerIdentity's mint-path warning now names both CAs' roles
// explicitly (see certs.go), and the conformance harness and docs are
// updated in this same fix round to use the serving CA as the anchor a
// client is actually given.
func TestFreshMintServerAuthNeedsServingCAAnchor(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()

	reg := registry.New()
	if err := reg.AddBatch(fixtureEntries()); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e, err := New(ctx, Config{
		Addr:                   "127.0.0.1:0",
		CertDir:                certDir,
		DeviceCertMode:         DeviceCertModeDevMint,
		ResolveRegistrationPIN: testResolvePIN,
		ShutdownTimeout:        time.Second,
	}, reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()

	deviceCACertPEM, err := os.ReadFile(filepath.Join(certDir, caCertFileName))
	if err != nil {
		t.Fatalf("read %s: %v", caCertFileName, err)
	}
	servingCACertPEM, err := os.ReadFile(filepath.Join(certDir, servingCACertFileName))
	if err != nil {
		t.Fatalf("read %s: %v", servingCACertFileName, err)
	}

	// A probe device certificate, signed by the device CA the server just
	// loaded, so the server's own mTLS verification of the CLIENT accepts
	// it in both dial attempts below: only the client's verification of
	// the SERVER (via RootCAs) differs between them.
	identities, err := EnsureDeviceIdentities(certDir, DeviceCertModeDevMint, []string{"probe-device"})
	if err != nil {
		t.Fatalf("EnsureDeviceIdentities: %v", err)
	}
	if _, ok := identities["probe-device"]; !ok {
		t.Fatalf("EnsureDeviceIdentities did not mint probe-device")
	}
	deviceBase, err := deviceCertFileBase("probe-device")
	if err != nil {
		t.Fatalf("deviceCertFileBase: %v", err)
	}
	devCertDER, err := os.ReadFile(filepath.Join(certDir, deviceCertDirName, deviceBase+".x509"))
	if err != nil {
		t.Fatalf("read probe device cert: %v", err)
	}
	devKeyPEM, err := os.ReadFile(filepath.Join(certDir, deviceCertDirName, deviceBase+".pem"))
	if err != nil {
		t.Fatalf("read probe device key: %v", err)
	}
	devCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: devCertDER})
	deviceCert, err := tls.X509KeyPair(devCertPEM, devKeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair(probe device): %v", err)
	}

	dial := func(rootPEM []byte) error {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(rootPEM) {
			t.Fatalf("AppendCertsFromPEM: no certificate parsed from the %d-byte root PEM", len(rootPEM))
		}
		cfg := &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{deviceCert},
			MinVersion:   tls.VersionTLS12,
		}
		conn, dialErr := tls.Dial("tcp", e.Addr(), cfg)
		if dialErr == nil {
			conn.Close()
		}
		return dialErr
	}

	// The anchor the split actually gives a client (section 11.3 of the
	// design: "a device's own trust store holds the serving CA
	// certificate"). This must succeed against a bridge that just minted
	// fresh material, with no other configuration.
	if err := dial(servingCACertPEM); err != nil {
		t.Errorf("a client trusting the serving CA could not verify a freshly minted bridge: %v", err)
	}

	// The anchor a pre-split client held. This is the P2 finding's
	// stranding case, pinned here as an explicit, asserted expectation
	// rather than a silent gap: it must keep failing, loudly, with a
	// chain-trust error rather than succeeding by accident.
	err = dial(deviceCACertPEM)
	if err == nil {
		t.Fatal("a client trusting only the device CA verified a freshly minted bridge; the serving/device split must hold in both directions")
	}
	if !strings.Contains(err.Error(), "unknown authority") {
		t.Errorf("dial with the device CA failed for an unexpected reason, want a chain-trust (unknown authority) error: %v", err)
	}

	cancel()
	if err := <-runErr; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("Run: %v", err)
	}
}

// readCert reads and parses the PEM certificate at path, failing the
// test on any error. A small helper to keep the cross-check assertions
// above readable as a sequence of comparisons rather than a wall of
// read-and-parse boilerplate.
func readCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	cert, err := sep2cert.ParseCertificatePEM(pemBytes)
	if err != nil {
		t.Fatalf("ParseCertificatePEM(%q): %v", path, err)
	}
	return cert
}

// readDERCert is readCert's twin for a device certificate, which
// ensureDeviceCert writes as raw DER (the ".x509" extension), not PEM;
// see its doc comment for why.
func readDERCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	der, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate(%q): %v", path, err)
	}
	return cert
}

func TestEnsureServerIdentityLoadsWhenAllFourPresent(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()

	certFile1, keyFile1, caFile1, err := ensureServerIdentity(certDir, DeviceCertModeDevMint)
	if err != nil {
		t.Fatalf("first ensureServerIdentity: %v", err)
	}
	original, err := os.ReadFile(certFile1)
	if err != nil {
		t.Fatalf("ReadFile(certFile1): %v", err)
	}

	// Second call against the SAME directory must load, not re-mint: the
	// server cert bytes on disk must be byte-for-byte unchanged.
	certFile2, keyFile2, caFile2, err := ensureServerIdentity(certDir, DeviceCertModeDevMint)
	if err != nil {
		t.Fatalf("second ensureServerIdentity: %v", err)
	}
	if certFile1 != certFile2 || keyFile1 != keyFile2 || caFile1 != caFile2 {
		t.Fatalf("second call returned different paths: (%q,%q,%q) vs (%q,%q,%q)",
			certFile1, keyFile1, caFile1, certFile2, keyFile2, caFile2)
	}

	reloaded, err := os.ReadFile(certFile2)
	if err != nil {
		t.Fatalf("ReadFile(certFile2): %v", err)
	}
	if string(original) != string(reloaded) {
		t.Fatalf("server cert bytes changed across calls: load-or-create re-minted instead of loading")
	}
}

func TestWriteFileAtomicWritesCompleteFileAndCleansUpTemp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	want := []byte("all-or-nothing contents")

	if err := writeFileAtomic(path, want, certFilePerm); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("file contents = %q, want %q", got, want)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != certFilePerm {
		t.Errorf("Mode().Perm() = %o, want %o", perm, certFilePerm)
	}

	// No leftover temp file: the rename consumed it and the defer's
	// cleanup only fires on the non-renamed path.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "target.txt" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("dir contents = %v, want exactly [target.txt]", names)
	}
}

func TestWriteFileAtomicErrorsOnUnwritableDir(t *testing.T) {
	t.Parallel()

	// A path whose directory does not exist: os.CreateTemp must fail
	// before anything is written, and writeFileAtomic must surface that
	// error rather than silently succeeding.
	path := filepath.Join(t.TempDir(), "missing-subdir", "target.txt")

	if err := writeFileAtomic(path, []byte("x"), certFilePerm); err == nil {
		t.Fatal("writeFileAtomic into a nonexistent directory: want error, got nil")
	}
}

func TestParseCAPairRejectsInvalidPEM(t *testing.T) {
	t.Parallel()

	validCertPEM, validKeyPEM, err := sep2cert.GenerateCA(sep2cert.CAOptions{CommonName: "test"})
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	if _, _, err := parseCAPair([]byte("not a cert"), validKeyPEM); err == nil {
		t.Error("parseCAPair with invalid cert PEM: want error, got nil")
	}
	if _, _, err := parseCAPair(validCertPEM, []byte("not a key")); err == nil {
		t.Error("parseCAPair with invalid key PEM: want error, got nil")
	}
}

// TestParseCAPairRejectsMismatchedKey is the regression test for the
// defect the CA cert and key not being checked against each other: a
// key from one CA paired with a certificate from a different CA parses
// cleanly on both sides individually but must not be accepted as a
// pair, since a CA signing with that key could never be verified by
// that certificate.
func TestParseCAPairRejectsMismatchedKey(t *testing.T) {
	t.Parallel()

	certPEM, _, err := sep2cert.GenerateCA(sep2cert.CAOptions{CommonName: "ca-a"})
	if err != nil {
		t.Fatalf("GenerateCA(ca-a): %v", err)
	}
	_, otherKeyPEM, err := sep2cert.GenerateCA(sep2cert.CAOptions{CommonName: "ca-b"})
	if err != nil {
		t.Fatalf("GenerateCA(ca-b): %v", err)
	}

	_, _, err = parseCAPair(certPEM, otherKeyPEM)
	if err == nil {
		t.Fatal("parseCAPair with a key from a different CA: want error, got nil")
	}
	if !strings.Contains(err.Error(), caCertFileName) || !strings.Contains(err.Error(), caKeyFileName) {
		t.Errorf("error must name both %s and %s: %v", caCertFileName, caKeyFileName, err)
	}
}

// TestParseCAPairAcceptsMatchingPair is the companion assertion to
// TestParseCAPairRejectsMismatchedKey: the same check must not refuse a
// genuinely matching pair. Asserted both ways (error is nil AND the
// returned cert/key are the parsed pair, key.PublicKey.Equal the cert's
// public key) so the mismatch check cannot pass by refusing everything.
func TestParseCAPairAcceptsMatchingPair(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM, err := sep2cert.GenerateCA(sep2cert.CAOptions{CommonName: "ca-matching"})
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	cert, key, err := parseCAPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parseCAPair with a matching pair: want nil error, got %v", err)
	}
	if cert.Subject.CommonName != "ca-matching" {
		t.Errorf("cert.Subject.CommonName = %q, want %q", cert.Subject.CommonName, "ca-matching")
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("cert.PublicKey type = %T, want *ecdsa.PublicKey", cert.PublicKey)
	}
	if !key.PublicKey.Equal(pub) {
		t.Error("returned key does not match returned cert's public key")
	}
}

// TestEnsureServerIdentityRefusesPartiallyPopulatedDir replaces an
// earlier test that asserted the OPPOSITE: that a partially populated
// directory was re-minted into a fresh consistent set. That behavior is
// the defect. Re-minting reaches the same rename over any of the four
// names that happens to be occupied, and the file it lands on is, by
// definition, one an operator put there.
//
// The refusal holds even though this directory is writable.
func TestEnsureServerIdentityRefusesPartiallyPopulatedDir(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()

	// A partially populated directory: only the CA cert exists.
	if err := os.MkdirAll(certDir, certDirPerm); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	stub := []byte("operator material this process did not write")
	if err := os.WriteFile(filepath.Join(certDir, caCertFileName), stub, certFilePerm); err != nil {
		t.Fatalf("WriteFile stub ca.pem: %v", err)
	}

	_, _, _, err := ensureServerIdentity(certDir, DeviceCertModeDevMint)
	if err == nil {
		t.Fatal("ensureServerIdentity on a partially populated directory: want an error, got nil")
	}
	if !errors.Is(err, errCertDirPartial) {
		t.Errorf("error = %v, want one matching errCertDirPartial", err)
	}
	// The message must be actionable without reading source: it names
	// what is there and what is not.
	for _, want := range []string{caCertFileName, caKeyFileName, serverCertFileName, serverKeyFileName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message does not name %s: %v", want, err)
		}
	}

	// The byte assertion is the point: the operator's file is untouched.
	assertFileBytes(t, certDir, caCertFileName, stub)
	for _, name := range []string{caKeyFileName, serverCertFileName, serverKeyFileName} {
		assertNoFile(t, certDir, name)
	}
}

// TestEnsureServerIdentityRefusesHalfServingCAPair is
// TestEnsureServerIdentityRefusesPartiallyPopulatedDir's #118 companion:
// the original four files are COMPLETE, so the refusal here can only
// come from the serving CA pair being half-present, which is what "a
// directory missing one file of either pair refuses to start and names
// what is missing" (done-when criterion 3) requires of the NEW pair,
// not only the original one.
func TestEnsureServerIdentityRefusesHalfServingCAPair(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()
	material := writePreprovisionedServerMaterial(t, certDir)
	// writePreprovisionedServerMaterial deliberately omits ca-key.pem;
	// this test signs in DevMint mode, so put it back.
	if err := os.WriteFile(filepath.Join(certDir, caKeyFileName), []byte("stub-ca-key"), certFilePerm); err != nil {
		t.Fatalf("WriteFile ca-key.pem: %v", err)
	}
	stub := []byte("operator started splitting and stopped here")
	if err := os.WriteFile(filepath.Join(certDir, servingCACertFileName), stub, certFilePerm); err != nil {
		t.Fatalf("WriteFile stub serving-ca.pem: %v", err)
	}

	_, _, _, err := ensureServerIdentity(certDir, DeviceCertModeDevMint)
	if err == nil {
		t.Fatal("ensureServerIdentity with serving-ca.pem but no serving-ca-key.pem: want an error, got nil")
	}
	if !errors.Is(err, errCertDirPartial) {
		t.Errorf("error = %v, want one matching errCertDirPartial", err)
	}
	if !strings.Contains(err.Error(), servingCAKeyFileName) {
		t.Errorf("error message does not name the missing %s: %v", servingCAKeyFileName, err)
	}

	// Nothing was touched: neither the operator's original material nor
	// the serving CA cert the operator had already staged.
	assertFileBytes(t, certDir, caCertFileName, material.caCertPEM)
	assertFileBytes(t, certDir, serverCertFileName, material.serverCertPEM)
	assertFileBytes(t, certDir, serverKeyFileName, material.serverKeyPEM)
	assertFileBytes(t, certDir, servingCACertFileName, stub)
	assertNoFile(t, certDir, servingCAKeyFileName)
}

// requiredClause extracts the file list between "requires " and " in
// directory" from an incompleteCertDirError message, so a test can
// assert on exactly what the message claims is required without also
// matching the (deliberately different) "found" and "missing" clauses,
// which legitimately name files the message must not call required.
func requiredClause(t *testing.T, msg string) string {
	t.Helper()
	const start = "requires "
	const end = " in directory"
	i := strings.Index(msg, start)
	if i < 0 {
		t.Fatalf("message has no %q clause: %s", start, msg)
	}
	rest := msg[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("message has no %q terminator: %s", end, msg)
	}
	return rest[:j]
}

// TestEnsureServerIdentityPreprovisionedSpareCAKeyMessageNamesOnlyRequired
// is the regression test for #118 P3: incompleteCertDirError's "requires"
// clause used to be built as present union missing, and present is every
// managed file classifyCertDir found, required or not. A preprovisioned
// directory holding nothing but a spare ca-key.pem (left over from, say,
// a dev-mint directory copied by mistake) is missing every file the mode
// actually needs, but the old message also claimed ca-key.pem itself was
// required, telling an operator to put a CA signing key on a host that
// mode never reads it from.
func TestEnsureServerIdentityPreprovisionedSpareCAKeyMessageNamesOnlyRequired(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()
	if err := os.MkdirAll(certDir, certDirPerm); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	stub := []byte("a device CA key that does not belong on this host")
	if err := os.WriteFile(filepath.Join(certDir, caKeyFileName), stub, certFilePerm); err != nil {
		t.Fatalf("WriteFile stub ca-key.pem: %v", err)
	}

	_, _, _, err := ensureServerIdentity(certDir, DeviceCertModePreprovisioned)
	if err == nil {
		t.Fatal("ensureServerIdentity on a directory holding only a spare ca-key.pem: want an error, got nil")
	}
	// Preprovisioned mode never writes, so incompleteness of any shape
	// (partial or empty) reports errCertDirWriteForbidden, checked ahead
	// of the partial/empty switch in ensureServerIdentity; this is not
	// errCertDirPartial's case.
	if !errors.Is(err, errCertDirWriteForbidden) {
		t.Errorf("error = %v, want one matching errCertDirWriteForbidden", err)
	}

	want := "ca.pem, server.pem, server-key.pem"
	if got := requiredClause(t, err.Error()); got != want {
		t.Errorf("requires clause = %q, want %q (preprovisioned mode never reads %s, so it must not be told the directory requires it, even though it is present)",
			got, want, caKeyFileName)
	}

	// The spare key is still reported as found and untouched: this is a
	// message-accuracy fix, not a licence to hide or delete it.
	if !strings.Contains(err.Error(), "found "+caKeyFileName) {
		t.Errorf("error message does not report %s as found: %v", caKeyFileName, err)
	}
	assertFileBytes(t, certDir, caKeyFileName, stub)
}
