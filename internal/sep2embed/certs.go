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
// load-or-create contract in ensureServerIdentity depends on checking
// for exactly these four names.
const (
	caCertFileName     = "ca.pem"
	caKeyFileName      = "ca-key.pem"
	serverCertFileName = "server.pem"
	serverKeyFileName  = "server-key.pem"

	certDirPerm  = 0o700
	certFilePerm = 0o600
)

// ensureServerIdentity is the single load-or-create code path for the
// embedded server's mTLS material, per the bridge's 2026-05-08
// dev-mint-vs-preprovisioned decision (see cmd/bridge history for the
// original EnsureDeviceCert precedent this mirrors for server identity).
//
//   - If all four files already exist under dir, they are loaded as-is
//     and returned unchanged: this is the production path, where an
//     operator has placed preprovisioned CA and server material.
//   - If any of the four is missing, fresh dev-mint material is
//     generated (a self-signed CA plus a server leaf cert signed by it)
//     and all four files are written to dir with 0600 permissions (dir
//     itself created/left at 0700). This is the dev path.
//
// There is no partial-mint state: the check is all-four-present or
// mint-all-four, so a directory that already holds some but not all of
// the expected files gets a fresh, consistent set rather than a mix of
// old and new material.
//
// Each of the four files is written via writeFileAtomic (temp file in
// the same directory, then rename), so a process crash mid-write leaves
// either the old file (rename never happened) or the new one
// (rename is the last step), never a truncated PEM. allExist's
// presence-only check is therefore checking a set of files that are
// each internally all-or-nothing; it is not itself a parse/validate
// step (see the certs_test.go coverage for what happens when a file
// exists but is not valid PEM: that is caught downstream by
// sep2tls.NewServerTLSConfigWithExtraCAs when New wires the listener,
// not here).
//
// Returns the three file paths sep2srv.Options needs (CertFile, KeyFile,
// CAFile). The CA private key file is written and its path returned to
// nothing further inside this package, but it is NOT dead weight: a
// caller (or a dev-only tooling script) that wants to mint additional
// device certs trusted by this same dev CA, for local testing, needs to
// sign against caKeyFile. See certs_test.go and embed_test.go's
// mintTestDeviceClient, which do exactly that. Dropping it would break
// that local-signing path with no runtime benefit, since the file
// already carries 0600 permissions.
func ensureServerIdentity(dir string) (certFile, keyFile, caFile string, err error) {
	caFile = filepath.Join(dir, caCertFileName)
	caKeyFile := filepath.Join(dir, caKeyFileName)
	certFile = filepath.Join(dir, serverCertFileName)
	keyFile = filepath.Join(dir, serverKeyFileName)

	if allExist(caFile, caKeyFile, certFile, keyFile) {
		return certFile, keyFile, caFile, nil
	}

	log.Printf("sep2embed: WARNING: no complete pre-provisioned certificate material found in %q; minting a development-only self-signed CA and server certificate. DO NOT use this material in production; provide preprovisioned %s, %s, %s, and %s instead.",
		dir, caCertFileName, caKeyFileName, serverCertFileName, serverKeyFileName)

	if err := os.MkdirAll(dir, certDirPerm); err != nil {
		return "", "", "", fmt.Errorf("create cert dir %q: %w", dir, err)
	}

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
		if err := writeFileAtomic(w.path, w.data, certFilePerm); err != nil {
			return "", "", "", fmt.Errorf("write %s: %w", w.path, err)
		}
	}

	return certFile, keyFile, caFile, nil
}

// allExist reports whether every path in paths names a file that Stat
// succeeds on. A permission error or any other Stat failure counts as
// "does not exist" (fail toward re-minting rather than silently trusting
// a path this process cannot actually read).
func allExist(paths ...string) bool {
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
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
