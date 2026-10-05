package sep2embed

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"
	gotls "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls/gotls"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

const (
	foreignLANIP   = "192.168.77.5"
	foreignLANName = "bridge.lan"
	foreignMRID    = "foreign-dev-1"
)

type foreignSet struct {
	dir          string
	servingCAPEM []byte
	devCertPEM   []byte
	devKeyPEM    []byte
	devCertDER   []byte
}

// buildForeignSet writes the files a certificate set made on another computer
// needs, using core's sep2cert only and none of this package's minting code:
// a device CA, a serving CA, a server leaf naming leafHosts signed by the
// serving CA, and one device certificate named by the documented scheme.
// Neither CA key is written.
func buildForeignSet(t *testing.T, leafHosts []string) foreignSet {
	t.Helper()

	devCA, devCAKey, devCACertPEM := genTestCA(t, "foreign device CA")
	srvCA, srvCAKey, srvCACertPEM := genTestCA(t, "foreign serving CA")

	leafPEM, leafKeyPEM, err := sep2cert.GenerateServerCert(srvCA, srvCAKey, sep2cert.ServerCertOptions{
		CommonName: "foreign server",
		Hosts:      leafHosts,
	})
	if err != nil {
		t.Fatalf("GenerateServerCert: %v", err)
	}
	devCertPEM, devKeyPEM, err := sep2cert.GenerateDeviceCert(devCA, devCAKey, sep2cert.DeviceCertOptions{
		DeviceType:  sep2cert.DeviceTypeGeneric,
		HWSerialNum: "foreign-serial-001",
		IsTestCert:  true,
	})
	if err != nil {
		t.Fatalf("GenerateDeviceCert: %v", err)
	}
	devDER, err := sep2cert.CertificateDER(devCertPEM)
	if err != nil {
		t.Fatalf("CertificateDER: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "certs")
	devicesDir := filepath.Join(dir, "devices")
	if err := os.MkdirAll(devicesDir, certDirPerm); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(foreignMRID))
	base := foreignMRID + "-" + hex.EncodeToString(sum[:8])
	for path, data := range map[string][]byte{
		filepath.Join(dir, "ca.pem"):            devCACertPEM,
		filepath.Join(dir, "serving-ca.pem"):    srvCACertPEM,
		filepath.Join(dir, "server.pem"):        leafPEM,
		filepath.Join(dir, "server-key.pem"):    leafKeyPEM,
		filepath.Join(devicesDir, base+".x509"): devDER,
		filepath.Join(devicesDir, base+".pem"):  devKeyPEM,
	} {
		if err := os.WriteFile(path, data, certFilePerm); err != nil {
			t.Fatal(err)
		}
	}
	return foreignSet{dir: dir, servingCAPEM: srvCACertPEM, devCertPEM: devCertPEM, devKeyPEM: devKeyPEM, devCertDER: devDER}
}

// runForeignServer starts the embedded server on a read-only mount of set and
// returns its address.
func runForeignServer(t *testing.T, set foreignSet) string {
	t.Helper()
	chmodForTest(t, filepath.Join(set.dir, "devices"), 0o500)
	chmodForTest(t, set.dir, 0o500)

	reg := registry.New()
	if err := reg.AddBatch(fixtureEntries()); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e, err := New(ctx, Config{
		Addr:                   "127.0.0.1:0",
		CertDir:                set.dir,
		DeviceCertMode:         DeviceCertModePreprovisioned,
		ResolveRegistrationPIN: testResolvePIN,
		ShutdownTimeout:        time.Second,
	}, reg)
	if err != nil {
		cancel()
		t.Fatalf("New against a foreign read-only set: %v", err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-runErr; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	})
	return e.Addr()
}

// dialAs completes an mTLS handshake with the set's device certificate,
// trusting only serving-ca.pem and verifying the server as serverName. The
// TCP dial goes to addr; serverName is the name a LAN client would use.
func dialAs(set foreignSet, addr, serverName string) (*x509.Certificate, error) {
	cfg, err := sepTLS.NewCCMClientConfigFromPEM(set.devCertPEM, set.devKeyPEM, set.servingCAPEM)
	if err != nil {
		return nil, err
	}
	cfg.ServerName = serverName
	conn, err := gotls.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0], nil
}

// A certificate set made elsewhere, mounted read-only, serves a client that
// trusts only serving-ca.pem and dials the LAN address the leaf names.
func TestForeignCertSetServesLANClientReadOnly(t *testing.T) {
	requireUnprivileged(t)
	set := buildForeignSet(t, []string{"localhost", "127.0.0.1", foreignLANIP, foreignLANName})

	ids, err := EnsureDeviceIdentities(set.dir, DeviceCertModePreprovisioned, []string{foreignMRID})
	if err != nil {
		t.Fatalf("EnsureDeviceIdentities on the foreign set: %v", err)
	}
	fp := sha256.Sum256(set.devCertDER)
	wantLFDI := hex.EncodeToString(fp[:20])
	if got := ids[foreignMRID].LFDI; !strings.EqualFold(got, wantLFDI) {
		t.Errorf("device LFDI = %q, want the SHA-256 of the DER, first 20 bytes: %q", got, wantLFDI)
	}

	before := snapshotTree(t, set.dir)
	addr := runForeignServer(t, set)

	for _, name := range []string{foreignLANIP, foreignLANName} {
		peer, err := dialAs(set, addr, name)
		if err != nil {
			t.Fatalf("handshake verifying the server as %q: %v", name, err)
		}
		if err := peer.VerifyHostname(name); err != nil {
			t.Errorf("served leaf does not name %q: %v", name, err)
		}
	}

	assertTreeUnchanged(t, before, snapshotTree(t, set.dir), "serving from the read-only foreign set")
}

// Control: the same set with a leaf that lacks the LAN address must fail the
// client's verification, so the success above is the SAN and not leniency.
func TestForeignCertSetLeafWithoutLANNameIsRefused(t *testing.T) {
	requireUnprivileged(t)
	set := buildForeignSet(t, []string{"localhost", "127.0.0.1"})
	addr := runForeignServer(t, set)

	if _, err := dialAs(set, addr, "127.0.0.1"); err != nil {
		t.Fatalf("control precondition: the loopback name must verify: %v", err)
	}
	_, err := dialAs(set, addr, foreignLANIP)
	if err == nil {
		t.Fatalf("a client verifying the server as %s succeeded against a leaf that does not name it", foreignLANIP)
	}
	var hostErr x509.HostnameError
	if !errors.As(err, &hostErr) {
		t.Errorf("handshake failed for another reason than the hostname: %v", err)
	}
}
