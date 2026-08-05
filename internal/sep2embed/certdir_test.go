package sep2embed

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
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

// requireUnprivileged skips a test that drives a permission-denied path.
// Root bypasses the discretionary permission bits these tests set, so on
// a root test runner (a CI container, typically) the chmod would be a
// no-op and the test would silently assert nothing.
func requireUnprivileged(t *testing.T) {
	t.Helper()

	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny this process, so the permission path under test cannot be driven")
	}
}

// chmodForTest sets dir's mode and restores it at test cleanup, so
// t.TempDir's own RemoveAll can still descend into it.
func chmodForTest(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()

	if err := os.Chmod(dir, mode); err != nil {
		t.Fatalf("Chmod(%q, %o): %v", dir, mode, err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, certDirPerm); err != nil {
			t.Errorf("restore mode on %q: %v", dir, err)
		}
	})
}

// TestCompletePreprovisionedDirIsUsableReadOnly is the deployment-shape
// proof. The target is a bind-mounted volume an operator can mount
// read-only, which is only viable if a complete set is loaded with no
// write attempted anywhere. The server is actually started and dialed,
// and the leaf it presents on the wire is compared, DER for DER, against
// the operator's own server.pem: "New returned no error" would not prove
// the operator's material is what is being served.
func TestCompletePreprovisionedDirIsUsableReadOnly(t *testing.T) {
	t.Parallel()
	requireUnprivileged(t)

	certDir := filepath.Join(t.TempDir(), "certs")
	material := writePreprovisionedServerMaterial(t, certDir)

	// r-x: readable and traversable, not writable. Any attempt to create,
	// rename or link a file in here now fails.
	chmodForTest(t, certDir, 0o500)

	reg := registry.New()
	if err := reg.AddBatch(fixtureEntries()); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e, err := New(ctx, Config{
		Addr:                   "127.0.0.1:0",
		CertDir:                certDir,
		DeviceCertMode:         DeviceCertModePreprovisioned,
		ResolveRegistrationPIN: testResolvePIN,
		ShutdownTimeout:        time.Second,
	}, reg)
	if err != nil {
		t.Fatalf("New against a complete, read-only preprovisioned dir: %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()

	servedLeaf := dialCapturingServerLeaf(t, e.Addr(), material)

	wantLeaf, err := sep2cert.CertificateDER(material.serverCertPEM)
	if err != nil {
		t.Fatalf("CertificateDER(operator server.pem): %v", err)
	}
	if !bytes.Equal(servedLeaf, wantLeaf) {
		t.Errorf("the server presented a leaf certificate that is not the operator's server.pem (%d served bytes vs %d operator bytes)",
			len(servedLeaf), len(wantLeaf))
	}

	// Nothing was written, and in particular no CA signing key was
	// invented on a host that must not hold one.
	assertFileBytes(t, certDir, caCertFileName, material.caCertPEM)
	assertFileBytes(t, certDir, serverCertFileName, material.serverCertPEM)
	assertFileBytes(t, certDir, serverKeyFileName, material.serverKeyPEM)
	assertNoFile(t, certDir, caKeyFileName)
	assertOnlyCertFiles(t, certDir, caCertFileName, serverCertFileName, serverKeyFileName)

	cancel()
	if err := <-runErr; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("Run: %v", err)
	}
}

// dialCapturingServerLeaf completes one mTLS handshake against addr,
// presenting a device certificate signed by material's CA, and returns
// the DER bytes of the leaf certificate the SERVER presented.
func dialCapturingServerLeaf(t *testing.T, addr string, material preprovisionedMaterial) []byte {
	t.Helper()

	devCertPEM, devKeyPEM, err := sep2cert.GenerateDeviceCert(material.caCert, material.caKey, sep2cert.DeviceCertOptions{
		DeviceType:  sep2cert.DeviceTypeGeneric,
		HWSerialNum: "read-only-dir-probe-001",
		IsTestCert:  true,
	})
	if err != nil {
		t.Fatalf("GenerateDeviceCert: %v", err)
	}
	tlsCfg, err := sepTLS.NewClientTLSConfigFromPEM(devCertPEM, devKeyPEM, material.caCertPEM)
	if err != nil {
		t.Fatalf("NewClientTLSConfigFromPEM: %v", err)
	}
	// Trust stays pinned to the operator's CA via RootCAs; only the
	// hostname match is skipped, since the test dials by IP:port. Same
	// posture as embed_test.go's mintTestDeviceClient.
	tlsCfg.InsecureSkipVerify = true //nolint:gosec // trust pinned via RootCAs above

	conn, err := tls.Dial("tcp", addr, tlsCfg)
	if err != nil {
		t.Fatalf("tls.Dial(%q): %v", addr, err)
	}
	defer conn.Close()

	peers := conn.ConnectionState().PeerCertificates
	if len(peers) == 0 {
		t.Fatal("server presented no certificate")
	}
	return peers[0].Raw
}

// assertOnlyCertFiles fails the test if certDir holds any entry beyond
// want. It is what catches a write probe, or a temp file from an aborted
// write, left behind in an operator's certificate directory.
func assertOnlyCertFiles(t *testing.T, certDir string, want ...string) {
	t.Helper()

	entries, err := os.ReadDir(certDir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", certDir, err)
	}
	allowed := make(map[string]bool, len(want))
	for _, name := range want {
		allowed[name] = true
	}
	for _, e := range entries {
		if !allowed[e.Name()] {
			t.Errorf("unexpected entry %q left in certificate directory %q", e.Name(), certDir)
		}
	}
}

// TestEnsureServerIdentityRefusesEmptyUnwritableDir covers the state an
// operator hits when a bind mount is read-only but empty, typically
// because the host path they meant to mount does not exist. There is
// nothing to load and nothing can be minted, so the process must die
// loudly, naming what is missing and saying it could not write.
func TestEnsureServerIdentityRefusesEmptyUnwritableDir(t *testing.T) {
	t.Parallel()
	requireUnprivileged(t)

	certDir := filepath.Join(t.TempDir(), "certs")
	if err := os.MkdirAll(certDir, certDirPerm); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	chmodForTest(t, certDir, 0o500)

	_, _, _, err := ensureServerIdentity(certDir, DeviceCertModePreprovisioned)
	if err == nil {
		t.Fatal("ensureServerIdentity on an empty unwritable directory: want an error, got nil")
	}
	if !errors.Is(err, errCertDirNotWritable) {
		t.Errorf("error = %v, want one matching errCertDirNotWritable", err)
	}

	msg := err.Error()
	// Every required file is named, so an operator knows what to supply.
	for _, want := range requiredServerCertFiles(DeviceCertModePreprovisioned) {
		if !strings.Contains(msg, want) {
			t.Errorf("error message does not name the missing %s: %v", want, err)
		}
	}
	// A mode that never signs must not tell an operator to supply the CA
	// signing key: doing so would be advice to put it on the host.
	if strings.Contains(msg, caKeyFileName) {
		t.Errorf("error message names %s, which a non-signing mode does not require: %v", caKeyFileName, err)
	}
	if !strings.Contains(msg, "written to") {
		t.Errorf("error message does not say the directory could not be written to: %v", err)
	}

	assertOnlyCertFiles(t, certDir)
}

// TestEnsureServerIdentityRefusesUnreadableDir is the EACCES-is-not-
// absence proof. A cert dir this process cannot read is a permission
// problem to surface, not an empty directory to fill: the old Stat check
// collapsed the two, so a fully provisioned directory whose permissions
// were wrong presented itself as empty and was minted into.
func TestEnsureServerIdentityRefusesUnreadableDir(t *testing.T) {
	t.Parallel()
	requireUnprivileged(t)

	certDir := filepath.Join(t.TempDir(), "certs")
	material := writePreprovisionedServerMaterial(t, certDir)

	// No permissions at all: Stat on the children fails with EACCES
	// rather than ENOENT.
	chmodForTest(t, certDir, 0o000)

	_, _, _, err := ensureServerIdentity(certDir, DeviceCertModePreprovisioned)
	if err == nil {
		t.Fatal("ensureServerIdentity on an unreadable directory: want an error, got nil")
	}
	if !errors.Is(err, errCertDirUnreadable) {
		t.Errorf("error = %v, want one matching errCertDirUnreadable", err)
	}
	if errors.Is(err, errCertDirNotWritable) || errors.Is(err, errCertDirPartial) {
		t.Errorf("an unreadable directory was classified as empty or partial: %v", err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("error does not carry the underlying permission error: %v", err)
	}

	// Restore access and prove nothing was minted over the material that
	// was there all along.
	if err := os.Chmod(certDir, certDirPerm); err != nil {
		t.Fatalf("Chmod restore: %v", err)
	}
	assertFileBytes(t, certDir, caCertFileName, material.caCertPEM)
	assertFileBytes(t, certDir, serverCertFileName, material.serverCertPEM)
	assertFileBytes(t, certDir, serverKeyFileName, material.serverKeyPEM)
	assertNoFile(t, certDir, caKeyFileName)
}

// TestCertDirWritableLeavesNoResidue pins the probe's cleanup. A
// writability check that leaves a file behind in an operator's
// certificate directory has traded one defect for a smaller one.
func TestCertDirWritableLeavesNoResidue(t *testing.T) {
	t.Parallel()

	certDir := filepath.Join(t.TempDir(), "certs")

	if err := certDirWritable(certDir); err != nil {
		t.Fatalf("certDirWritable on a writable path: %v", err)
	}
	// The directory was created by the probe, and holds nothing.
	assertOnlyCertFiles(t, certDir)

	// A second call is equally clean: the probe name is generated per
	// call, so a leftover from the first would show up here.
	if err := certDirWritable(certDir); err != nil {
		t.Fatalf("second certDirWritable: %v", err)
	}
	assertOnlyCertFiles(t, certDir)
}

// TestWriteFileNoClobberRefusesExistingFile is the last-line guard: even
// if a future edit reintroduced a code path that reaches the mint writes
// with material already on disk, the write itself must fail rather than
// replace. The check is on the bytes, not just the error.
func TestWriteFileNoClobberRefusesExistingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "server-key.pem")
	operator := []byte("-----BEGIN PRIVATE KEY-----\noperator material\n-----END PRIVATE KEY-----\n")
	if err := os.WriteFile(path, operator, certFilePerm); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := writeFileNoClobber(path, []byte("minted material"), certFilePerm)
	if err == nil {
		t.Fatal("writeFileNoClobber over an existing file: want an error, got nil")
	}
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("error = %v, want one matching fs.ErrExist", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(operator) {
		t.Errorf("file was replaced: %q, want %q", got, operator)
	}

	// The temp file it staged the write in is gone.
	assertOnlyCertFiles(t, dir, "server-key.pem")
}

// TestWriteFileNoClobberWritesAndCleansUp covers the success path: the
// exact bytes land at the target with the requested permissions, and the
// staging temp file is removed (os.Link, unlike os.Rename, leaves its
// source in place, so the cleanup is load bearing here in a way it was
// not for writeFileAtomic).
func TestWriteFileNoClobberWritesAndCleansUp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "target.pem")
	want := []byte("all-or-nothing contents")

	if err := writeFileNoClobber(path, want, certFilePerm); err != nil {
		t.Fatalf("writeFileNoClobber: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("contents = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != certFilePerm {
		t.Errorf("Mode().Perm() = %o, want %o", perm, certFilePerm)
	}
	assertOnlyCertFiles(t, dir, "target.pem")
}

// TestClassifyCertDirModeRequiredSets pins which files each mode
// requires, which is the decision the whole fix turns on: the same
// directory must be complete for a mode that never signs and partial for
// one that does.
func TestClassifyCertDirModeRequiredSets(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		write     []string
		mode      DeviceCertMode
		wantState certDirState
	}{
		{
			name:      "no CA key is complete for a mode that never signs",
			write:     []string{caCertFileName, serverCertFileName, serverKeyFileName},
			mode:      DeviceCertModePreprovisioned,
			wantState: certDirComplete,
		},
		{
			name:      "no CA key is partial for a signing mode",
			write:     []string{caCertFileName, serverCertFileName, serverKeyFileName},
			mode:      DeviceCertModeDevMint,
			wantState: certDirPartial,
		},
		{
			name:      "all four is complete for a signing mode",
			write:     []string{caCertFileName, caKeyFileName, serverCertFileName, serverKeyFileName},
			mode:      DeviceCertModeDevMint,
			wantState: certDirComplete,
		},
		{
			name:      "a spare CA key alone is partial, not empty, for a mode that does not require it",
			write:     []string{caKeyFileName},
			mode:      DeviceCertModePreprovisioned,
			wantState: certDirPartial,
		},
		{
			name:      "nothing at all is empty",
			write:     nil,
			mode:      DeviceCertModePreprovisioned,
			wantState: certDirEmpty,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			for _, name := range tc.write {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), certFilePerm); err != nil {
					t.Fatalf("WriteFile(%q): %v", name, err)
				}
			}

			state, _, _, err := classifyCertDir(dir, tc.mode)
			if err != nil {
				t.Fatalf("classifyCertDir: %v", err)
			}
			if state != tc.wantState {
				t.Errorf("state = %d, want %d", state, tc.wantState)
			}
		})
	}
}

// TestServerIdentityIsFixedForTheProcessLifetime pins a correctness
// requirement that no other test in this package would catch: the
// server's own identity is read from disk exactly once, at startup, and
// is never re-read.
//
// This is deliberately NOT symmetric with the device-cert lookup below,
// which re-reads per request. The CA certificate is the trust anchor
// every already-registered client's chain was verified against, and the
// server key backs every live TLS session; picking up a replacement
// mid-run would break both, silently, for clients that had been working.
// A server whose identity can change underneath it is worse than one
// that refuses to start.
//
// The test drives it the only way that proves anything: it replaces
// server.pem and server-key.pem on disk while the server is running,
// with a different leaf signed by the same CA (so a client would still
// trust the chain and the swap would go unnoticed), then dials again and
// asserts the ORIGINAL leaf is still presented. A future refactor that
// helpfully adds a watcher or a reload fails here.
func TestServerIdentityIsFixedForTheProcessLifetime(t *testing.T) {
	t.Parallel()

	certDir := filepath.Join(t.TempDir(), "certs")
	material := writePreprovisionedServerMaterial(t, certDir)

	reg := registry.New()
	if err := reg.AddBatch(fixtureEntries()); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e, err := New(ctx, Config{
		Addr:                   "127.0.0.1:0",
		CertDir:                certDir,
		DeviceCertMode:         DeviceCertModePreprovisioned,
		ResolveRegistrationPIN: testResolvePIN,
		ShutdownTimeout:        time.Second,
	}, reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()

	identityBefore := e.Identity()
	leafBefore := dialCapturingServerLeaf(t, e.Addr(), material)

	// Act as an operator (or an attacker with write access) rotating the
	// leaf underneath the running process. Written with os.WriteFile
	// rather than through this package's own writers, because the point
	// is that the FILES changed, by whatever means.
	replacementCertPEM, replacementKeyPEM, err := sep2cert.GenerateServerCert(material.caCert, material.caKey, sep2cert.ServerCertOptions{
		CommonName: "replacement server leaf",
		Hosts:      []string{"localhost", "127.0.0.1"},
	})
	if err != nil {
		t.Fatalf("GenerateServerCert(replacement): %v", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, serverCertFileName), replacementCertPEM, certFilePerm); err != nil {
		t.Fatalf("WriteFile(replacement server.pem): %v", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, serverKeyFileName), replacementKeyPEM, certFilePerm); err != nil {
		t.Fatalf("WriteFile(replacement server-key.pem): %v", err)
	}

	// Load-bearing precondition: the replacement really is a different
	// certificate, so "unchanged" below means something.
	replacementDER, err := sep2cert.CertificateDER(replacementCertPEM)
	if err != nil {
		t.Fatalf("CertificateDER(replacement): %v", err)
	}
	if bytes.Equal(replacementDER, leafBefore) {
		t.Fatal("the replacement leaf is identical to the original; this test would pass vacuously")
	}

	leafAfter := dialCapturingServerLeaf(t, e.Addr(), material)
	if !bytes.Equal(leafAfter, leafBefore) {
		t.Error("the running server picked up a replacement leaf certificate from disk; server identity must be fixed at startup for the process lifetime")
	}
	if bytes.Equal(leafAfter, replacementDER) {
		t.Error("the running server is serving the on-disk replacement leaf")
	}

	identityAfter := e.Identity()
	if identityAfter.LFDI != identityBefore.LFDI {
		t.Errorf("Identity().LFDI changed mid-run: %q -> %q", identityBefore.LFDI, identityAfter.LFDI)
	}
	if identityAfter.SFDI != identityBefore.SFDI {
		t.Errorf("Identity().SFDI changed mid-run: %q -> %q", identityBefore.SFDI, identityAfter.SFDI)
	}

	cancel()
	if err := <-runErr; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("Run: %v", err)
	}
}

// TestDeviceCertLookupReevaluatesDirectoryPerRequest is the other half
// of the re-evaluation split, and the reason a read-only preprovisioned
// certificate directory is workable in production: the per-device
// lookup consults the filesystem at the moment it is asked, never a
// listing taken at startup. An operator drops a new device's certificate
// into the directory and the next lookup finds it, with no restart.
//
// Server identity is emphatically NOT re-evaluated; see
// TestServerIdentityIsFixedForTheProcessLifetime above.
func TestDeviceCertLookupReevaluatesDirectoryPerRequest(t *testing.T) {
	t.Parallel()

	certDir := filepath.Join(t.TempDir(), "certs")
	material := writePreprovisionedServerMaterial(t, certDir)

	const lateMRID = "mrid-device-provisioned-after-startup"

	// First request, before the operator has supplied anything for this
	// device: fail closed, naming the path.
	if _, err := EnsureDeviceIdentities(certDir, DeviceCertModePreprovisioned, []string{lateMRID}); err == nil {
		t.Fatal("lookup for an unprovisioned device: want a fail-closed error, got nil")
	}

	// The failed lookup must not have written anything, in particular not
	// a minted stand-in.
	assertOnlyCertFiles(t, certDir, caCertFileName, serverCertFileName, serverKeyFileName)

	// The operator now drops the device's certificate in, with the
	// process still running.
	devCertPEM, _, err := sep2cert.GenerateDeviceCert(material.caCert, material.caKey, sep2cert.DeviceCertOptions{
		DeviceType:  sep2cert.DeviceTypeGeneric,
		HWSerialNum: "late-provisioned-serial-001",
	})
	if err != nil {
		t.Fatalf("GenerateDeviceCert: %v", err)
	}
	wantCert, err := sep2cert.ParseCertificatePEM(devCertPEM)
	if err != nil {
		t.Fatalf("ParseCertificatePEM: %v", err)
	}
	devCertDER, err := sep2cert.CertificateDER(devCertPEM)
	if err != nil {
		t.Fatalf("CertificateDER: %v", err)
	}
	base, err := deviceCertFileBase(lateMRID)
	if err != nil {
		t.Fatalf("deviceCertFileBase: %v", err)
	}
	devicesDir := filepath.Join(certDir, deviceCertDirName)
	if err := os.MkdirAll(devicesDir, certDirPerm); err != nil {
		t.Fatalf("MkdirAll(devicesDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(devicesDir, base+".x509"), devCertDER, certFilePerm); err != nil {
		t.Fatalf("WriteFile(device cert): %v", err)
	}

	// Second request, same process, no restart: the directory is
	// consulted again and the new material is found.
	got, err := EnsureDeviceIdentities(certDir, DeviceCertModePreprovisioned, []string{lateMRID})
	if err != nil {
		t.Fatalf("lookup after the operator supplied the certificate: %v", err)
	}
	identity, ok := got[lateMRID]
	if !ok {
		t.Fatalf("no identity returned for mRID %q", lateMRID)
	}
	// Value assertion, not just "no error": the identity must be the one
	// derived from the operator's certificate, per spec section 6.3.4
	// (LFDI) and 6.3.3 (SFDI).
	if want := sepTLS.LFDI(wantCert); identity.LFDI != want {
		t.Errorf("LFDI = %q, want %q (derived from the late-provisioned cert)", identity.LFDI, want)
	}
	if want := sepTLS.SFDI(wantCert); identity.SFDI != want {
		t.Errorf("SFDI = %q, want %q (derived from the late-provisioned cert)", identity.SFDI, want)
	}

	// Still no CA signing key on this host, and no minted key beside the
	// operator's certificate.
	assertNoFile(t, certDir, caKeyFileName)
	assertOnlyCertFiles(t, devicesDir, base+".x509")
}
