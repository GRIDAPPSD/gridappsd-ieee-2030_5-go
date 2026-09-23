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
// mode, their private keys). Devices are signed by the DEVICE CA (see
// ensureServerIdentity and loadDeviceSigningCA), which is also the
// trust bundle the mTLS listener verifies clients against, so a device
// cert minted or loaded here is automatically trusted once the listener
// starts: no ExtraClientCAs wiring is needed for that. Since #118 split
// the CAs, the device CA is not necessarily the CA that signs the
// embedded server's own leaf (the serving CA); the two default to the
// same material and only diverge once an operator supplies a separate
// serving CA pair. This mirrors how
// internal/sep2embed/embed_test.go's mintTestDeviceClient already signs
// its own throwaway test device certs against the device CA.
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
// function or New reads back. That is not a licence to write over
// whatever sits at the key path, though: a key present without its
// certificate makes the pair partially populated, and ensureDeviceCert
// refuses that rather than minting over it. A Preprovisioned-mode load additionally
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
// does NOT call ensureServerIdentity and does NOT read the CA private
// key at all. The CA signing key is the highest-value secret in this
// trust chain; a production bridge host that only ever LOADS
// operator-issued device certs has no legitimate need for it on the box
// (least privilege). Preprovisioned mode reads ca.pem directly and
// requires it to exist; a missing CA cert is a fail-closed error naming
// the expected path.
//
// The CA certificate is re-read from disk on every call rather than
// held from a prior one. That is not a caching oversight: it is what
// lets a bridge started against a read-only preprovisioned directory
// pick up material an operator adds later, without a restart. It does
// NOT make the running server's own identity mutable; that is fixed at
// startup by New and never re-read (see ensureServerIdentity).
func loadDeviceSigningCA(dir string, mode DeviceCertMode) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	if mode == DeviceCertModeDevMint {
		_, _, caFile, err := ensureServerIdentity(dir, mode)
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

	// certFileExists rather than a bare os.Stat: only fs.ErrNotExist is
	// absence here, exactly as in classifyCertDir. A cert this process
	// cannot stat because of a permission error is not a cert that is
	// missing, and treating it as one would send an unreadable but
	// provisioned device down the mint path.
	certExists, err := certFileExists(certFile)
	if err != nil {
		return nil, fmt.Errorf("sep2embed: device cert for mRID %q: %w", mrid, err)
	}

	if certExists {
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

	// Wrapped in the same sentinel the server-identity refusal uses, so
	// "this mode never creates certificate material" is one matchable
	// predicate across both layers rather than two similar-looking error
	// strings that could drift apart.
	if !modeMayWriteCertMaterial(mode) {
		return nil, fmt.Errorf("%w: mode %q requires an operator-supplied certificate for mRID %q at %q, and none is there. Nothing was written; this mode never mints one (fail closed)",
			errCertDirWriteForbidden, mode, mrid, certFile)
	}

	// The certificate's absence is the mint signal, but a mint writes TWO
	// files, and the key is at a second path that may be occupied when
	// the certificate is not: a prior mint that died between the two
	// writes, or an operator staging keys ahead of certificates. Minting
	// now would rename over that key.
	//
	// This pair is partially populated in precisely the sense a server
	// cert dir can be (see classifyCertDir), so it gets the same answer:
	// refuse, and write nothing. Completing somebody else's set would
	// leave a certificate paired with a private key it does not match,
	// which is a device that cannot complete a handshake and a directory
	// that looks provisioned.
	keyExists, err := certFileExists(keyFile)
	if err != nil {
		return nil, fmt.Errorf("sep2embed: device key for mRID %q: %w", mrid, err)
	}
	if keyExists {
		return nil, fmt.Errorf("%w: a private key for mRID %q already exists at %q but its certificate %q does not. Nothing was written. Minting now would overwrite that key, and a fresh key would not match a certificate issued for the old one. Either supply the matching certificate, or move the key aside",
			errCertDirPartial, mrid, keyFile, certFile)
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

	// writeFileNoClobber, not writeFileAtomic: the guard above is a check
	// and these are acts, and between them there is a window. Finishing
	// with os.Link closes it in the kernel, so neither write can destroy
	// a file no matter how the two interleave.
	if err := writeFileNoClobber(certFile, devCertDER, certFilePerm); err != nil {
		return nil, fmt.Errorf("sep2embed: write %s: %w", certFile, err)
	}
	if err := writeFileNoClobber(keyFile, devKeyPEM, certFilePerm); err != nil {
		return nil, fmt.Errorf("sep2embed: write %s: %w", keyFile, err)
	}

	log.Printf("sep2embed: WARNING: minted development-only device certificate for mRID %q at %q; DO NOT use in production. Provide a preprovisioned cert instead.",
		mrid, certFile)

	// Parsed from the in-memory devCertDER just written, not re-read
	// from certFile: writeFileNoClobber's link-based all-or-nothing
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
