package main

import (
	"context"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/sim"
)

// TestBusConfigMapsFields verifies busConfig's field-by-field mapping
// from the bridge's own config onto gridappsd.Config, in particular
// that AllowPlaintext passes through unmodified in both directions
// rather than being silently forced to a fixed value in the wiring
// step. This is what stands in for a live-broker connect test at this
// layer; the end-to-end proof against a real broker is GAGO-040's job.
func TestBusConfigMapsFields(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  config
	}{
		{
			name: "plaintext left at the fail-closed default",
			cfg: config{
				STOMPAddr:      "127.0.0.1:61613",
				STOMPUser:      "system",
				STOMPPassword:  "manager",
				AllowPlaintext: false,
			},
		},
		{
			name: "plaintext explicitly opted in",
			cfg: config{
				STOMPAddr:      "broker.example:61613",
				STOMPUser:      "u",
				STOMPPassword:  "p",
				AllowPlaintext: true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := busConfig(tc.cfg)
			if got.Address != tc.cfg.STOMPAddr {
				t.Errorf("Address: got %q, want %q", got.Address, tc.cfg.STOMPAddr)
			}
			if got.User != tc.cfg.STOMPUser {
				t.Errorf("User: got %q, want %q", got.User, tc.cfg.STOMPUser)
			}
			if got.Password != tc.cfg.STOMPPassword {
				t.Errorf("Password: got %q, want %q", got.Password, tc.cfg.STOMPPassword)
			}
			if got.AllowPlaintext != tc.cfg.AllowPlaintext {
				t.Errorf("AllowPlaintext: got %v, want %v", got.AllowPlaintext, tc.cfg.AllowPlaintext)
			}
			if got.TLSConfig != nil {
				t.Errorf("TLSConfig: got %v, want nil (bridge does not yet supply a custom TLS config)", got.TLSConfig)
			}
		})
	}
}

// TestBusConfigDefaultIsFailClosed verifies that a zero-value config
// (as loadConfig produces when no plaintext override is set) maps to
// a gridappsd.Config that dials TLS, not plaintext. This is the
// wiring-layer half of the fail-closed invariant; loadConfig's own
// default-off behavior is covered in config_test.go.
func TestBusConfigDefaultIsFailClosed(t *testing.T) {
	t.Parallel()

	got := busConfig(config{})
	if got.AllowPlaintext {
		t.Errorf("AllowPlaintext: got true for a zero-value config, want false (fail-closed default)")
	}
	if got.TLSConfig != nil {
		t.Errorf("TLSConfig: got %v, want nil", got.TLSConfig)
	}
}

// TestSEP2EmbedConfigMapsFields verifies sep2EmbedConfig's field-by-field
// mapping from the bridge's own config onto sep2embed.Config, mirroring
// TestBusConfigMapsFields for the STOMP side. This is the pure-mapping
// unit test that stands in for exercising newSEP2Embed's cert minting
// and listener bind at this layer; the boot-over-mTLS proof lives in
// sep2embed_wiring_test.go.
func TestSEP2EmbedConfigMapsFields(t *testing.T) {
	t.Parallel()

	cfg := config{
		SEP2ServerAddr:    "127.0.0.1:8443",
		SEP2ServerCertDir: "/var/lib/bridge/sep2-certs",
	}

	got := sep2EmbedConfig(cfg, nil)
	if got.Addr != cfg.SEP2ServerAddr {
		t.Errorf("Addr: got %q, want %q", got.Addr, cfg.SEP2ServerAddr)
	}
	if got.CertDir != cfg.SEP2ServerCertDir {
		t.Errorf("CertDir: got %q, want %q", got.CertDir, cfg.SEP2ServerCertDir)
	}
	if got.Bus != nil {
		t.Errorf("Bus: got %v, want nil (no bus passed in)", got.Bus)
	}
	if got.TelemetryDestination != "" {
		t.Errorf("TelemetryDestination: got %q, want empty (no SimulationID set)", got.TelemetryDestination)
	}
	if got.TelemetrySimulationID != "" {
		t.Errorf("TelemetrySimulationID: got %q, want empty (no SimulationID set)", got.TelemetrySimulationID)
	}
}

// TestSEP2EmbedConfigWiresTelemetryWhenSimulationIDSet proves the
// GAGO-034 UP-path wiring: when the bridge is configured with a
// SimulationID (and a non-nil bus is threaded through), sep2EmbedConfig
// derives TelemetryDestination from internal/cim/sim.InputTopic and
// carries the bus and simulation ID straight through, so
// sep2embed.New's telemetry relay is actually enabled end to end.
func TestSEP2EmbedConfigWiresTelemetryWhenSimulationIDSet(t *testing.T) {
	t.Parallel()

	cfg := config{
		SEP2ServerAddr:    "127.0.0.1:8443",
		SEP2ServerCertDir: "/var/lib/bridge/sep2-certs",
		SimulationID:      "sim-123",
	}

	fakeBus := fakeBusPublisherForTest{}
	got := sep2EmbedConfig(cfg, fakeBus)

	wantDest := sim.InputTopic("sim-123")
	if got.TelemetryDestination != wantDest {
		t.Errorf("TelemetryDestination: got %q, want %q", got.TelemetryDestination, wantDest)
	}
	if got.TelemetrySimulationID != "sim-123" {
		t.Errorf("TelemetrySimulationID: got %q, want %q", got.TelemetrySimulationID, "sim-123")
	}
	if got.Bus == nil {
		t.Error("Bus: got nil, want the passed-in bus")
	}
}

// fakeBusPublisherForTest satisfies sep2embed.BusPublisher without
// pulling a real fieldbus.MessageBus into this test.
type fakeBusPublisherForTest struct{}

func (fakeBusPublisherForTest) Send(_ context.Context, _, _ string, _ []byte) error { return nil }
