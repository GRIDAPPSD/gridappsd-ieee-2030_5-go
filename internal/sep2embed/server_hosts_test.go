package sep2embed

import (
	"bytes"
	"crypto/x509"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
)

func TestServerCertHostsKeepsLoopbackAndDedupes(t *testing.T) {
	t.Parallel()
	got := serverCertHosts([]string{"192.168.1.9", "localhost", "bridge.lan", "192.168.1.9"})
	want := []string{"localhost", "127.0.0.1", "192.168.1.9", "bridge.lan"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("serverCertHosts = %v, want %v", got, want)
	}
}

func readLeaf(t *testing.T, dir string) *x509.Certificate {
	t.Helper()
	pemBytes, err := os.ReadFile(filepath.Join(dir, serverCertFileName))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := sep2cert.ParseCertificatePEM(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

func TestEnsureServerIdentityMintsExtraHostsIntoLeaf(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, _, _, err := ensureServerIdentity(dir, DeviceCertModeDevMint, "192.168.1.9", "bridge.lan"); err != nil {
		t.Fatal(err)
	}
	leaf := readLeaf(t, dir)
	for _, h := range []string{"localhost", "127.0.0.1", "192.168.1.9", "bridge.lan"} {
		if err := leaf.VerifyHostname(h); err != nil {
			t.Errorf("minted leaf does not name %q: %v", h, err)
		}
	}
	if err := leaf.VerifyHostname("192.168.1.10"); err == nil {
		t.Error("minted leaf names an address nobody asked for (control)")
	}
}

// An existing leaf is not re-minted: the hosts setting is ignored for it, and
// the warning is the only signal.
func TestWarnMissingServerHostsNamesWhatTheExistingLeafLacks(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, err := ensureServerIdentity(dir, DeviceCertModeDevMint); err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(dir, serverCertFileName)
	before, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	if _, _, _, err := ensureServerIdentity(dir, DeviceCertModeDevMint, "192.168.1.9"); err != nil {
		t.Fatal(err)
	}
	warnMissingServerHosts(certFile, []string{"192.168.1.9", "localhost"})

	after, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the existing server certificate was re-minted")
	}
	out := buf.String()
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "does not name [192.168.1.9];") {
		t.Errorf("want a warning naming exactly the missing host: %q", out)
	}

	buf.Reset()
	warnMissingServerHosts(certFile, []string{"localhost", "127.0.0.1"})
	warnMissingServerHosts(certFile, nil)
	if buf.Len() != 0 {
		t.Errorf("warned although the leaf names every requested host: %q", buf.String())
	}
}
