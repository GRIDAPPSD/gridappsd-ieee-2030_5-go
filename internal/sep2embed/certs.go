package sep2embed

import (
	"crypto/ecdsa"
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
)

// File names within Config.CertDir. Fixed, not configurable: the
// directory classification in classifyCertDir depends on checking for
// exactly these six names, and which of them a given mode or directory
// state requires is decided by requiredServerCertFiles and
// requiredServingCAFiles.
//
// ca.pem/ca-key.pem is the DEVICE CA: the trust bundle the mTLS
// listener verifies device certificates against (ensureServerIdentity's
// caFile), and what loadDeviceSigningCA signs new device certs with.
// serving-ca.pem/serving-ca-key.pem (#118) is the SERVING CA: it signs
// only the bridge's own leaf, server.pem/server-key.pem.
//
// The pair is optional as a whole ONLY for an existing, already-complete
// directory: when neither serving CA file is present, an EXISTING
// server.pem was signed before the split and the device CA still fills
// both roles, so a pre-#118 deployment loads unchanged with no operator
// action. This does NOT extend to the mint path: an empty directory
// always mints two distinct CAs (see ensureServerIdentity), because
// there is no pre-split leaf to stay compatible with. A client that
// holds only ca.pem cannot verify a freshly minted bridge; it needs
// serving-ca.pem instead. See ensureServerIdentity's mint-path log line.
const (
	caCertFileName        = "ca.pem"
	caKeyFileName         = "ca-key.pem"
	serverCertFileName    = "server.pem"
	serverKeyFileName     = "server-key.pem"
	servingCACertFileName = "serving-ca.pem"
	servingCAKeyFileName  = "serving-ca-key.pem"

	certDirPerm  = 0o700
	certFilePerm = 0o600
)

// ensureServerIdentity resolves the embedded server's mTLS material
// under dir for the given mode.
//
// It classifies dir on every call (classifyCertDir; nothing is cached)
// and acts:
//
//   - Complete for the mode: the files are loaded as-is and their paths
//     returned. NOTHING is written, whether or not dir is writable. This
//     is the production path.
//   - Anything less than complete, in a mode that may not write
//     (preprovisioned): a fatal error, immediately, with no writability
//     probe, because the probe is itself a write. See
//     modeMayWriteCertMaterial: that mode never creates material under
//     any circumstances, which is what makes a read-only bind-mounted
//     certificate volume the SUPPORTED deployment shape rather than one
//     that happens to work.
//   - Empty and writable, in a mode that may write: a fresh device CA, a
//     fresh serving CA, and a server leaf signed by the SERVING CA are
//     minted and written at 0600 (dir at 0700). The dev path. All six
//     files are written even in a mode whose required set is smaller,
//     so the resulting directory is complete under either mode and a
//     later start cannot classify this process's own output as partial.
//   - Empty and not writable: a fatal error naming the missing files and
//     saying the directory could not be written to.
//   - Partially populated: a fatal error, EVEN when dir is writable. See
//     classifyCertDir for why completing an operator's partial set is
//     worse than refusing to start.
//
// Every fatal case here is fatal to the PROCESS: the caller has no
// serviceable state to start into, because the server has no identity,
// so it exits non-zero rather than warning and continuing. That is a
// startup rule and it does NOT extend to the running server: once the
// listener is up, a client that cannot be served is refused and the
// process keeps serving every other device. Anything reachable from a
// client interaction that could terminate the process would be a remote
// denial of service.
//
// Which files a mode requires depends on whether that mode signs; see
// requiresCASigningKey. The previous unconditional all-four check is the
// defect this shape replaces: a correctly deployed non-signing bridge
// that withholds the CA private key had its real server certificate and
// key renamed over with self-signed development material, which the
// server then served.
//
// Minted files are written via writeFileNoClobber, which fails rather
// than replacing an existing file, so no execution order through this
// function can destroy operator material.
//
// The returned material is read from disk exactly once per process, by
// the caller, at startup. It is NOT re-read afterwards, and must not be:
// the CA certificate is the trust anchor every registered client's chain
// was verified against, and the server key backs every live TLS session.
// A server whose identity could change underneath it would break both
// silently. Only the per-device certificate lookup re-reads the
// filesystem later; see ensureDeviceCert.
//
// Returns the three file paths sep2srv.Options needs (CertFile, KeyFile,
// CAFile). The CA private key file is written by the mint path and its
// path returned to nothing further inside this package, but it is NOT
// dead weight: a caller (or a dev-only tooling script) that wants to
// mint additional device certs trusted by this same dev CA, for local
// testing, needs to sign against caKeyFile. See certs_test.go and
// embed_test.go's mintTestDeviceClient, which do exactly that.
func ensureServerIdentity(dir string, mode DeviceCertMode) (certFile, keyFile, caFile string, err error) {
	caFile = filepath.Join(dir, caCertFileName)
	caKeyFile := filepath.Join(dir, caKeyFileName)
	certFile = filepath.Join(dir, serverCertFileName)
	keyFile = filepath.Join(dir, serverKeyFileName)
	servingCAFile := filepath.Join(dir, servingCACertFileName)
	servingCAKeyFile := filepath.Join(dir, servingCAKeyFileName)

	state, present, missing, err := classifyCertDir(dir, mode)
	if err != nil {
		return "", "", "", err
	}

	if state == certDirComplete {
		return certFile, keyFile, caFile, nil
	}

	// The material is not complete for this mode. A mode that never
	// writes cannot make it complete, so this is fatal here and now, with
	// no probe (a probe is itself a write) and no mint. The caller is
	// expected to exit non-zero rather than start degraded; there is no
	// serviceable state to start into, because the server has no identity.
	if !modeMayWriteCertMaterial(mode) {
		return "", "", "", incompleteCertDirError(errCertDirWriteForbidden, dir, mode, present, missing,
			"This mode never creates certificate material, so it cannot supply the missing files and did not write anything. Place them in that directory before starting, or start in dev-mint mode if this is a development host.")
	}

	switch state {
	case certDirPartial:
		return "", "", "", incompleteCertDirError(errCertDirPartial, dir, mode, present, missing,
			"Nothing was written. Minting the missing files would create a CA private key that does not match the CA certificate already there, so this process refuses rather than guessing. Either add the missing files, or move the existing ones aside to let a fresh development set be minted.")

	case certDirEmpty:
		if werr := certDirWritable(dir); werr != nil {
			return "", "", "", incompleteCertDirError(errCertDirNotWritable, dir, mode, present, missing,
				fmt.Sprintf("Nothing was written. The directory could not be written to, so nothing can be minted either: %v. Either preprovision the missing files, or make the directory writable by this process.", werr))
		}

	default:
		return "", "", "", fmt.Errorf("sep2embed: unhandled certificate directory state %d for %q", state, dir)
	}

	log.Printf("sep2embed: WARNING: certificate directory %q is empty; minting a development-only self-signed device CA, serving CA, and server certificate. DO NOT use this material in production; provide preprovisioned %s, %s, %s, and %s instead (plus %s and %s only if this process must sign certificates). The server leaf is signed by the SERVING CA (%s), not the device CA (%s): a client that trusts only %s cannot verify this server and will fail with an unknown-authority error. Distribute %s as the server's trust anchor, and %s only to sign device certificates you mint yourself.",
		dir, caCertFileName, servingCACertFileName, serverCertFileName, serverKeyFileName, caKeyFileName, servingCAKeyFileName,
		servingCACertFileName, caCertFileName, caCertFileName, servingCACertFileName, caCertFileName)

	// The device CA is minted here but not parsed or used to sign
	// anything in this call: loadDeviceSigningCA re-reads and parses it
	// from disk, later, per device. Signing the server's own leaf below
	// with the SERVING CA rather than this one is the entire change #118
	// makes; everything else about the mint path is unchanged.
	caCertPEM, caKeyPEM, err := sep2cert.GenerateCA(sep2cert.CAOptions{
		Organization: "gridappsd-ieee-2030_5-go dev-mint",
		CommonName:   "gridappsd-ieee-2030_5-go dev device CA",
	})
	if err != nil {
		return "", "", "", fmt.Errorf("mint dev device CA: %w", err)
	}

	servingCACertPEM, servingCAKeyPEM, err := sep2cert.GenerateCA(sep2cert.CAOptions{
		Organization: "gridappsd-ieee-2030_5-go dev-mint",
		CommonName:   "gridappsd-ieee-2030_5-go dev serving CA",
	})
	if err != nil {
		return "", "", "", fmt.Errorf("mint dev serving CA: %w", err)
	}

	servingCACert, servingCAKey, err := parseCAPair(servingCACertPEM, servingCAKeyPEM)
	if err != nil {
		return "", "", "", fmt.Errorf("parse minted dev serving CA: %w", err)
	}

	serverCertPEM, serverKeyPEM, err := sep2cert.GenerateServerCert(servingCACert, servingCAKey, sep2cert.ServerCertOptions{
		CommonName: "gridappsd-ieee-2030_5-go embedded server",
		Hosts:      []string{"localhost", "127.0.0.1"},
	})
	if err != nil {
		return "", "", "", fmt.Errorf("mint dev server cert: %w", err)
	}

	writes := []struct {
		path string
		data []byte
	}{
		{caFile, caCertPEM},
		{caKeyFile, caKeyPEM},
		{servingCAFile, servingCACertPEM},
		{servingCAKeyFile, servingCAKeyPEM},
		{certFile, serverCertPEM},
		{keyFile, serverKeyPEM},
	}
	for _, w := range writes {
		if err := writeFileNoClobber(w.path, w.data, certFilePerm); err != nil {
			return "", "", "", fmt.Errorf("write %s: %w", w.path, err)
		}
	}

	return certFile, keyFile, caFile, nil
}

// writeFileAtomic writes data to path as a single all-or-nothing
// operation: it writes to a temp file in the same directory as path
// (same filesystem, so the final rename is atomic on POSIX), sets perm,
// then renames the temp file over path. A crash or error at any point
// before the rename leaves path exactly as it was (untouched, or absent);
// a crash after the rename leaves the complete new file. There is no
// window in which path exists but holds a partial write.
//
// It REPLACES an existing file at path: os.Rename does not ask. That is
// acceptable only where the caller has already established the target is
// its own to write, which is why the server-identity mint path uses
// writeFileNoClobber instead. Reach for that one for anything an
// operator may have placed.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	// Clean up the temp file on any path that does not reach the
	// rename; renamed-away files are not removed by this defer since
	// os.Remove on an already-renamed path is a harmless not-exist error
	// we deliberately ignore here (best-effort cleanup, not the primary
	// error return).
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()

	if _, writeErr := tmp.Write(data); writeErr != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", writeErr)
	}
	if closeErr := tmp.Close(); closeErr != nil {
		return fmt.Errorf("close temp file: %w", closeErr)
	}
	if chmodErr := os.Chmod(tmpName, perm); chmodErr != nil {
		return fmt.Errorf("chmod temp file: %w", chmodErr)
	}
	if renameErr := os.Rename(tmpName, path); renameErr != nil {
		return fmt.Errorf("rename temp file to %s: %w", path, renameErr)
	}
	renamed = true

	return nil
}

// parseCAPair parses a CA certificate and private key and confirms the
// key can actually sign for that certificate before returning either.
// ParseKeyPEM only ever returns ECDSA keys, so a cert whose public key
// is not ECDSA can never match one and is refused on that basis too.
// Without this check a mismatched pair loads cleanly and every device
// certificate it signs fails to verify later, at the client, with no
// indication the CA material itself was the cause.
func parseCAPair(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cert, err := sep2cert.ParseCertificatePEM(certPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert: %w", err)
	}
	key, err := sep2cert.ParseKeyPEM(keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !key.PublicKey.Equal(pub) {
		return nil, nil, fmt.Errorf("CA key %s does not match CA certificate %s", caKeyFileName, caCertFileName)
	}
	return cert, key, nil
}
