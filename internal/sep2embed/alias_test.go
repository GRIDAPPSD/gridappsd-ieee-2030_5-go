package sep2embed

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCombinedFileLFDIMatchesPythonFileHashScheme is the data-invariants
// VALUE assertion for the alias: combinedFileLFDI must equal the
// lowercase-hex SHA-256 of the EXACT combined bytes (device cert PEM
// block immediately followed by device key PEM block), left-truncated to
// 40 characters, which is precisely what the Python reference computes
// in lfdi_mode_from_file (hashlib.sha256(combined_bytes).hexdigest()[:40]
// via lfdi_from_fingerprint). This is not "some 40-char string": it pins
// the byte layout, the truncation length, and the lowercase casing.
func TestCombinedFileLFDIMatchesPythonFileHashScheme(t *testing.T) {
	t.Parallel()

	certPEM := []byte("-----BEGIN CERTIFICATE-----\nMIICdummycertblock\n-----END CERTIFICATE-----\n")
	keyPEM := []byte("-----BEGIN PRIVATE KEY-----\nMIGdummykeyblock\n-----END PRIVATE KEY-----\n")

	// Reference: hash the exact cert-then-key concatenation the client
	// receives and hashes; lowercase hex; first 40 chars.
	combined := append(append([]byte{}, certPEM...), keyPEM...)
	sum := sha256.Sum256(combined)
	wantFull := hex.EncodeToString(sum[:]) // lowercase
	want := wantFull[:40]

	got := combinedFileLFDI(certPEM, keyPEM)

	if got != want {
		t.Errorf("combinedFileLFDI = %q, want %q (lowercase sha256 of cert||key, first 40 hex)", got, want)
	}
	if len(got) != 40 {
		t.Errorf("combinedFileLFDI length = %d, want 40", len(got))
	}
	if got != strings.ToLower(got) {
		t.Errorf("combinedFileLFDI = %q is not lowercase; the client matches on lowercase file-hash casing", got)
	}
}

// TestCombinedPEMLayoutIsCertThenKeyNoReserialization pins the exact
// byte layout hashed: cert block first, key block second, back to back,
// no bytes inserted or dropped between them, and the bytes hashed are
// the bytes returned (no re-serialization). This is the "hash what would
// be shipped" invariant: combinedPEM is the single definition of the
// combined-file bytes, so a future export must reuse it.
func TestCombinedPEMLayoutIsCertThenKeyNoReserialization(t *testing.T) {
	t.Parallel()

	certPEM := []byte("CERTBYTES\n")
	keyPEM := []byte("KEYBYTES\n")

	got := combinedPEM(certPEM, keyPEM)
	want := "CERTBYTES\nKEYBYTES\n"
	if string(got) != want {
		t.Fatalf("combinedPEM = %q, want %q (cert then key, no separator, no reordering)", got, want)
	}

	// The alias must be the hash of exactly these bytes: no divergence
	// between the bytes hashed and the bytes combinedPEM returns.
	sum := sha256.Sum256(got)
	wantAlias := hex.EncodeToString(sum[:])[:40]
	if combinedFileLFDI(certPEM, keyPEM) != wantAlias {
		t.Error("combinedFileLFDI hashed bytes differ from combinedPEM output; the hashed bytes and the shippable bytes must be identical")
	}
}

// TestEnsureDeviceIdentitiesDevMintAliasReproducibleOnReload proves the
// mint path and the reload path produce the SAME alias for a device: the
// alias computed from the in-memory PEM at mint time equals the alias
// recomputed from the cert+key read back off disk on a later DevMint
// run. This is the reproducibility invariant that makes the advertised
// id stable across bridge restarts.
func TestEnsureDeviceIdentitiesDevMintAliasReproducibleOnReload(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const mrid = "mrid-alias-reproducible"

	first, err := EnsureDeviceIdentities(dir, DeviceCertModeDevMint, []string{mrid})
	if err != nil {
		t.Fatalf("EnsureDeviceIdentities (mint): %v", err)
	}
	mintAlias := first[mrid].AliasLFDI
	if mintAlias == "" {
		t.Fatal("DevMint alias is empty; a minted device must carry a file-hash alias")
	}
	if len(mintAlias) != 40 || mintAlias != strings.ToLower(mintAlias) {
		t.Errorf("DevMint alias %q is not 40 lowercase hex chars", mintAlias)
	}

	// Independently recompute the alias from the on-disk cert+key, the
	// exact bytes a file-mode client would read, and confirm it matches
	// what mint returned.
	base, err := deviceCertFileBase(mrid)
	if err != nil {
		t.Fatalf("deviceCertFileBase: %v", err)
	}
	devicesDir := filepath.Join(dir, deviceCertDirName)
	certPEM, err := os.ReadFile(filepath.Join(devicesDir, base+".pem"))
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(devicesDir, base+"-key.pem"))
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	if want := combinedFileLFDI(certPEM, keyPEM); want != mintAlias {
		t.Errorf("mint alias %q != recomputed-from-disk alias %q", mintAlias, want)
	}

	// A second DevMint run loads the existing cert+key and must return
	// the identical alias (stable advertised id across restarts).
	second, err := EnsureDeviceIdentities(dir, DeviceCertModeDevMint, []string{mrid})
	if err != nil {
		t.Fatalf("EnsureDeviceIdentities (reload): %v", err)
	}
	if second[mrid].AliasLFDI != mintAlias {
		t.Errorf("reload alias %q != mint alias %q; advertised id is not stable across runs", second[mrid].AliasLFDI, mintAlias)
	}
	// The canonical LFDI (DER-hash, uppercase) must be distinct from the
	// alias (file-hash, lowercase): the two identities differ by design.
	if second[mrid].LFDI == mintAlias {
		t.Error("canonical LFDI equals the alias; they must be different identities (DER-hash vs file-hash)")
	}
	if second[mrid].LFDI != strings.ToUpper(second[mrid].LFDI) {
		t.Errorf("canonical LFDI %q is not uppercase", second[mrid].LFDI)
	}
}

// TestEnsureDeviceIdentitiesDevMintReloadUnreadableKeyDegradesToEmptyAlias
// locks the safe-degrade contract of the DevMint reload branch in
// ensureDeviceCert: when a device's cert is present but its key file is
// gone (a degraded dev-cert dir where the bridge no longer holds the
// private key), the reload MUST degrade to advertising under the
// canonical LFDI with an EMPTY alias, and MUST NOT fail or fabricate a
// wrong alias from partial data. It asserts the actual returned values
// (data-invariants Rule 1), not non-crash, so a future change to that
// branch that flips it from safe-empty-alias to unsafe-fabricated-alias
// (or to a hard error that strands the device) fails this test.
func TestEnsureDeviceIdentitiesDevMintReloadUnreadableKeyDegradesToEmptyAlias(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const mrid = "mrid-degraded-key"

	// First DevMint run mints cert + key and computes a real alias.
	first, err := EnsureDeviceIdentities(dir, DeviceCertModeDevMint, []string{mrid})
	if err != nil {
		t.Fatalf("EnsureDeviceIdentities (mint): %v", err)
	}
	mintCanonical := first[mrid].LFDI
	if mintCanonical == "" {
		t.Fatal("minted canonical LFDI is empty; fixture is broken")
	}
	if first[mrid].AliasLFDI == "" {
		t.Fatal("minted alias is empty before the key was removed; fixture is broken")
	}

	// Remove the device key so the reload cannot reproduce the combined
	// file, driving the degrade branch. The cert stays in place, so the
	// device is still loadable and its canonical LFDI still derivable.
	base, err := deviceCertFileBase(mrid)
	if err != nil {
		t.Fatalf("deviceCertFileBase: %v", err)
	}
	devicesDir := filepath.Join(dir, deviceCertDirName)
	keyFile := filepath.Join(devicesDir, base+"-key.pem")
	if err := os.Remove(keyFile); err != nil {
		t.Fatalf("remove device key: %v", err)
	}

	// Second DevMint run: cert present, key absent. This is the degrade
	// branch under test.
	second, err := EnsureDeviceIdentities(dir, DeviceCertModeDevMint, []string{mrid})
	// (c) It degrades, it does not fail.
	if err != nil {
		t.Fatalf("EnsureDeviceIdentities (degraded reload) returned error, want nil (safe degrade): %v", err)
	}
	id, ok := second[mrid]
	if !ok {
		t.Fatalf("no identity returned for mRID %q on degraded reload", mrid)
	}
	// (a) The alias is empty: no fabricated alias from partial data.
	if id.AliasLFDI != "" {
		t.Errorf("degraded reload AliasLFDI = %q, want empty (no alias fabricated when the key is unreadable)", id.AliasLFDI)
	}
	// (b) The canonical LFDI is still returned correctly, so the device
	// stays reachable under its canonical LFDI. It must equal the
	// canonical LFDI from the original mint (same cert on disk).
	if id.LFDI != mintCanonical {
		t.Errorf("degraded reload canonical LFDI = %q, want %q (device must stay reachable under its canonical LFDI)", id.LFDI, mintCanonical)
	}
}
