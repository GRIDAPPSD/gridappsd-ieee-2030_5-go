package cimstomp

import (
	"crypto/tls"
	"testing"
)

// TestSTOMPConfig_TLSFieldNilByDefault locks in the backward-compatibility
// contract: a freshly constructed STOMPConfig has no TLS configured, and
// the nil sentinel is the documented signal for "use plain TCP".
func TestSTOMPConfig_TLSFieldNilByDefault(t *testing.T) {
	cfg := STOMPConfig{Address: "127.0.0.1:61613", User: "u", Password: "p"}
	if cfg.TLS != nil {
		t.Fatalf("STOMPConfig.TLS default = %v, want nil", cfg.TLS)
	}
}

// TestSTOMPConfig_TLSFieldSettable verifies the new field accepts a
// caller-provided *tls.Config. cimstomp does not own cert-loading policy:
// the caller hands in a fully populated Config (Certificates, RootCAs,
// MinVersion, ServerName) for mTLS deployments.
func TestSTOMPConfig_TLSFieldSettable(t *testing.T) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "broker.example"}
	cfg := STOMPConfig{
		Address:  "127.0.0.1:61614",
		User:     "u",
		Password: "p",
		TLS:      tlsCfg,
	}
	if cfg.TLS != tlsCfg {
		t.Fatalf("STOMPConfig.TLS round-trip mismatch: got %p, want %p", cfg.TLS, tlsCfg)
	}
	if cfg.TLS.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want VersionTLS12", cfg.TLS.MinVersion)
	}
	if cfg.TLS.ServerName != "broker.example" {
		t.Errorf("ServerName = %q, want broker.example", cfg.TLS.ServerName)
	}
}
