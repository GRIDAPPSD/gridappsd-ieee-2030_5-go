package sep2embed

import (
	"crypto/ecdsa"
	"crypto/x509"
	"fmt"
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
// Returns the three file paths sep2srv.Options needs (CertFile, KeyFile,
// CAFile); the CA private key file is written but never returned, since
// nothing past this function needs to sign anything with it.
func ensureServerIdentity(dir string) (certFile, keyFile, caFile string, err error) {
	caFile = filepath.Join(dir, caCertFileName)
	caKeyFile := filepath.Join(dir, caKeyFileName)
	certFile = filepath.Join(dir, serverCertFileName)
	keyFile = filepath.Join(dir, serverKeyFileName)

	if allExist(caFile, caKeyFile, certFile, keyFile) {
		return certFile, keyFile, caFile, nil
	}

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
		if err := os.WriteFile(w.path, w.data, certFilePerm); err != nil {
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
