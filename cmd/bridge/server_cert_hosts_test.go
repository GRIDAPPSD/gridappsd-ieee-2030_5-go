package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2config"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

func TestLoadConfigServerCertHosts(t *testing.T) {
	tests := []struct {
		name string
		env  string
		args []string
		want []string
	}{
		{"unset is nil", "", nil, nil},
		{"env list trimmed, empties dropped", "192.168.1.50, bridge.lan,,", nil, []string{"192.168.1.50", "bridge.lan"}},
		{"flag shadows env", "env.lan", []string{"-sep2-server-cert-hosts", "flag.lan,10.0.0.9"}, []string{"flag.lan", "10.0.0.9"}},
		{"ipv6 literal accepted", "fe80::1", nil, []string{"fe80::1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SEP2_SERVER_CERT_HOSTS", tc.env)
			cfg, err := loadConfig(tc.args)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if !reflect.DeepEqual(cfg.SEP2ServerCertHosts, tc.want) {
				t.Errorf("SEP2ServerCertHosts = %#v, want %#v", cfg.SEP2ServerCertHosts, tc.want)
			}
		})
	}
}

func TestLoadConfigServerCertHostsRefusesBadEntries(t *testing.T) {
	for _, bad := range []string{
		"*.example.com", "host:8443", "https://bridge.lan", "bridge.lan/x",
		"-lead.lan", "trail-.lan", "a..b", "under_score.lan", "999.1.1.1.1.", "999.1.1.1",
		"good.lan,bad host",
	} {
		t.Run(bad, func(t *testing.T) {
			t.Setenv("SEP2_SERVER_CERT_HOSTS", bad)
			_, err := loadConfig(nil)
			if err == nil {
				t.Fatalf("loadConfig accepted %q", bad)
			}
		})
	}
}

// The mint is reached through bootstrapRegistry (device identities first), so
// the configured names must land in the server.pem it writes.
func TestBootstrapRegistryMintsServerLeafWithConfiguredHosts(t *testing.T) {
	requester := &kindRoutedMockCIMRequester{
		inverterResp: singleRowEnvelope(t, pecUnitBinding("UNIT-1", "PEC-1", "Inverter 1")),
		solarResp:    singleRowEnvelope(t, pecUnitBinding("UNIT-1", "PEC-1", "Inverter 1")),
		batteryResp:  singleRowEnvelope(t, pecUnitBinding("PEC-1", "PEC-1", "Inverter 1")),
		countResp:    countEnvelope(t, 1),
	}
	certDir := t.TempDir()
	hosts := []string{"192.168.1.50", "bridge.lan"}
	if _, err := bootstrapRegistry(context.Background(), cim.NewClient(requester), "_DEADBEEF-0000-0000-0000-000000000123", certDir, sep2embed.DeviceCertModeDevMint, hosts, nil); err != nil {
		t.Fatalf("bootstrapRegistry: %v", err)
	}
	pemBytes, err := os.ReadFile(filepath.Join(certDir, "server.pem"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := sep2cert.ParseCertificatePEM(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range append([]string{"localhost", "127.0.0.1"}, hosts...) {
		if err := leaf.VerifyHostname(h); err != nil {
			t.Errorf("server.pem does not name %q: %v", h, err)
		}
	}
	if err := leaf.VerifyHostname("other.lan"); err == nil {
		t.Error("server.pem names an unrequested host (control)")
	}
}

func TestSEP2EmbedConfigCarriesServerCertHosts(t *testing.T) {
	cfg := config{SEP2ServerCertHosts: []string{"bridge.lan"}}
	got := sep2EmbedConfig(cfg, sep2config.SEP2Policy{}, nil, sep2embed.DeviceCertModeDevMint)
	if !reflect.DeepEqual(got.ServerCertHosts, []string{"bridge.lan"}) {
		t.Errorf("ServerCertHosts = %v", got.ServerCertHosts)
	}
}
