package main

import (
	"crypto/tls"
	"testing"
)

// TestMonitorSTOMPConfigMatchesTheBusTransport: the bus monitor's
// connections use the bridge's broker and credential, and dial TLS
// verified against the broker's host unless plaintext was opted in.
func TestMonitorSTOMPConfigMatchesTheBusTransport(t *testing.T) {
	t.Parallel()

	tlsCfg := monitorSTOMPConfig(config{STOMPAddr: "broker.example:61614", STOMPUser: "u", STOMPPassword: "p"})
	if tlsCfg.Address != "broker.example:61614" || tlsCfg.User != "u" || tlsCfg.Password != "p" {
		t.Errorf("TLS config identity = %q %q %q", tlsCfg.Address, tlsCfg.User, tlsCfg.Password)
	}
	if tlsCfg.TLS == nil {
		t.Fatal("TLS = nil for a config that did not opt into plaintext")
	}
	if tlsCfg.TLS.ServerName != "broker.example" || tlsCfg.TLS.MinVersion != tls.VersionTLS12 || tlsCfg.TLS.InsecureSkipVerify {
		t.Errorf("TLS = server name %q, min version %x, skip verify %v", tlsCfg.TLS.ServerName, tlsCfg.TLS.MinVersion, tlsCfg.TLS.InsecureSkipVerify)
	}

	plain := monitorSTOMPConfig(config{STOMPAddr: "127.0.0.1:61613", STOMPUser: "system", STOMPPassword: "manager", AllowPlaintext: true})
	if plain.TLS != nil || plain.Address != "127.0.0.1:61613" || plain.User != "system" || plain.Password != "manager" {
		t.Errorf("plaintext config = %+v", plain)
	}
}

// TestMonitorRedactionsCoverTheCredentialInEveryForm: the blob comes first,
// so replacing the user or password cannot break it up before it is found.
func TestMonitorRedactionsCoverTheCredentialInEveryForm(t *testing.T) {
	t.Parallel()

	got := monitorRedactions(config{STOMPUser: "opsuser", STOMPPassword: "s3cret"})
	want := []string{gossAuthBlob(config{STOMPUser: "opsuser", STOMPPassword: "s3cret"}), "s3cret", "opsuser"}
	if len(got) != len(want) {
		t.Fatalf("redactions = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("redactions = %q, want %q", got, want)
		}
	}
	if got := monitorRedactions(config{}); len(got) != 0 {
		t.Errorf("empty credential: %q, want none (an empty string would match everywhere)", got)
	}
}
