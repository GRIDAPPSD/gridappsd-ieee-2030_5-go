package sep2embed

import (
	"crypto/ecdsa"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
)

// preprovisionedMaterial is one operator-supplied certificate set, held
// in memory alongside the bytes actually written to disk so a test can
// compare disk contents before and after a call byte for byte.
//
// The CA private key is deliberately a field on this struct and NOT
// written to disk: it is what an operator keeps on an offline issuing
// host. Tests that need to sign a client cert against this CA use the
// in-memory key; nothing under the cert dir ever holds it.
type preprovisionedMaterial struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey

	caCertPEM     []byte
	serverCertPEM []byte
	serverKeyPEM  []byte
}

// writePreprovisionedServerMaterial lays down the file set a real
// operator deploys for a bridge that never signs anything: the CA
// certificate, the server leaf certificate, and the server private key.
// ca-key.pem is deliberately absent, because a process that only ever
// loads operator-issued certificates has no legitimate need for the CA
// signing key on the box (see loadDeviceSigningCA's least-privilege
// note).
//
// Returns the material so a caller can assert the exact bytes are still
// on disk afterwards.
func writePreprovisionedServerMaterial(t *testing.T, dir string) preprovisionedMaterial {
	t.Helper()

	caCert, caKey, caCertPEM := genTestCA(t, "operator-offline-issuing-ca")

	serverCertPEM, serverKeyPEM, err := sep2cert.GenerateServerCert(caCert, caKey, sep2cert.ServerCertOptions{
		CommonName: "operator-supplied embedded server",
		Hosts:      []string{"localhost", "127.0.0.1"},
	})
	if err != nil {
		t.Fatalf("GenerateServerCert: %v", err)
	}

	if err := os.MkdirAll(dir, certDirPerm); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	writes := []struct {
		name string
		data []byte
	}{
		{caCertFileName, caCertPEM},
		{serverCertFileName, serverCertPEM},
		{serverKeyFileName, serverKeyPEM},
	}
	for _, w := range writes {
		if err := os.WriteFile(filepath.Join(dir, w.name), w.data, certFilePerm); err != nil {
			t.Fatalf("WriteFile(%q): %v", w.name, err)
		}
	}

	// Load-bearing precondition: the CA private key must not be on disk,
	// because its absence is precisely the condition that used to trigger
	// a mint over the three files just written.
	if _, statErr := os.Stat(filepath.Join(dir, caKeyFileName)); statErr == nil {
		t.Fatalf("test setup wrote %s; the condition this fixture exists to exercise is violated", caKeyFileName)
	}

	return preprovisionedMaterial{
		caCert:        caCert,
		caKey:         caKey,
		caCertPEM:     caCertPEM,
		serverCertPEM: serverCertPEM,
		serverKeyPEM:  serverKeyPEM,
	}
}

// assertFileBytes fails the test unless the file at dir/name holds
// exactly want. This is the destruction check: a call that returned no
// error has still destroyed operator material if these bytes moved.
func assertFileBytes(t *testing.T, dir, name string, want []byte) {
	t.Helper()

	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", name, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s was rewritten: %d bytes on disk, want the operator's original %d bytes\n on disk: %.80q\noriginal: %.80q",
			name, len(got), len(want), got, want)
	}
}

// assertNoFile fails the test if dir/name exists.
func assertNoFile(t *testing.T, dir, name string) {
	t.Helper()

	if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
		t.Errorf("%s exists in %q; it must not have been created", name, dir)
	}
}

// TestEnsureServerIdentityNeverOverwritesOperatorMaterial is the
// regression test for the defect this file exists to fix: an operator
// preprovisions a real CA certificate, server certificate and server key
// and deliberately withholds the CA private key, which is correct
// practice for a process that never signs. The old all-four-or-mint
// check read that withheld key as "incomplete" and renamed freshly
// minted, self-signed development material over the operator's real
// certificate AND their real private key, then served it.
//
// The assertion is on the bytes on disk, not on the returned error: a
// call that returns nil has still destroyed the deployment if those
// bytes moved.
func TestEnsureServerIdentityNeverOverwritesOperatorMaterial(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	material := writePreprovisionedServerMaterial(t, dir)

	certFile, keyFile, caFile, err := ensureServerIdentity(dir, DeviceCertModePreprovisioned)
	if err != nil {
		t.Fatalf("ensureServerIdentity on a complete preprovisioned set: %v", err)
	}

	if want := filepath.Join(dir, serverCertFileName); certFile != want {
		t.Errorf("certFile = %q, want %q", certFile, want)
	}
	if want := filepath.Join(dir, serverKeyFileName); keyFile != want {
		t.Errorf("keyFile = %q, want %q", keyFile, want)
	}
	if want := filepath.Join(dir, caCertFileName); caFile != want {
		t.Errorf("caFile = %q, want %q", caFile, want)
	}

	assertFileBytes(t, dir, caCertFileName, material.caCertPEM)
	assertFileBytes(t, dir, serverCertFileName, material.serverCertPEM)
	assertFileBytes(t, dir, serverKeyFileName, material.serverKeyPEM)

	// The CA signing key must stay off the host: a mode that never signs
	// must not have invented one.
	assertNoFile(t, dir, caKeyFileName)
}
