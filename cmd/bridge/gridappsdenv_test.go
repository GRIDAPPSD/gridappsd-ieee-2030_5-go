package main

import (
	"os"
	"strings"
	"testing"
)

// clearBrokerEnv empties every variable that feeds the broker settings so
// a test sees only what it sets.
func clearBrokerEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"SEP2_STOMP_ADDR", "SEP2_STOMP_USER", "SEP2_STOMP_PASSWORD",
		"GRIDAPPSD_ADDRESS", "GRIDAPPSD_PORT", "GRIDAPPSD_USER", "GRIDAPPSD_PASSWORD",
	} {
		t.Setenv(k, "")
	}
}

func TestGridappsdBrokerAddress(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"address alone keeps the default port", map[string]string{"GRIDAPPSD_ADDRESS": "broker.example"}, nil, "broker.example:61613"},
		{"port alone keeps the default host", map[string]string{"GRIDAPPSD_PORT": "7777"}, nil, "127.0.0.1:7777"},
		{"address and port combine", map[string]string{"GRIDAPPSD_ADDRESS": "broker.example", "GRIDAPPSD_PORT": "7777"}, nil, "broker.example:7777"},
		{"ipv6 address is bracketed", map[string]string{"GRIDAPPSD_ADDRESS": "::1", "GRIDAPPSD_PORT": "7777"}, nil, "[::1]:7777"},
		{"SEP2 address wins whole, GRIDAPPSD port ignored", map[string]string{"SEP2_STOMP_ADDR": "sep2.example:1111", "GRIDAPPSD_ADDRESS": "g.example", "GRIDAPPSD_PORT": "7777"}, nil, "sep2.example:1111"},
		{"flag beats both", map[string]string{"SEP2_STOMP_ADDR": "sep2.example:1111", "GRIDAPPSD_ADDRESS": "g.example", "GRIDAPPSD_PORT": "7777"}, []string{"-stomp-addr=flag.example:2222"}, "flag.example:2222"},
		{"flag beats GRIDAPPSD alone", map[string]string{"GRIDAPPSD_ADDRESS": "g.example"}, []string{"-stomp-addr=flag.example:2222"}, "flag.example:2222"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearBrokerEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg, err := loadConfig(tc.args)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.STOMPAddr != tc.want {
				t.Errorf("STOMPAddr: got %q want %q", cfg.STOMPAddr, tc.want)
			}
		})
	}
}

func TestGridappsdBadPortNamesVariableNotPassword(t *testing.T) {
	for _, bad := range []string{"notaport", "0", "65536", "-1"} {
		t.Run(bad, func(t *testing.T) {
			clearBrokerEnv(t)
			t.Setenv("GRIDAPPSD_PORT", bad)
			t.Setenv("GRIDAPPSD_PASSWORD", "topsecret")
			_, err := loadConfig(nil)
			if err == nil {
				t.Fatal("expected an error for a bad GRIDAPPSD_PORT")
			}
			if !strings.Contains(err.Error(), "GRIDAPPSD_PORT") {
				t.Errorf("error should name the variable: %v", err)
			}
			if strings.Contains(err.Error(), "topsecret") {
				t.Errorf("error leaked the password: %v", err)
			}
		})
	}
}

func TestGridappsdCredentials(t *testing.T) {
	cases := []struct {
		name     string
		env      map[string]string
		args     []string
		wantUser string
		wantPass string
	}{
		{"GRIDAPPSD names alone", map[string]string{"GRIDAPPSD_USER": "gu", "GRIDAPPSD_PASSWORD": "gp"}, nil, "gu", "gp"},
		{"SEP2 names beat GRIDAPPSD", map[string]string{"SEP2_STOMP_USER": "su", "SEP2_STOMP_PASSWORD": "sp", "GRIDAPPSD_USER": "gu", "GRIDAPPSD_PASSWORD": "gp"}, nil, "su", "sp"},
		{"flags beat both", map[string]string{"SEP2_STOMP_USER": "su", "SEP2_STOMP_PASSWORD": "sp", "GRIDAPPSD_USER": "gu", "GRIDAPPSD_PASSWORD": "gp"}, []string{"-stomp-user=fu", "-stomp-password=fp"}, "fu", "fp"},
		{"neither set takes the built-in defaults", nil, nil, defaultSTOMPUser, defaultSTOMPPassword},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearBrokerEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg, err := loadConfig(tc.args)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.STOMPUser != tc.wantUser {
				t.Errorf("STOMPUser: got %q want %q", cfg.STOMPUser, tc.wantUser)
			}
			if cfg.STOMPPassword != tc.wantPass {
				t.Errorf("STOMPPassword: got %q want %q", cfg.STOMPPassword, tc.wantPass)
			}
		})
	}
}

// Both names of each credential are scrubbed, whichever one won, so a
// leftover export reaches no child process.
func TestGridappsdCredentialEnvScrubbed(t *testing.T) {
	clearBrokerEnv(t)
	for _, k := range []string{"SEP2_STOMP_USER", "SEP2_STOMP_PASSWORD", "GRIDAPPSD_USER", "GRIDAPPSD_PASSWORD"} {
		t.Setenv(k, "value-of-"+k)
	}
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.STOMPPassword != "value-of-SEP2_STOMP_PASSWORD" {
		t.Errorf("STOMPPassword: got %q", cfg.STOMPPassword)
	}
	for _, k := range []string{"SEP2_STOMP_USER", "SEP2_STOMP_PASSWORD", "GRIDAPPSD_USER", "GRIDAPPSD_PASSWORD"} {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			t.Errorf("%s still set after loadConfig", k)
		}
	}
}

func TestGridappsdPasswordScrubbedWhenFlagWins(t *testing.T) {
	clearBrokerEnv(t)
	t.Setenv("GRIDAPPSD_PASSWORD", "gp")
	if _, err := loadConfig([]string{"-stomp-password=fp"}); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if v := os.Getenv("GRIDAPPSD_PASSWORD"); v != "" {
		t.Errorf("GRIDAPPSD_PASSWORD still set after the flag won: %q", v)
	}
}

func TestResolveCredOrder(t *testing.T) {
	t.Setenv("A_FIRST", "")
	t.Setenv("B_SECOND", "second")
	if got := resolveCred("", "fb", "A_FIRST", "B_SECOND"); got != "second" {
		t.Errorf("empty first env should fall through to the second: got %q", got)
	}
	if got := resolveCred("", "fb", "NOPE_1", "NOPE_2"); got != "fb" {
		t.Errorf("nothing set should give the fallback: got %q", got)
	}
}
