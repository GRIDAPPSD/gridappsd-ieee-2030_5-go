package sep2embed

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"
)

// genTestCA mints a standalone, in-memory-only CA (never written to
// disk), for tests that need a CA distinct from the one
// EnsureDeviceIdentities load-or-creates under a given dir. Returns the
// parsed certificate and key plus the certificate's own PEM bytes, so
// callers that need to write ca.pem to a test dir do not have to
// re-encode cert.Raw by hand.
func genTestCA(t *testing.T, commonName string) (cert *x509.Certificate, key *ecdsa.PrivateKey, certPEM []byte) {
	t.Helper()

	certPEM, keyPEM, err := sep2cert.GenerateCA(sep2cert.CAOptions{CommonName: commonName})
	if err != nil {
		t.Fatalf("GenerateCA(%q): %v", commonName, err)
	}
	cert, key, err = parseCAPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parseCAPair(%q): %v", commonName, err)
	}
	return cert, key, certPEM
}

// TestEnsureDeviceIdentitiesDevMintDerivesRealLFDIMatchingSepTLS is the
// data-invariants-required VALUE assertion: the LFDI/SFDI
// EnsureDeviceIdentities returns for a minted device must equal what
// sepTLS.LFDI / sepTLS.SFDI compute directly from that same device's
// minted certificate read back off disk, not merely "some 40-hex-char
// string".
func TestEnsureDeviceIdentitiesDevMintDerivesRealLFDIMatchingSepTLS(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mrid := "_C1C3E687-6FFD-C753-582B-632A27E28507"

	got, err := EnsureDeviceIdentities(dir, DeviceCertModeDevMint, []string{mrid})
	if err != nil {
		t.Fatalf("EnsureDeviceIdentities: %v", err)
	}
	identity, ok := got[mrid]
	if !ok {
		t.Fatalf("EnsureDeviceIdentities: no identity returned for mRID %q", mrid)
	}

	base, err := deviceCertFileBase(mrid)
	if err != nil {
		t.Fatalf("deviceCertFileBase: %v", err)
	}
	certFile := filepath.Join(dir, deviceCertDirName, base+".x509")
	certDER, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", certFile, err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	wantLFDI := sepTLS.LFDI(cert)
	wantSFDI := sepTLS.SFDI(cert)

	if identity.LFDI != wantLFDI {
		t.Errorf("LFDI = %q, want sepTLS.LFDI(cert) = %q", identity.LFDI, wantLFDI)
	}
	if identity.SFDI != wantSFDI {
		t.Errorf("SFDI = %q, want sepTLS.SFDI(cert) = %q", identity.SFDI, wantSFDI)
	}
	if !sepTLS.ValidateSFDI(identity.SFDI) {
		t.Errorf("SFDI %q fails ValidateSFDI (bad check digit)", identity.SFDI)
	}

	// The device key must also have been written for DevMint tooling,
	// as the sibling ".pem" path (same base name, extension swapped)
	// the reference client derives from the cert path, not a
	// "-key.pem" suffixed name: see TestEnsureDeviceKeyUsesClientCompatSiblingPath.
	keyFile := filepath.Join(dir, deviceCertDirName, base+".pem")
	if _, err := os.Stat(keyFile); err != nil {
		t.Errorf("device key file missing after DevMint: %v", err)
	}
}

// TestClientSelfHashOfX509FileMatchesServedLFDI is the end to end
// interop invariant this change exists to establish: a client that
// never calls into sepTLS at all, and instead just reads the minted
// .x509 file's raw bytes off disk and computes SHA256 over them by
// hand (exactly what an external client does with a certificate it is
// handed), derives the SAME 40 character uppercase hex value as the
// LFDI EnsureDeviceIdentities returns and the server would advertise.
// This is the reason device certs are now written as raw DER rather
// than PEM: PEM armoring (base64 plus header and footer lines) is not
// byte identical to the DER payload sepTLS.LFDI hashes, so a naive
// client hashing PEM bytes would compute a different, non matching
// value. Proving this end to end, without going through sepTLS.LFDI on
// the client side, is what distinguishes this test from
// TestEnsureDeviceIdentitiesDevMintDerivesRealLFDIMatchingSepTLS above,
// which only proves internal agreement between the server side helpers.
func TestClientSelfHashOfX509FileMatchesServedLFDI(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mrid := "_D2D4F798-7GGE-D864-693C-743B38F39618"

	got, err := EnsureDeviceIdentities(dir, DeviceCertModeDevMint, []string{mrid})
	if err != nil {
		t.Fatalf("EnsureDeviceIdentities: %v", err)
	}
	identity, ok := got[mrid]
	if !ok {
		t.Fatalf("EnsureDeviceIdentities: no identity returned for mRID %q", mrid)
	}

	base, err := deviceCertFileBase(mrid)
	if err != nil {
		t.Fatalf("deviceCertFileBase: %v", err)
	}
	certFile := filepath.Join(dir, deviceCertDirName, base+".x509")

	// The client side of this invariant: read the raw file bytes and
	// hash them directly, with no sepTLS call and no certificate
	// parsing at all. This is exactly what a client that is only
	// handed the .x509 file does to discover the device's LFDI.
	fileBytes, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", certFile, err)
	}
	sum := sha256.Sum256(fileBytes)
	clientComputedLFDI := fmt.Sprintf("%X", sum[:20])

	if clientComputedLFDI != identity.LFDI {
		t.Errorf("client self hash LFDI = %q, want EnsureDeviceIdentities LFDI = %q", clientComputedLFDI, identity.LFDI)
	}

	// The server side of the same invariant: sepTLS.LFDI on the parsed
	// certificate must agree too, confirming the file on disk really is
	// the DER sepTLS hashes, not some other encoding that happens to
	// produce the same length.
	cert, err := x509.ParseCertificate(fileBytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	if want := sepTLS.LFDI(cert); clientComputedLFDI != want {
		t.Errorf("client self hash LFDI = %q, want sepTLS.LFDI(cert) = %q", clientComputedLFDI, want)
	}
}

// TestEnsureDeviceKeyUsesClientCompatSiblingPath pins the reference
// client's key discovery convention: the client derives a device's
// private key path from its certificate path by replacing the ".x509"
// extension with ".pem", not by appending a "-key" suffix. So for a
// cert minted at <base>.x509, the key must land at the sibling
// <base>.pem, and a <base>-key.pem file must NOT exist. A future rename
// back to a "-key.pem" suffix would compile and pass every other test
// in this file, since none of them assert the exact key filename
// against the client's derivation rule, but it would silently break the
// client's TLS handshake with no cert side symptom. This test exists so
// that rename cannot land unnoticed.
func TestEnsureDeviceKeyUsesClientCompatSiblingPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mrid := "_E3E5G8A9-8HHF-E975-7A4D-854C49G4A729"

	if _, err := EnsureDeviceIdentities(dir, DeviceCertModeDevMint, []string{mrid}); err != nil {
		t.Fatalf("EnsureDeviceIdentities: %v", err)
	}

	base, err := deviceCertFileBase(mrid)
	if err != nil {
		t.Fatalf("deviceCertFileBase: %v", err)
	}
	devicesDir := filepath.Join(dir, deviceCertDirName)

	wantKeyFile := filepath.Join(devicesDir, base+".pem")
	if _, err := os.Stat(wantKeyFile); err != nil {
		t.Errorf("device key file missing at the client compat sibling path %q: %v", wantKeyFile, err)
	}

	staleKeyFile := filepath.Join(devicesDir, base+"-key.pem")
	if _, err := os.Stat(staleKeyFile); err == nil {
		t.Errorf("device key file also exists at the stale suffixed path %q; the client cannot discover a key there", staleKeyFile)
	} else if !os.IsNotExist(err) {
		t.Errorf("Stat(%q): unexpected error %v", staleKeyFile, err)
	}
}

// TestEnsureDeviceIdentitiesDevMintLoadsOnSecondCall proves the
// load-or-create contract: a second call against the same dir for the
// same mRID must load the existing cert byte-for-byte, not re-mint (a
// re-mint would still be a syntactically valid cert, but a different
// key pair, silently changing "the same device"'s identity underfoot).
func TestEnsureDeviceIdentitiesDevMintLoadsOnSecondCall(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mrid := "mrid-load-twice"

	first, err := EnsureDeviceIdentities(dir, DeviceCertModeDevMint, []string{mrid})
	if err != nil {
		t.Fatalf("first EnsureDeviceIdentities: %v", err)
	}

	base, err := deviceCertFileBase(mrid)
	if err != nil {
		t.Fatalf("deviceCertFileBase: %v", err)
	}
	certFile := filepath.Join(dir, deviceCertDirName, base+".x509")
	original, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	second, err := EnsureDeviceIdentities(dir, DeviceCertModeDevMint, []string{mrid})
	if err != nil {
		t.Fatalf("second EnsureDeviceIdentities: %v", err)
	}

	reloaded, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("ReadFile (second): %v", err)
	}
	if string(original) != string(reloaded) {
		t.Fatalf("device cert bytes changed across calls: load-or-create re-minted instead of loading")
	}
	if first[mrid] != second[mrid] {
		t.Errorf("identity changed across calls: first=%+v second=%+v", first[mrid], second[mrid])
	}
}

// TestEnsureDeviceIdentitiesPreprovisionedMissingCertFailsClosed is the
// fail-closed proof: in Preprovisioned mode, a missing device cert is a
// hard error, and critically, EnsureDeviceIdentities must NOT have
// minted anything as a side effect on the way to that error.
func TestEnsureDeviceIdentitiesPreprovisionedMissingCertFailsClosed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mrid := "mrid-missing-preprovisioned"

	_, err := EnsureDeviceIdentities(dir, DeviceCertModePreprovisioned, []string{mrid})
	if err == nil {
		t.Fatal("EnsureDeviceIdentities in Preprovisioned mode with a missing cert: want error, got nil")
	}

	base, berr := deviceCertFileBase(mrid)
	if berr != nil {
		t.Fatalf("deviceCertFileBase: %v", berr)
	}
	certFile := filepath.Join(dir, deviceCertDirName, base+".x509")
	if _, statErr := os.Stat(certFile); statErr == nil {
		t.Fatalf("Preprovisioned mode minted a device cert at %q despite the missing-cert error (not fail closed)", certFile)
	}
}

// TestEnsureDeviceIdentitiesPreprovisionedLoadsExistingCert proves the
// load half of Preprovisioned mode: an operator-supplied cert (signed
// by the same CA ensureServerIdentity load-or-creates under dir) is
// loaded and its LFDI/SFDI derived, with no mint attempted. Because the
// cert genuinely chains to the CA loaded from dir, this also exercises
// (and must still pass through) the chain-verify check Preprovisioned
// mode now performs: this is the "a correctly-signed cert passes" half
// of that check; TestEnsureDeviceIdentitiesPreprovisionedRejectsWrongSignerCert
// is the "wrong signer is rejected" half.
func TestEnsureDeviceIdentitiesPreprovisionedLoadsExistingCert(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mrid := "mrid-preprovisioned-present"

	// Stand up the shared CA the same way EnsureDeviceIdentities would,
	// then hand-sign a device cert as if an operator had preprovisioned
	// it, before EnsureDeviceIdentities is ever called against dir.
	_, _, caFile, err := ensureServerIdentity(dir, DeviceCertModeDevMint)
	if err != nil {
		t.Fatalf("ensureServerIdentity: %v", err)
	}
	caCertPEM, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("ReadFile(caFile): %v", err)
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(dir, caKeyFileName))
	if err != nil {
		t.Fatalf("ReadFile(caKeyFile): %v", err)
	}
	caCert, caKey, err := parseCAPair(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("parseCAPair: %v", err)
	}

	devCertPEM, _, err := sep2cert.GenerateDeviceCert(caCert, caKey, sep2cert.DeviceCertOptions{
		DeviceType:  sep2cert.DeviceTypeGeneric,
		HWSerialNum: "preprovisioned-serial-001",
	})
	if err != nil {
		t.Fatalf("GenerateDeviceCert: %v", err)
	}
	wantCert, err := sep2cert.ParseCertificatePEM(devCertPEM)
	if err != nil {
		t.Fatalf("ParseCertificatePEM: %v", err)
	}

	base, err := deviceCertFileBase(mrid)
	if err != nil {
		t.Fatalf("deviceCertFileBase: %v", err)
	}
	devicesDir := filepath.Join(dir, deviceCertDirName)
	if err := os.MkdirAll(devicesDir, certDirPerm); err != nil {
		t.Fatalf("MkdirAll(devicesDir): %v", err)
	}
	devCertDER, err := sep2cert.CertificateDER(devCertPEM)
	if err != nil {
		t.Fatalf("CertificateDER: %v", err)
	}
	certFile := filepath.Join(devicesDir, base+".x509")
	if err := os.WriteFile(certFile, devCertDER, certFilePerm); err != nil {
		t.Fatalf("WriteFile(certFile): %v", err)
	}

	got, err := EnsureDeviceIdentities(dir, DeviceCertModePreprovisioned, []string{mrid})
	if err != nil {
		t.Fatalf("EnsureDeviceIdentities: %v", err)
	}
	identity, ok := got[mrid]
	if !ok {
		t.Fatalf("no identity returned for mRID %q", mrid)
	}
	if want := sepTLS.LFDI(wantCert); identity.LFDI != want {
		t.Errorf("LFDI = %q, want %q (derived from the preprovisioned cert)", identity.LFDI, want)
	}
	if want := sepTLS.SFDI(wantCert); identity.SFDI != want {
		t.Errorf("SFDI = %q, want %q (derived from the preprovisioned cert)", identity.SFDI, want)
	}
	// No key file should have been written: Preprovisioned mode never mints.
	keyFile := filepath.Join(devicesDir, base+".pem")
	if _, statErr := os.Stat(keyFile); statErr == nil {
		t.Errorf("Preprovisioned mode wrote a device key file at %q; it should only ever load", keyFile)
	}
}

// TestEnsureDeviceIdentitiesPreprovisionedSucceedsWithoutCAKey is the
// least-privilege proof (PR #7 review, Leon): Preprovisioned mode must
// derive a device identity successfully even when ca-key.pem was NEVER
// written to dir, and must never require or read it. The CA here is
// generated entirely in memory and only its PUBLIC certificate is
// written to dir, mirroring how a real operator would deploy: the CA
// signing key stays wherever certs are issued offline, never on the
// running bridge host.
func TestEnsureDeviceIdentitiesPreprovisionedSucceedsWithoutCAKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mrid := "mrid-no-ca-key-on-host"

	caCert, caKey, caCertPEM := genTestCA(t, "offline-issuing-ca")

	if err := os.WriteFile(filepath.Join(dir, caCertFileName), caCertPEM, certFilePerm); err != nil {
		t.Fatalf("WriteFile(ca.pem): %v", err)
	}

	devCertPEM, _, err := sep2cert.GenerateDeviceCert(caCert, caKey, sep2cert.DeviceCertOptions{
		DeviceType:  sep2cert.DeviceTypeGeneric,
		HWSerialNum: "no-ca-key-serial-001",
	})
	if err != nil {
		t.Fatalf("GenerateDeviceCert: %v", err)
	}
	wantCert, err := sep2cert.ParseCertificatePEM(devCertPEM)
	if err != nil {
		t.Fatalf("ParseCertificatePEM: %v", err)
	}

	base, err := deviceCertFileBase(mrid)
	if err != nil {
		t.Fatalf("deviceCertFileBase: %v", err)
	}
	devicesDir := filepath.Join(dir, deviceCertDirName)
	if err := os.MkdirAll(devicesDir, certDirPerm); err != nil {
		t.Fatalf("MkdirAll(devicesDir): %v", err)
	}
	devCertDER, err := sep2cert.CertificateDER(devCertPEM)
	if err != nil {
		t.Fatalf("CertificateDER: %v", err)
	}
	if err := os.WriteFile(filepath.Join(devicesDir, base+".x509"), devCertDER, certFilePerm); err != nil {
		t.Fatalf("WriteFile(device cert): %v", err)
	}

	// Load-bearing precondition: no ca-key.pem exists anywhere under dir.
	if _, statErr := os.Stat(filepath.Join(dir, caKeyFileName)); statErr == nil {
		t.Fatalf("test setup wrote a ca-key.pem; the precondition this test proves against is violated")
	}

	got, err := EnsureDeviceIdentities(dir, DeviceCertModePreprovisioned, []string{mrid})
	if err != nil {
		t.Fatalf("EnsureDeviceIdentities with no ca-key.pem on disk: %v", err)
	}
	identity, ok := got[mrid]
	if !ok {
		t.Fatalf("no identity returned for mRID %q", mrid)
	}
	if want := sepTLS.LFDI(wantCert); identity.LFDI != want {
		t.Errorf("LFDI = %q, want %q", identity.LFDI, want)
	}
	if want := sepTLS.SFDI(wantCert); identity.SFDI != want {
		t.Errorf("SFDI = %q, want %q", identity.SFDI, want)
	}

	// Postcondition: EnsureDeviceIdentities must not have created a
	// ca-key.pem as a side effect either.
	if _, statErr := os.Stat(filepath.Join(dir, caKeyFileName)); statErr == nil {
		t.Errorf("EnsureDeviceIdentities created ca-key.pem in Preprovisioned mode; it must never touch the CA private key")
	}
}

// TestEnsureDeviceIdentitiesPreprovisionedRejectsWrongSignerCert is the
// fail-closed chain-verify proof (PR #7 review, Leon): a device cert
// signed by a DIFFERENT CA than the one loaded from dir must be
// rejected at EnsureDeviceIdentities time, with a clear error, rather
// than silently producing a syntactically valid identity that would
// only fail later, at the real mTLS handshake.
func TestEnsureDeviceIdentitiesPreprovisionedRejectsWrongSignerCert(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mrid := "mrid-wrong-signer"

	// The CA EnsureDeviceIdentities will load and verify against.
	_, _, trustedCACertPEM := genTestCA(t, "trusted-ca")
	if err := os.WriteFile(filepath.Join(dir, caCertFileName), trustedCACertPEM, certFilePerm); err != nil {
		t.Fatalf("WriteFile(ca.pem): %v", err)
	}

	// A DIFFERENT CA signs the device cert placed under dir/devices.
	rogueCACert, rogueCAKey, _ := genTestCA(t, "rogue-ca")
	devCertPEM, _, err := sep2cert.GenerateDeviceCert(rogueCACert, rogueCAKey, sep2cert.DeviceCertOptions{
		DeviceType:  sep2cert.DeviceTypeGeneric,
		HWSerialNum: "wrong-signer-serial-001",
	})
	if err != nil {
		t.Fatalf("GenerateDeviceCert (rogue CA): %v", err)
	}

	base, err := deviceCertFileBase(mrid)
	if err != nil {
		t.Fatalf("deviceCertFileBase: %v", err)
	}
	devicesDir := filepath.Join(dir, deviceCertDirName)
	if err := os.MkdirAll(devicesDir, certDirPerm); err != nil {
		t.Fatalf("MkdirAll(devicesDir): %v", err)
	}
	devCertDER, err := sep2cert.CertificateDER(devCertPEM)
	if err != nil {
		t.Fatalf("CertificateDER: %v", err)
	}
	if err := os.WriteFile(filepath.Join(devicesDir, base+".x509"), devCertDER, certFilePerm); err != nil {
		t.Fatalf("WriteFile(device cert): %v", err)
	}

	_, err = EnsureDeviceIdentities(dir, DeviceCertModePreprovisioned, []string{mrid})
	if err == nil {
		t.Fatal("EnsureDeviceIdentities with a wrong-signer device cert: want error, got nil")
	}
	if !strings.Contains(err.Error(), "chain") {
		t.Errorf("error should mention the chain-verify failure: %v", err)
	}
}

// TestEnsureDeviceIdentitiesDevMintRequiresAndWritesCAKey is the
// DevMint-side complement to
// TestEnsureDeviceIdentitiesPreprovisionedSucceedsWithoutCAKey: DevMint
// mode's signing path genuinely needs the CA private key, so a DevMint
// call must both produce it (via ensureServerIdentity, when absent) and
// use it to mint a device cert whose signature verifies against the
// same CA.
func TestEnsureDeviceIdentitiesDevMintRequiresAndWritesCAKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mrid := "mrid-dev-mint-needs-ca-key"

	if _, err := EnsureDeviceIdentities(dir, DeviceCertModeDevMint, []string{mrid}); err != nil {
		t.Fatalf("EnsureDeviceIdentities: %v", err)
	}

	caKeyFile := filepath.Join(dir, caKeyFileName)
	if _, err := os.Stat(caKeyFile); err != nil {
		t.Fatalf("DevMint mode did not write %q: %v", caKeyFile, err)
	}

	caCertPEM, err := os.ReadFile(filepath.Join(dir, caCertFileName))
	if err != nil {
		t.Fatalf("ReadFile(ca.pem): %v", err)
	}
	caCert, err := sep2cert.ParseCertificatePEM(caCertPEM)
	if err != nil {
		t.Fatalf("ParseCertificatePEM(ca.pem): %v", err)
	}

	base, err := deviceCertFileBase(mrid)
	if err != nil {
		t.Fatalf("deviceCertFileBase: %v", err)
	}
	certDER, err := os.ReadFile(filepath.Join(dir, deviceCertDirName, base+".x509"))
	if err != nil {
		t.Fatalf("ReadFile(device cert): %v", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("ParseCertificate(device cert): %v", err)
	}
	if err := verifyDeviceCertChain(cert, caCert); err != nil {
		t.Errorf("minted device cert does not verify against the minted CA: %v", err)
	}
}

// TestEnsureDeviceIdentitiesMultipleDevicesDistinctIdentities covers
// the fleet case (the bridge's real feeder query returns many devices):
// every mRID gets its own identity, and no two collide.
func TestEnsureDeviceIdentitiesMultipleDevicesDistinctIdentities(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mrids := []string{"mrid-inv-1", "mrid-bat-1", "mrid-sol-1"}

	got, err := EnsureDeviceIdentities(dir, DeviceCertModeDevMint, mrids)
	if err != nil {
		t.Fatalf("EnsureDeviceIdentities: %v", err)
	}
	if len(got) != len(mrids) {
		t.Fatalf("got %d identities, want %d", len(got), len(mrids))
	}

	seenLFDI := make(map[string]string, len(mrids))
	for _, mrid := range mrids {
		identity, ok := got[mrid]
		if !ok {
			t.Fatalf("no identity for mRID %q", mrid)
		}
		if identity.LFDI == "" || identity.SFDI == "" {
			t.Fatalf("mRID %q: empty LFDI/SFDI: %+v", mrid, identity)
		}
		if other, dup := seenLFDI[identity.LFDI]; dup {
			t.Fatalf("mRID %q and %q collided on LFDI %q", mrid, other, identity.LFDI)
		}
		seenLFDI[identity.LFDI] = mrid
	}
}

func TestEnsureDeviceIdentitiesRejectsEmptyDir(t *testing.T) {
	t.Parallel()

	if _, err := EnsureDeviceIdentities("", DeviceCertModeDevMint, []string{"m1"}); err == nil {
		t.Fatal("want error for empty dir, got nil")
	}
}

func TestEnsureDeviceIdentitiesRejectsEmptyMRID(t *testing.T) {
	t.Parallel()

	if _, err := EnsureDeviceIdentities(t.TempDir(), DeviceCertModeDevMint, []string{"m1", ""}); err == nil {
		t.Fatal("want error for an empty mRID in mrids, got nil")
	}
}

func TestEnsureDeviceIdentitiesRejectsDuplicateMRID(t *testing.T) {
	t.Parallel()

	if _, err := EnsureDeviceIdentities(t.TempDir(), DeviceCertModeDevMint, []string{"m1", "m1"}); err == nil {
		t.Fatal("want error for a duplicate mRID in mrids, got nil")
	}
}

func TestEnsureDeviceIdentitiesEmptyMRIDsIsANoOp(t *testing.T) {
	t.Parallel()

	got, err := EnsureDeviceIdentities(t.TempDir(), DeviceCertModeDevMint, nil)
	if err != nil {
		t.Fatalf("EnsureDeviceIdentities with no mRIDs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d identities, want 0", len(got))
	}
}

func TestDeviceCertFileBaseRejectsEmpty(t *testing.T) {
	t.Parallel()

	if _, err := deviceCertFileBase(""); err == nil {
		t.Fatal("want error for empty mRID, got nil")
	}
}

// TestDeviceCertFileBaseSanitizesPathTraversal proves the security
// invariant: an mRID engineered to look like a path-traversal or
// absolute-path attempt must never survive into the returned base
// unescaped.
func TestDeviceCertFileBaseSanitizesPathTraversal(t *testing.T) {
	t.Parallel()

	cases := []string{
		"../../etc/passwd",
		"/etc/passwd",
		"a/b/../../c",
		"..",
	}
	for _, mrid := range cases {
		base, err := deviceCertFileBase(mrid)
		if err != nil {
			t.Fatalf("deviceCertFileBase(%q): unexpected error: %v", mrid, err)
		}
		if strings.Contains(base, "/") || strings.Contains(base, "..") {
			t.Errorf("deviceCertFileBase(%q) = %q still contains a path separator or traversal segment", mrid, base)
		}
	}
}

// TestDeviceCertFileBaseDistinctForNearCollisions proves the collision
// guard: two mRIDs that sanitize to the same safe text (because they
// differ only in characters the sanitizer replaces) must still produce
// distinct bases.
func TestDeviceCertFileBaseDistinctForNearCollisions(t *testing.T) {
	t.Parallel()

	a, err := deviceCertFileBase("a/b")
	if err != nil {
		t.Fatalf("deviceCertFileBase(a/b): %v", err)
	}
	b, err := deviceCertFileBase("a_b")
	if err != nil {
		t.Fatalf("deviceCertFileBase(a_b): %v", err)
	}
	if a == b {
		t.Errorf("distinct mRIDs %q and %q collided onto the same file base %q", "a/b", "a_b", a)
	}
}
