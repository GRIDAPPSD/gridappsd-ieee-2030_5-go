package sep2embed

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
)

func TestEnsureServerIdentityMintsWhenAbsent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certDir := filepath.Join(dir, "certs") // does not exist yet; MkdirAll must create it

	certFile, keyFile, caFile, err := ensureServerIdentity(certDir)
	if err != nil {
		t.Fatalf("ensureServerIdentity: %v", err)
	}

	if certFile != filepath.Join(certDir, serverCertFileName) {
		t.Errorf("certFile = %q, want %q", certFile, filepath.Join(certDir, serverCertFileName))
	}
	if keyFile != filepath.Join(certDir, serverKeyFileName) {
		t.Errorf("keyFile = %q, want %q", keyFile, filepath.Join(certDir, serverKeyFileName))
	}
	if caFile != filepath.Join(certDir, caCertFileName) {
		t.Errorf("caFile = %q, want %q", caFile, filepath.Join(certDir, caCertFileName))
	}

	// All four files exist (the CA key file is the one not returned).
	caKeyFile := filepath.Join(certDir, caKeyFileName)
	for _, p := range []string{certFile, keyFile, caFile, caKeyFile} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("Stat(%q): %v", p, err)
		}
		if perm := info.Mode().Perm(); perm != certFilePerm {
			t.Errorf("Stat(%q).Mode().Perm() = %o, want %o", p, perm, certFilePerm)
		}
	}

	dirInfo, err := os.Stat(certDir)
	if err != nil {
		t.Fatalf("Stat(certDir): %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != certDirPerm {
		t.Errorf("Stat(certDir).Mode().Perm() = %o, want %o", perm, certDirPerm)
	}

	// The minted material must actually parse as a valid CA-signed server
	// leaf: load-bearing for New's downstream sep2tls.NewServerTLSConfig
	// call, not just "files exist".
	caCertPEM, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("ReadFile(caFile): %v", err)
	}
	caCert, err := sep2cert.ParseCertificatePEM(caCertPEM)
	if err != nil {
		t.Fatalf("ParseCertificatePEM(ca): %v", err)
	}
	if !caCert.IsCA {
		t.Errorf("minted CA cert has IsCA = false")
	}

	serverCertPEM, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("ReadFile(certFile): %v", err)
	}
	serverCert, err := sep2cert.ParseCertificatePEM(serverCertPEM)
	if err != nil {
		t.Fatalf("ParseCertificatePEM(server): %v", err)
	}
	if err := serverCert.CheckSignatureFrom(caCert); err != nil {
		t.Errorf("minted server cert is not signed by the minted CA: %v", err)
	}
}

func TestEnsureServerIdentityLoadsWhenAllFourPresent(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()

	certFile1, keyFile1, caFile1, err := ensureServerIdentity(certDir)
	if err != nil {
		t.Fatalf("first ensureServerIdentity: %v", err)
	}
	original, err := os.ReadFile(certFile1)
	if err != nil {
		t.Fatalf("ReadFile(certFile1): %v", err)
	}

	// Second call against the SAME directory must load, not re-mint: the
	// server cert bytes on disk must be byte-for-byte unchanged.
	certFile2, keyFile2, caFile2, err := ensureServerIdentity(certDir)
	if err != nil {
		t.Fatalf("second ensureServerIdentity: %v", err)
	}
	if certFile1 != certFile2 || keyFile1 != keyFile2 || caFile1 != caFile2 {
		t.Fatalf("second call returned different paths: (%q,%q,%q) vs (%q,%q,%q)",
			certFile1, keyFile1, caFile1, certFile2, keyFile2, caFile2)
	}

	reloaded, err := os.ReadFile(certFile2)
	if err != nil {
		t.Fatalf("ReadFile(certFile2): %v", err)
	}
	if string(original) != string(reloaded) {
		t.Fatalf("server cert bytes changed across calls: load-or-create re-minted instead of loading")
	}
}

func TestParseCAPairRejectsInvalidPEM(t *testing.T) {
	t.Parallel()

	validCertPEM, validKeyPEM, err := sep2cert.GenerateCA(sep2cert.CAOptions{CommonName: "test"})
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	if _, _, err := parseCAPair([]byte("not a cert"), validKeyPEM); err == nil {
		t.Error("parseCAPair with invalid cert PEM: want error, got nil")
	}
	if _, _, err := parseCAPair(validCertPEM, []byte("not a key")); err == nil {
		t.Error("parseCAPair with invalid key PEM: want error, got nil")
	}
}

func TestEnsureServerIdentityRemintsWhenOnlySomeFilesPresent(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()

	// Simulate a partially-populated directory: only the CA cert exists.
	if err := os.MkdirAll(certDir, certDirPerm); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, caCertFileName), []byte("not a real cert"), certFilePerm); err != nil {
		t.Fatalf("WriteFile stub ca.pem: %v", err)
	}

	certFile, keyFile, caFile, err := ensureServerIdentity(certDir)
	if err != nil {
		t.Fatalf("ensureServerIdentity with partial dir: %v", err)
	}

	// allExist requires ALL four; the stub ca.pem alone must not satisfy
	// the load path, so a fresh, consistent, parseable set is written.
	caCertPEM, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("ReadFile(caFile): %v", err)
	}
	if string(caCertPEM) == "not a real cert" {
		t.Fatalf("stub ca.pem was not replaced: load-or-create incorrectly treated a partial dir as complete")
	}
	if _, err := sep2cert.ParseCertificatePEM(caCertPEM); err != nil {
		t.Fatalf("re-minted ca.pem does not parse: %v", err)
	}
	for _, p := range []string{certFile, keyFile} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("Stat(%q) after remint: %v", p, err)
		}
	}
}
