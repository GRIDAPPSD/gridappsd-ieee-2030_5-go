package sep2embed

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"
)

// deviceCertDirName is the fixed subdirectory of the embedded server's
// CertDir holding per-device identity certificates (and, in DevMint
// mode, their private keys). Devices are signed by the SAME CA the
// embedded server's own leaf certificate is signed by (see
// ensureServerIdentity), so a device cert minted or loaded here is
// automatically trusted by the mTLS listener once it starts: no
// ExtraClientCAs wiring is needed for this same-CA shape. This mirrors
// how internal/sep2embed/embed_test.go's mintTestDeviceClient already
// signs its own throwaway test device certs against the same CA.
const deviceCertDirName = "devices"

// deviceCertFileNameSafeRune reports whether r is safe to appear
// unescaped in a device-cert file name.
func deviceCertFileNameSafeRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
}

// deviceCertFileNameMaxSafeLen caps the human-readable portion of a
// sanitized device-cert file name so a pathological (very long) mRID
// cannot produce an unwieldy or filesystem-limit-exceeding path.
const deviceCertFileNameMaxSafeLen = 64

// deviceCertFileBase sanitizes mrid into a filesystem-safe file name
// base for use under a device-cert directory.
//
// mrid crosses a system trust boundary (it is CIM SPARQL query data
// from the GridAPPS-D platform, not a value this package controls), so
// it is never spliced into a path unchecked: every rune outside
// [A-Za-z0-9_-] is replaced with '_' before use, which by construction
// rules out a path separator or a ".." segment surviving into the
// resulting file name (see [[secure-coding]] Rule 1, canonicalize
// before use, and [[data-invariants]] Rule 3, the boundary itself is a
// candidate for the disallowed condition).
//
// The sanitized text alone is not sufficient to guarantee uniqueness:
// two distinct mRIDs that differ only in characters replaced by the
// sanitizer (e.g. "a/b" and "a_b") would otherwise collide onto the
// same file, silently merging two devices' certificate material. A
// short hex suffix derived from the SHA-256 of the *original,
// unsanitized* mrid is appended to rule that out deterministically.
func deviceCertFileBase(mrid string) (string, error) {
	if mrid == "" {
		return "", errors.New("sep2embed: empty mRID cannot be used to build a device cert path")
	}

	sum := sha256.Sum256([]byte(mrid))

	var b strings.Builder
	for _, r := range mrid {
		if deviceCertFileNameSafeRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	safe := b.String()
	if len(safe) > deviceCertFileNameMaxSafeLen {
		safe = safe[:deviceCertFileNameMaxSafeLen]
	}

	return safe + "-" + hex.EncodeToString(sum[:8]), nil
}

// DeviceCertMode selects how EnsureDeviceIdentities sources each
// device's identity certificate.
type DeviceCertMode int

const (
	// DeviceCertModeDevMint mints a fresh certificate, signed by the
	// shared CA under the configured dir, for any device whose cert
	// file does not already exist. Dev-only: minted material is
	// written to disk with 0600 permissions and logged with a WARNING.
	DeviceCertModeDevMint DeviceCertMode = iota

	// DeviceCertModePreprovisioned requires every device's certificate
	// to already exist under dir/devices; a missing certificate is a
	// hard, fail-closed error rather than a silent mint. Production: an
	// operator places every device's cert under dir/devices before the
	// bridge starts.
	DeviceCertModePreprovisioned
)

// DeviceIdentity is one device's certificate-derived IEEE 2030.5
// identity: LFDI per spec section 6.3.4, SFDI per spec section 6.3.3.
type DeviceIdentity struct {
	LFDI string
	SFDI string
}

// EnsureDeviceIdentities derives a real, certificate-backed IEEE 2030.5
// identity for every mRID in mrids, keyed by mRID.
//
// It first load-or-creates the embedded server's own CA and leaf
// material under dir via ensureServerIdentity, so the CA that will sign
// (DevMint) or is expected to already have signed (Preprovisioned)
// every device cert is fixed and stable BEFORE any device cert is
// touched. Calling ensureServerIdentity here, ahead of any per-device
// work, matters: it prevents the hazard where a LATER sep2embed.New
// call (which also calls ensureServerIdentity) finds an incomplete
// four-file set (a CA present but no server leaf yet, for example) and
// re-mints a FRESH CA, silently orphaning every device cert this
// function already signed against the old one. Once this function has
// run, a subsequent ensureServerIdentity call from New finds all four
// files present and only loads them: a cheap, idempotent no-op.
//
// Each device's cert lives at dir/devices/<safe-mrid-hash>.pem (see
// deviceCertFileBase); in DevMint mode a freshly minted device also
// gets a sibling <safe-mrid-hash>-key.pem for dev tooling that needs to
// dial in as that device. "Cert file exists" is the sole load-vs-mint
// signal per device, matching ensureServerIdentity's own load-or-create
// shape: the private key is dev-tooling material, not something this
// function or New reads back.
//
// mrids with an empty string, or duplicate entries, are both rejected:
// an empty mRID cannot address a file (see deviceCertFileBase), and
// EnsureDeviceIdentities' one-cert-per-device contract would be
// ambiguous for a duplicate. Both are caller programming errors, not
// data conditions to silently tolerate.
func EnsureDeviceIdentities(dir string, mode DeviceCertMode, mrids []string) (map[string]DeviceIdentity, error) {
	if dir == "" {
		return nil, errors.New("sep2embed: EnsureDeviceIdentities: dir is required")
	}

	if len(mrids) == 0 {
		return map[string]DeviceIdentity{}, nil
	}

	seen := make(map[string]struct{}, len(mrids))
	for _, mrid := range mrids {
		if mrid == "" {
			return nil, errors.New("sep2embed: EnsureDeviceIdentities: mrids contains an empty mRID")
		}
		if _, dup := seen[mrid]; dup {
			return nil, fmt.Errorf("sep2embed: EnsureDeviceIdentities: duplicate mRID %q", mrid)
		}
		seen[mrid] = struct{}{}
	}

	_, _, caFile, err := ensureServerIdentity(dir)
	if err != nil {
		return nil, fmt.Errorf("sep2embed: device identities: server CA: %w", err)
	}
	caKeyFile := filepath.Join(dir, caKeyFileName)

	caCertPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("sep2embed: device identities: read CA cert: %w", err)
	}
	caKeyPEM, err := os.ReadFile(caKeyFile)
	if err != nil {
		return nil, fmt.Errorf("sep2embed: device identities: read CA key: %w", err)
	}
	caCert, caKey, err := parseCAPair(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("sep2embed: device identities: parse CA: %w", err)
	}

	devicesDir := filepath.Join(dir, deviceCertDirName)

	out := make(map[string]DeviceIdentity, len(mrids))
	for _, mrid := range mrids {
		cert, err := ensureDeviceCert(devicesDir, mode, mrid, caCert, caKey)
		if err != nil {
			return nil, err
		}
		out[mrid] = DeviceIdentity{
			LFDI: sepTLS.LFDI(cert),
			SFDI: sepTLS.SFDI(cert),
		}
	}
	return out, nil
}

// ensureDeviceCert loads mrid's device certificate from devicesDir if
// present, or, in DevMint mode, mints a fresh one signed by caCert/caKey
// and writes it (plus its private key) to devicesDir. In Preprovisioned
// mode a missing certificate is a hard error: see EnsureDeviceIdentities
// and the card's fail-closed invariant for production deployments.
func ensureDeviceCert(devicesDir string, mode DeviceCertMode, mrid string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey) (*x509.Certificate, error) {
	base, err := deviceCertFileBase(mrid)
	if err != nil {
		return nil, err
	}
	certFile := filepath.Join(devicesDir, base+".pem")

	if _, statErr := os.Stat(certFile); statErr == nil {
		certPEM, err := os.ReadFile(certFile)
		if err != nil {
			return nil, fmt.Errorf("sep2embed: read device cert %q (mRID %q): %w", certFile, mrid, err)
		}
		cert, err := sep2cert.ParseCertificatePEM(certPEM)
		if err != nil {
			return nil, fmt.Errorf("sep2embed: parse device cert %q (mRID %q): %w", certFile, mrid, err)
		}
		return cert, nil
	}

	if mode == DeviceCertModePreprovisioned {
		return nil, fmt.Errorf("sep2embed: device cert for mRID %q not found at %q: preprovisioned mode requires an operator-supplied cert and refuses to mint one (fail closed)", mrid, certFile)
	}

	if err := os.MkdirAll(devicesDir, certDirPerm); err != nil {
		return nil, fmt.Errorf("sep2embed: create device cert dir %q: %w", devicesDir, err)
	}

	devCertPEM, devKeyPEM, err := sep2cert.GenerateDeviceCert(caCert, caKey, sep2cert.DeviceCertOptions{
		DeviceType:  sep2cert.DeviceTypeGeneric,
		HWSerialNum: mrid,
		IsTestCert:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("sep2embed: mint device cert for mRID %q: %w", mrid, err)
	}

	if err := writeFileAtomic(certFile, devCertPEM, certFilePerm); err != nil {
		return nil, fmt.Errorf("sep2embed: write %s: %w", certFile, err)
	}
	keyFile := filepath.Join(devicesDir, base+"-key.pem")
	if err := writeFileAtomic(keyFile, devKeyPEM, certFilePerm); err != nil {
		return nil, fmt.Errorf("sep2embed: write %s: %w", keyFile, err)
	}

	log.Printf("sep2embed: WARNING: minted development-only device certificate for mRID %q at %q; DO NOT use in production. Provide a preprovisioned cert instead.",
		mrid, certFile)

	cert, err := sep2cert.ParseCertificatePEM(devCertPEM)
	if err != nil {
		return nil, fmt.Errorf("sep2embed: parse minted device cert for mRID %q: %w", mrid, err)
	}
	return cert, nil
}
