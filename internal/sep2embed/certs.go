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
// exactly these four names, and which of them a given mode requires is
// decided by requiredServerCertFiles.
const (
	caCertFileName     = "ca.pem"
	caKeyFileName      = "ca-key.pem"
	serverCertFileName = "server.pem"
	serverKeyFileName  = "server-key.pem"

	certDirPerm  = 0o700
	certFilePerm = 0o600
)

// ensureServerIdentity resolves the embedded server's mTLS material
// under dir for the given mode, minting a development-only set only when
// dir holds no certificate material at all and can actually be written
// to.
//
// It classifies dir on every call (classifyCertDir; nothing is cached)
// into exactly one of three states, and acts:
//
//   - Complete for the mode: the files are loaded as-is and their paths
//     returned. NOTHING is written, whether or not dir is writable. This
//     is the production path, and it is what makes a read-only
//     bind-mounted certificate volume a supported deployment.
//   - Empty and writable: a fresh self-signed CA plus a server leaf
//     signed by it is minted and written at 0600 (dir at 0700). The dev
//     path. All four files are written even in a mode whose required set
//     is three, so the resulting directory is complete under either mode
//     and a later start cannot classify this process's own output as
//     partial.
//   - Empty and not writable: a fatal error naming the missing files and
//     saying the directory could not be written to.
//   - Partially populated: a fatal error, EVEN when dir is writable. See
//     classifyCertDir for why completing an operator's partial set is
//     worse than refusing to start.
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

	state, present, missing, err := classifyCertDir(dir, mode)
	if err != nil {
		return "", "", "", err
	}

	switch state {
	case certDirComplete:
		return certFile, keyFile, caFile, nil

	case certDirPartial:
		return "", "", "", fmt.Errorf("%w: %q holds %s but is missing %s. Nothing was written. Minting the missing files would create a CA private key that does not match the CA certificate already there, so this process refuses rather than guessing. Either add the missing files, or move the existing ones aside to let a fresh development set be minted",
			errCertDirPartial, dir, describeCertFiles(present), describeCertFiles(missing))

	case certDirEmpty:
		if werr := certDirWritable(dir); werr != nil {
			return "", "", "", fmt.Errorf("%w: %q holds none of %s, so there is nothing to load, and it could not be written to, so there is nothing that can be minted: %w. Either preprovision %s in that directory, or make it writable by this process",
				errCertDirNotWritable, dir, describeCertFiles(missing), werr, describeCertFiles(missing))
		}

	default:
		return "", "", "", fmt.Errorf("sep2embed: unhandled certificate directory state %d for %q", state, dir)
	}

	log.Printf("sep2embed: WARNING: certificate directory %q is empty; minting a development-only self-signed CA and server certificate. DO NOT use this material in production; provide preprovisioned %s, %s, and %s instead (plus %s only if this process must sign certificates).",
		dir, caCertFileName, serverCertFileName, serverKeyFileName, caKeyFileName)

	caCertPEM, caKeyPEM, err := sep2cert.GenerateCA(sep2cert.CAOptions{
		Organization: "gridappsd-ieee-2030_5-go dev-mint",
		CommonName:   "gridappsd-ieee-2030_5-go dev CA",
	})
	if err != nil {
		return "", "", "", fmt.Errorf("mint dev CA: %w", err)
	}

	caCert, caKey, err := parseCAPair(caCertPEM, caKeyPEM)
	if err != nil {
		return "", "", "", fmt.Errorf("parse minted dev CA: %w", err)
	}

	serverCertPEM, serverKeyPEM, err := sep2cert.GenerateServerCert(caCert, caKey, sep2cert.ServerCertOptions{
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

func parseCAPair(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cert, err := sep2cert.ParseCertificatePEM(certPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert: %w", err)
	}
	key, err := sep2cert.ParseKeyPEM(keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}
	return cert, key, nil
}
