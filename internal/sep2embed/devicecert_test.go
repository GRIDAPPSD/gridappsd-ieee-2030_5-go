package sep2embed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"
)

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
	certFile := filepath.Join(dir, deviceCertDirName, base+".pem")
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", certFile, err)
	}
	cert, err := sep2cert.ParseCertificatePEM(certPEM)
	if err != nil {
		t.Fatalf("ParseCertificatePEM: %v", err)
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

	// The device key must also have been written for DevMint tooling.
	keyFile := filepath.Join(dir, deviceCertDirName, base+"-key.pem")
	if _, err := os.Stat(keyFile); err != nil {
		t.Errorf("device key file missing after DevMint: %v", err)
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
	certFile := filepath.Join(dir, deviceCertDirName, base+".pem")
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
	certFile := filepath.Join(dir, deviceCertDirName, base+".pem")
	if _, statErr := os.Stat(certFile); statErr == nil {
		t.Fatalf("Preprovisioned mode minted a device cert at %q despite the missing-cert error (not fail closed)", certFile)
	}
}

// TestEnsureDeviceIdentitiesPreprovisionedLoadsExistingCert proves the
// load half of Preprovisioned mode: an operator-supplied cert (signed
// by the same CA ensureServerIdentity load-or-creates under dir) is
// loaded and its LFDI/SFDI derived, with no mint attempted.
func TestEnsureDeviceIdentitiesPreprovisionedLoadsExistingCert(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mrid := "mrid-preprovisioned-present"

	// Stand up the shared CA the same way EnsureDeviceIdentities would,
	// then hand-sign a device cert as if an operator had preprovisioned
	// it, before EnsureDeviceIdentities is ever called against dir.
	_, _, caFile, err := ensureServerIdentity(dir)
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
	certFile := filepath.Join(devicesDir, base+".pem")
	if err := os.WriteFile(certFile, devCertPEM, certFilePerm); err != nil {
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
	keyFile := filepath.Join(devicesDir, base+"-key.pem")
	if _, statErr := os.Stat(keyFile); statErr == nil {
		t.Errorf("Preprovisioned mode wrote a device key file at %q; it should only ever load", keyFile)
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
