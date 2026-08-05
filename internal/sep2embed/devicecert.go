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
//
// LFDI and SFDI are the CANONICAL, spec-conformant identities, derived
// from the certificate's DER bytes (sepTLS.LFDI / sepTLS.SFDI). LFDI is
// always the identity a wire caller is matched against for ownership,
// and the only identity a device is advertised under.
type DeviceIdentity struct {
	LFDI string
	SFDI string
}

// EnsureDeviceIdentities derives a real, certificate-backed IEEE 2030.5
// identity for every mRID in mrids, keyed by mRID.
//
// The CA that signs (DevMint) or is expected to have already signed
// (Preprovisioned) every device cert is obtained via
// loadDeviceSigningCA, whose doc comment covers the mode-dependent
// least-privilege shape: DevMint load-or-creates the embedded server's
// full four-file identity set (including the CA private key) BEFORE
// any device cert is touched, to prevent a later re-mint from orphaning
// already-signed device certs; Preprovisioned reads only the CA
// certificate and never the CA private key, since it never signs
// anything.
//
// Each device's cert lives at dir/devices/<safe-mrid-hash>.x509 (see
// deviceCertFileBase); in DevMint mode a freshly minted device also
// gets a sibling <safe-mrid-hash>.pem private key for dev tooling that
// needs to dial in as that device. The key uses the ".pem" extension on
// the SAME base name, not a "-key.pem" suffix, because the reference
// client derives the key path from the cert path by swapping the
// ".x509" extension for ".pem"; see ensureDeviceCert's doc comment for
// the full rationale. "Cert file exists" is the sole load-vs-mint
// signal per device, matching ensureServerIdentity's own load-or-create
// shape: the private key is dev-tooling material, not something this
// function or New reads back. A Preprovisioned-mode load additionally
// verifies the loaded cert chains to the CA (see ensureDeviceCert and
// verifyDeviceCertChain), so a misconfigured or wrong-signer
// operator-supplied cert fails here rather than at the real mTLS
// handshake.
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

	caCert, caKey, err := loadDeviceSigningCA(dir, mode)
	if err != nil {
		return nil, err
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

// loadDeviceSigningCA obtains the CA certificate that device
// certificates are signed by (DevMint) or verified against
// (Preprovisioned), and, ONLY in DevMint mode, its private key.
//
// DevMint calls ensureServerIdentity, which load-or-creates the
// embedded server's full four-file identity set (ca.pem, ca-key.pem,
// server.pem, server-key.pem) BEFORE any device cert is touched: this
// prevents the hazard where a LATER sep2embed.New call (which also
// calls ensureServerIdentity) finds an incomplete four-file set and
// re-mints a fresh CA, silently orphaning every device cert this
// function already signed against the old one. Once EnsureDeviceIdentities
// has run in DevMint mode, a subsequent ensureServerIdentity call from
// New finds all four files present and only loads them: a cheap,
// idempotent no-op.
//
// Preprovisioned mode never signs anything here, so it deliberately
// does NOT call ensureServerIdentity (whose all-four-files-present
// check would require ca-key.pem to exist on disk purely to satisfy
// that check, even though the key's bytes are never read on the
// all-four-present load path) and does NOT read the CA private key at
// all. The CA signing key is the highest-value secret in this trust
// chain; a production bridge host that only ever LOADS operator-issued
// device certs has no legitimate need for it on the box (least
// privilege). Preprovisioned mode reads ca.pem directly and requires it
// to exist; a missing CA cert is a fail-closed error naming the
// expected path.
func loadDeviceSigningCA(dir string, mode DeviceCertMode) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	if mode == DeviceCertModeDevMint {
		_, _, caFile, err := ensureServerIdentity(dir)
		if err != nil {
			return nil, nil, fmt.Errorf("sep2embed: device identities: server CA: %w", err)
		}
		caCertPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, nil, fmt.Errorf("sep2embed: device identities: read CA cert: %w", err)
		}
		caKeyPEM, err := os.ReadFile(filepath.Join(dir, caKeyFileName))
		if err != nil {
			return nil, nil, fmt.Errorf("sep2embed: device identities: read CA key: %w", err)
		}
		caCert, caKey, err := parseCAPair(caCertPEM, caKeyPEM)
		if err != nil {
			return nil, nil, fmt.Errorf("sep2embed: device identities: parse CA: %w", err)
		}
		return caCert, caKey, nil
	}

	caFile := filepath.Join(dir, caCertFileName)
	caCertPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, nil, fmt.Errorf("sep2embed: device identities: preprovisioned mode requires a CA certificate at %q: %w", caFile, err)
	}
	caCert, err := sep2cert.ParseCertificatePEM(caCertPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("sep2embed: device identities: parse CA cert %q: %w", caFile, err)
	}
	return caCert, nil, nil
}

// ensureDeviceCert loads mrid's device certificate from devicesDir if
// present, or, in DevMint mode, mints a fresh one signed by caCert/caKey
// and writes it (plus its private key) to devicesDir. In Preprovisioned
// mode a missing certificate is a hard error: see EnsureDeviceIdentities
// and its fail-closed invariant for production deployments.
//
// The certificate is stored as raw DER at <base>.x509, not PEM. This is
// deliberate: a client that is handed these exact bytes and self-hashes
// them (SHA256 over the raw DER) computes the SAME value as
// sepTLS.LFDI(cert), which the server already serves as the canonical,
// spec-conformant LFDI (per spec section 6.3.4). Shipping DER instead of
// PEM removes the need for any alias identity: there is only one LFDI,
// and handing out the .x509 file IS handing out the bytes that produce
// it. The private key remains PEM-encoded, at <base>.pem, the sibling
// path the reference client expects: it derives the key path from the
// cert path by replacing the ".x509" extension with ".pem", not by
// appending a "-key" suffix. A "-key.pem" name, while equally valid on
// this side, is invisible to that client and fails its TLS handshake
// with no cert side symptom, so the sibling ".pem" name is load bearing
// for interop, not a stylistic choice.
func ensureDeviceCert(devicesDir string, mode DeviceCertMode, mrid string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey) (*x509.Certificate, error) {
	base, err := deviceCertFileBase(mrid)
	if err != nil {
		return nil, err
	}
	certFile := filepath.Join(devicesDir, base+".x509")
	keyFile := filepath.Join(devicesDir, base+".pem")

	if _, statErr := os.Stat(certFile); statErr == nil {
		certDER, err := os.ReadFile(certFile)
		if err != nil {
			return nil, fmt.Errorf("sep2embed: read device cert %q (mRID %q): %w", certFile, mrid, err)
		}
		cert, err := x509.ParseCertificate(certDER)
		if err != nil {
			return nil, fmt.Errorf("sep2embed: parse device cert %q (mRID %q): %w", certFile, mrid, err)
		}
		if mode == DeviceCertModePreprovisioned {
			// A DevMint-mode load (the second-call path, where the cert
			// already exists from a prior mint) was signed by caCert
			// moments earlier in this same process; re-verifying it
			// would be redundant, so this check is scoped to
			// Preprovisioned mode only. An operator-supplied cert has no
			// such guarantee: a wrong-signer or self-signed cert must
			// fail here, at seed time, rather than silently producing a
			// syntactically valid LFDI/SFDI that then fails the real
			// mTLS handshake at runtime.
			if err := verifyDeviceCertChain(cert, caCert); err != nil {
				return nil, fmt.Errorf("sep2embed: device cert %q (mRID %q): %w", certFile, mrid, err)
			}
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

	devCertDER, err := sep2cert.CertificateDER(devCertPEM)
	if err != nil {
		return nil, fmt.Errorf("sep2embed: extract DER from minted device cert for mRID %q: %w", mrid, err)
	}

	if err := writeFileAtomic(certFile, devCertDER, certFilePerm); err != nil {
		return nil, fmt.Errorf("sep2embed: write %s: %w", certFile, err)
	}
	if err := writeFileAtomic(keyFile, devKeyPEM, certFilePerm); err != nil {
		return nil, fmt.Errorf("sep2embed: write %s: %w", keyFile, err)
	}

	log.Printf("sep2embed: WARNING: minted development-only device certificate for mRID %q at %q; DO NOT use in production. Provide a preprovisioned cert instead.",
		mrid, certFile)

	// Parsed from the in-memory devCertDER just written, not re-read
	// from certFile: writeFileAtomic's rename-based all-or-nothing
	// guarantee (see its own doc comment) means certFile on disk is now
	// either exactly these bytes, or, on an error already returned
	// above, untouched. There is no partial-write state on disk a
	// re-read could observe that this parse of the in-memory DER would
	// miss, so the round trip through the filesystem is unnecessary.
	cert, err := x509.ParseCertificate(devCertDER)
	if err != nil {
		return nil, fmt.Errorf("sep2embed: parse minted device cert for mRID %q: %w", mrid, err)
	}
	return cert, nil
}

// verifyDeviceCertChain verifies that cert chains to caCert, via core's
// sep2tls.VerifyPeerCertWithHardwareModuleSAN rather than a bare
// stdlib x509.Certificate.Verify call. This matters: a CSIP device
// cert's HardwareModuleName SAN (RFC 4108, spec section 6.11 / CSIP
// section 6.2) is a critical extension the stdlib x509 parser does not
// understand, so stdlib Verify would otherwise reject even a
// correctly-signed, well-formed device cert with "unhandled critical
// extension". VerifyPeerCertWithHardwareModuleSAN is the same
// acknowledge-then-verify helper the mTLS handshake path uses (or
// would use, per sep2tls's own doc comment), so this check has the
// identical tolerance shape. Used only for Preprovisioned-mode loads;
// see ensureDeviceCert's call site for why DevMint mode skips it.
func verifyDeviceCertChain(cert, caCert *x509.Certificate) error {
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if err := sepTLS.VerifyPeerCertWithHardwareModuleSAN([][]byte{cert.Raw}, roots); err != nil {
		return fmt.Errorf("does not chain to the trusted CA: %w", err)
	}
	return nil
}
