package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/gridappsdclient"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2config"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// tuningCase is one knob: its names, its default, a valid value with the
// exact result it must produce, and how to read it back from the config.
type tuningCase struct {
	flag, env string
	def       any
	valid     string
	want      any
	get       func(config) any
	count     bool
}

func dur(f func(tuning) time.Duration) func(config) any {
	return func(c config) any { return f(c.Tuning) }
}

func tuningCases() []tuningCase {
	s := time.Second
	return []tuningCase{
		{"stomp-probe-interval", "SEP2_STOMP_PROBE_INTERVAL", 5 * s, "7s", 7 * s, dur(func(t tuning) time.Duration { return t.ProbeInterval }), false},
		{"stomp-probe-timeout", "SEP2_STOMP_PROBE_TIMEOUT", 10 * s, "8s", 8 * s, dur(func(t tuning) time.Duration { return t.ProbeTimeout }), false},
		{"stomp-reconnect-backoff-base", "SEP2_STOMP_RECONNECT_BACKOFF_BASE", 500 * time.Millisecond, "250ms", 250 * time.Millisecond, dur(func(t tuning) time.Duration { return t.ReconnectBackoffBase }), false},
		{"stomp-reconnect-backoff-max", "SEP2_STOMP_RECONNECT_BACKOFF_MAX", 10 * s, "20s", 20 * s, dur(func(t tuning) time.Duration { return t.ReconnectBackoffMax }), false},
		{"stomp-unsubscribe-timeout", "SEP2_STOMP_UNSUBSCRIBE_TIMEOUT", 5 * s, "6s", 6 * s, dur(func(t tuning) time.Duration { return t.UnsubscribeTimeout }), false},
		{"stomp-heartbeat", "SEP2_STOMP_HEARTBEAT", 10 * s, "30s", 30 * s, dur(func(t tuning) time.Duration { return t.Heartbeat }), false},
		{"stomp-connect-timeout", "SEP2_STOMP_CONNECT_TIMEOUT", 15 * s, "45s", 45 * s, dur(func(t tuning) time.Duration { return t.ConnectTimeout }), false},
		{"cim-query-timeout", "SEP2_CIM_QUERY_TIMEOUT", 30 * s, "2m", 2 * time.Minute, dur(func(t tuning) time.Duration { return t.CIMQueryTimeout }), false},
		{"history-log-interval", "SEP2_HISTORY_LOG_INTERVAL", 30 * s, "1m", time.Minute, dur(func(t tuning) time.Duration { return t.HistoryLogInterval }), false},
		{"sep2-server-read-header-timeout", "SEP2_SERVER_READ_HEADER_TIMEOUT", 10 * s, "11s", 11 * s, dur(func(t tuning) time.Duration { return t.ServerReadHeaderTimeout }), false},
		{"sep2-server-read-timeout", "SEP2_SERVER_READ_TIMEOUT", 30 * s, "31s", 31 * s, dur(func(t tuning) time.Duration { return t.ServerReadTimeout }), false},
		{"sep2-server-write-timeout", "SEP2_SERVER_WRITE_TIMEOUT", 30 * s, "32s", 32 * s, dur(func(t tuning) time.Duration { return t.ServerWriteTimeout }), false},
		{"sep2-server-idle-timeout", "SEP2_SERVER_IDLE_TIMEOUT", 120 * s, "3m", 3 * time.Minute, dur(func(t tuning) time.Duration { return t.ServerIdleTimeout }), false},
		{"sep2-server-shutdown-timeout", "SEP2_SERVER_SHUTDOWN_TIMEOUT", 5 * s, "9s", 9 * s, dur(func(t tuning) time.Duration { return t.ServerShutdownTimeout }), false},
		{"admin-ui-read-header-timeout", "SEP2_ADMIN_UI_READ_HEADER_TIMEOUT", 5 * s, "6s", 6 * s, dur(func(t tuning) time.Duration { return t.AdminReadHeaderTimeout }), false},
		{"admin-ui-read-timeout", "SEP2_ADMIN_UI_READ_TIMEOUT", 10 * s, "12s", 12 * s, dur(func(t tuning) time.Duration { return t.AdminReadTimeout }), false},
		{"admin-ui-write-timeout", "SEP2_ADMIN_UI_WRITE_TIMEOUT", 10 * s, "13s", 13 * s, dur(func(t tuning) time.Duration { return t.AdminWriteTimeout }), false},
		{"admin-ui-idle-timeout", "SEP2_ADMIN_UI_IDLE_TIMEOUT", time.Minute, "90s", 90 * s, dur(func(t tuning) time.Duration { return t.AdminIdleTimeout }), false},
		{"admin-ui-shutdown-timeout", "SEP2_ADMIN_UI_SHUTDOWN_TIMEOUT", 5 * s, "15s", 15 * s, dur(func(t tuning) time.Duration { return t.AdminShutdownTimeout }), false},
		{"sep2-control-sweep-interval", "SEP2_CONTROL_SWEEP_INTERVAL", 10 * s, "2s", 2 * s, dur(func(t tuning) time.Duration { return t.ControlSweepInterval }), false},
		{"sep2-notify-workers", "SEP2_NOTIFY_WORKERS", 4, "16", 16, func(c config) any { return c.Tuning.NotifyWorkers }, true},
		{"sep2-notify-queue-size", "SEP2_NOTIFY_QUEUE_SIZE", 100, "500", 500, func(c config) any { return c.Tuning.NotifyQueueSize }, true},
	}
}

func clearTuningEnv(t *testing.T) {
	t.Helper()
	for _, tc := range tuningCases() {
		t.Setenv(tc.env, "")
	}
}

func TestTuningDefaultsAreUnchanged(t *testing.T) {
	clearTuningEnv(t)
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	for _, tc := range tuningCases() {
		if got := tc.get(cfg); got != tc.def {
			t.Errorf("%s default = %v, want %v", tc.flag, got, tc.def)
		}
	}
}

func TestTuningFlagAndEnvLandInConfig(t *testing.T) {
	for _, tc := range tuningCases() {
		t.Run(tc.flag+" flag", func(t *testing.T) {
			clearTuningEnv(t)
			cfg, err := loadConfig([]string{"-" + tc.flag + "=" + tc.valid})
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if got := tc.get(cfg); got != tc.want {
				t.Errorf("%s = %v, want %v", tc.flag, got, tc.want)
			}
		})
		t.Run(tc.env+" env", func(t *testing.T) {
			clearTuningEnv(t)
			t.Setenv(tc.env, tc.valid)
			cfg, err := loadConfig(nil)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if got := tc.get(cfg); got != tc.want {
				t.Errorf("%s = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

func TestTuningFlagShadowsEnv(t *testing.T) {
	for _, tc := range tuningCases() {
		clearTuningEnv(t)
		t.Setenv(tc.env, "garbage")
		cfg, err := loadConfig([]string{"-" + tc.flag + "=" + tc.valid})
		if err != nil {
			t.Fatalf("%s: loadConfig: %v", tc.flag, err)
		}
		if got := tc.get(cfg); got != tc.want {
			t.Errorf("%s = %v, want the flag value %v", tc.flag, got, tc.want)
		}
	}
}

func TestTuningRefusesUnusableValues(t *testing.T) {
	for _, tc := range tuningCases() {
		bad := []string{"0", "-1", "garbage"}
		if !tc.count {
			bad = []string{"0s", "-1s", "garbage", "5"}
		}
		for _, v := range bad {
			t.Run(tc.flag+" flag "+v, func(t *testing.T) {
				clearTuningEnv(t)
				_, err := loadConfig([]string{"-" + tc.flag + "=" + v})
				if err == nil {
					t.Fatalf("loadConfig accepted %s=%q", tc.flag, v)
				}
				if !strings.Contains(err.Error(), tc.env) || !strings.Contains(err.Error(), "-"+tc.flag) {
					t.Errorf("error %q does not name %s and -%s", err, tc.env, tc.flag)
				}
			})
			t.Run(tc.env+" env "+v, func(t *testing.T) {
				clearTuningEnv(t)
				t.Setenv(tc.env, v)
				_, err := loadConfig(nil)
				if err == nil {
					t.Fatalf("loadConfig accepted %s=%q", tc.env, v)
				}
				if !strings.Contains(err.Error(), tc.env) {
					t.Errorf("error %q does not name %s", err, tc.env)
				}
			})
		}
	}
}

func TestTuningRefusesBackoffMaxBelowBase(t *testing.T) {
	clearTuningEnv(t)
	_, err := loadConfig([]string{"-stomp-reconnect-backoff-base=5s", "-stomp-reconnect-backoff-max=1s"})
	if err == nil || !strings.Contains(err.Error(), "SEP2_STOMP_RECONNECT_BACKOFF_MAX") || !strings.Contains(err.Error(), "SEP2_STOMP_RECONNECT_BACKOFF_BASE") {
		t.Fatalf("err = %v, want a refusal naming both backoff variables", err)
	}
}

func testTuningConfig() config {
	return config{ApplicationID: "IEEE_2030_5", SimulationID: "sim-1", Tuning: tuning{
		ProbeInterval:        21 * time.Second,
		ProbeTimeout:         22 * time.Second,
		ReconnectBackoffBase: 23 * time.Millisecond,
		ReconnectBackoffMax:  24 * time.Second,
		UnsubscribeTimeout:   25 * time.Second,
		Heartbeat:            26 * time.Second,
		ConnectTimeout:       27 * time.Second,
		CIMQueryTimeout:      28 * time.Second,
		HistoryLogInterval:   29 * time.Second,

		ServerReadHeaderTimeout: 31 * time.Second,
		ServerReadTimeout:       32 * time.Second,
		ServerWriteTimeout:      33 * time.Second,
		ServerIdleTimeout:       34 * time.Second,
		ServerShutdownTimeout:   35 * time.Second,

		AdminReadHeaderTimeout: 41 * time.Second,
		AdminReadTimeout:       42 * time.Second,
		AdminWriteTimeout:      43 * time.Second,
		AdminIdleTimeout:       44 * time.Second,
		AdminShutdownTimeout:   45 * time.Second,

		ControlSweepInterval: 51 * time.Second,
		NotifyWorkers:        52,
		NotifyQueueSize:      53,
	}}
}

func TestTuningReachesSupervisor(t *testing.T) {
	cfg := testTuningConfig()
	sup := gridappsdclient.NewSupervisor(newRouterBus(), supervisorOptions(cfg)...)
	want := gridappsdclient.SupervisorSettings{
		ProbeDestination:   probeDestination(cfg),
		ProbeInterval:      21 * time.Second,
		ProbeTimeout:       22 * time.Second,
		BackoffBase:        23 * time.Millisecond,
		BackoffMax:         24 * time.Second,
		UnsubscribeTimeout: 25 * time.Second,
	}
	if got := sup.Settings(); got != want {
		t.Errorf("supervisor settings = %+v, want %+v", got, want)
	}
}

func TestTuningReachesBusConfig(t *testing.T) {
	if got := busConfig(testTuningConfig()).HeartBeat; got != 26*time.Second {
		t.Errorf("busConfig HeartBeat = %s, want 26s", got)
	}
}

func TestTuningReachesConnectAndQueryDeadlines(t *testing.T) {
	cfg := testTuningConfig()
	before := time.Now()
	ctx, cancel := cimQueryContext(context.Background(), cfg)
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("CIM query context has no deadline")
	}
	if got := dl.Sub(before); got < 28*time.Second || got > 29*time.Second {
		t.Errorf("CIM query deadline is %s out, want about 28s", got)
	}

	before = time.Now()
	cctx, ccancel := connectContext(context.Background(), cfg)
	defer ccancel()
	cdl, ok := cctx.Deadline()
	if !ok {
		t.Fatal("connect context has no deadline")
	}
	if got := cdl.Sub(before); got < 27*time.Second || got > 28*time.Second {
		t.Errorf("connect deadline is %s out, want about 27s", got)
	}
}

func TestTuningReachesHistorySink(t *testing.T) {
	cfg := testTuningConfig()
	cfg.Tuning.HistoryLogInterval = 10 * time.Second
	now := time.Unix(1000, 0)
	var lines []string
	logf := historyLogf(cfg, func() time.Time { return now }, func(f string, a ...any) { lines = append(lines, f) })

	logf("same kind")
	now = now.Add(9 * time.Second)
	logf("same kind")
	if len(lines) != 1 {
		t.Fatalf("lines after 9s = %d, want 1 (inside the 10s gap)", len(lines))
	}
	now = now.Add(2 * time.Second)
	logf("same kind")
	if len(lines) != 2 {
		t.Errorf("lines after 11s = %d, want 2 (past the 10s gap)", len(lines))
	}
	if newHistorySink(cfg, nil) == nil {
		t.Error("newHistorySink returned nil")
	}
}

func TestTuningReachesEmbedAndAdminConfig(t *testing.T) {
	cfg := testTuningConfig()
	ec := sep2EmbedConfig(cfg, sep2config.SEP2Policy{}, nil, sep2embed.DeviceCertModeDevMint)
	if ec.ReadHeaderTimeout != 31*time.Second || ec.ReadTimeout != 32*time.Second || ec.WriteTimeout != 33*time.Second ||
		ec.IdleTimeout != 34*time.Second || ec.ShutdownTimeout != 35*time.Second {
		t.Errorf("embed listener timeouts = %v/%v/%v/%v/%v, want 31s..35s", ec.ReadHeaderTimeout, ec.ReadTimeout, ec.WriteTimeout, ec.IdleTimeout, ec.ShutdownTimeout)
	}
	if ec.ControlSweepInterval != 51*time.Second {
		t.Errorf("embed ControlSweepInterval = %s, want 51s", ec.ControlSweepInterval)
	}
	if ec.NotifyWorkers != 52 || ec.NotifyQueueSize != 53 {
		t.Errorf("embed notify sizing = %d/%d, want 52/53", ec.NotifyWorkers, ec.NotifyQueueSize)
	}

	ac := adminUIConfig(cfg)
	if ac.ReadHeaderTimeout != 41*time.Second || ac.ReadTimeout != 42*time.Second || ac.WriteTimeout != 43*time.Second ||
		ac.IdleTimeout != 44*time.Second || ac.ShutdownTimeout != 45*time.Second {
		t.Errorf("admin timeouts = %v/%v/%v/%v/%v, want 41s..45s", ac.ReadHeaderTimeout, ac.ReadTimeout, ac.WriteTimeout, ac.IdleTimeout, ac.ShutdownTimeout)
	}
}

// tuningRanges is the allowed range of each knob, as the operator would
// write it. A value at either end is accepted; one step outside is refused.
var tuningRanges = []struct {
	flag, env, lo, hi, belowLo, aboveHi string
}{
	{"stomp-probe-interval", "SEP2_STOMP_PROBE_INTERVAL", "1s", "1h", "999ms", "3601s"},
	{"stomp-probe-timeout", "SEP2_STOMP_PROBE_TIMEOUT", "1s", "5m", "1ms", "301s"},
	{"stomp-reconnect-backoff-base", "SEP2_STOMP_RECONNECT_BACKOFF_BASE", "100ms", "1m", "99ms", "61s"},
	{"stomp-reconnect-backoff-max", "SEP2_STOMP_RECONNECT_BACKOFF_MAX", "1s", "10m", "999ms", "601s"},
	{"stomp-unsubscribe-timeout", "SEP2_STOMP_UNSUBSCRIBE_TIMEOUT", "1s", "5m", "999ms", "301s"},
	{"stomp-heartbeat", "SEP2_STOMP_HEARTBEAT", "1s", "5m", "999us", "1h"},
	{"stomp-connect-timeout", "SEP2_STOMP_CONNECT_TIMEOUT", "1s", "5m", "999ms", "301s"},
	{"cim-query-timeout", "SEP2_CIM_QUERY_TIMEOUT", "1s", "10m", "999ms", "601s"},
	{"history-log-interval", "SEP2_HISTORY_LOG_INTERVAL", "1s", "1h", "999ms", "3601s"},
	{"sep2-server-read-header-timeout", "SEP2_SERVER_READ_HEADER_TIMEOUT", "1s", "1h", "999ms", "3601s"},
	{"sep2-server-read-timeout", "SEP2_SERVER_READ_TIMEOUT", "1s", "1h", "999ms", "3601s"},
	{"sep2-server-write-timeout", "SEP2_SERVER_WRITE_TIMEOUT", "1s", "1h", "999ms", "3601s"},
	{"sep2-server-idle-timeout", "SEP2_SERVER_IDLE_TIMEOUT", "1s", "1h", "999ms", "3601s"},
	{"sep2-server-shutdown-timeout", "SEP2_SERVER_SHUTDOWN_TIMEOUT", "1s", "5m", "999ms", "301s"},
	{"admin-ui-read-header-timeout", "SEP2_ADMIN_UI_READ_HEADER_TIMEOUT", "1s", "1h", "999ms", "3601s"},
	{"admin-ui-read-timeout", "SEP2_ADMIN_UI_READ_TIMEOUT", "1s", "1h", "999ms", "3601s"},
	{"admin-ui-write-timeout", "SEP2_ADMIN_UI_WRITE_TIMEOUT", "1s", "1h", "999ms", "3601s"},
	{"admin-ui-idle-timeout", "SEP2_ADMIN_UI_IDLE_TIMEOUT", "1s", "1h", "999ms", "3601s"},
	{"admin-ui-shutdown-timeout", "SEP2_ADMIN_UI_SHUTDOWN_TIMEOUT", "1s", "5m", "999ms", "301s"},
	{"sep2-control-sweep-interval", "SEP2_CONTROL_SWEEP_INTERVAL", "1s", "1h", "999ms", "3601s"},
	{"sep2-notify-workers", "SEP2_NOTIFY_WORKERS", "1", "1024", "0", "1025"},
	{"sep2-notify-queue-size", "SEP2_NOTIFY_QUEUE_SIZE", "1", "100000", "0", "100001"},
}

func TestTuningRangesAreEnforcedAtBothEnds(t *testing.T) {
	cases := map[string]tuningCase{}
	for _, tc := range tuningCases() {
		cases[tc.flag] = tc
	}
	if len(tuningRanges) != len(cases) {
		t.Fatalf("range table has %d knobs, tuning table has %d", len(tuningRanges), len(cases))
	}
	for _, r := range tuningRanges {
		t.Run(r.flag, func(t *testing.T) {
			// Pair a backoff knob with a partner that keeps base <= max.
			extra := []string{}
			if r.flag == "stomp-reconnect-backoff-max" {
				extra = []string{"-stomp-reconnect-backoff-base=100ms"}
			}
			if r.flag == "stomp-reconnect-backoff-base" {
				extra = []string{"-stomp-reconnect-backoff-max=10m"}
			}
			for _, ok := range []string{r.lo, r.hi} {
				clearTuningEnv(t)
				if _, err := loadConfig(append([]string{"-" + r.flag + "=" + ok}, extra...)); err != nil {
					t.Errorf("%s=%s refused: %v", r.flag, ok, err)
				}
			}
			for _, bad := range []string{r.belowLo, r.aboveHi} {
				clearTuningEnv(t)
				_, err := loadConfig(append([]string{"-" + r.flag + "=" + bad}, extra...))
				if err == nil {
					t.Errorf("%s=%s accepted, want a range refusal", r.flag, bad)
					continue
				}
				for _, want := range []string{"-" + r.flag, r.env, "between " + r.lo + " and " + r.hi} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%s=%s: error %q does not contain %q", r.flag, bad, err, want)
					}
				}
			}
		})
	}
}

func TestTuningDefaultsLieInsideTheirRanges(t *testing.T) {
	clearTuningEnv(t)
	if _, err := loadConfig(nil); err != nil {
		t.Fatalf("defaults refused: %v", err)
	}
	for _, k := range tuningKnobs(&tuning{}) {
		if k.dur != nil && (k.minDur <= 0 || k.maxDur < k.minDur) {
			t.Errorf("%s has no usable duration range", k.flag)
		}
		if k.count != nil && (k.minCount <= 0 || k.maxCount < k.minCount) {
			t.Errorf("%s has no usable count range", k.flag)
		}
	}
}

func TestTuningHeartbeatEndsAreEnforced(t *testing.T) {
	clearTuningEnv(t)
	for _, v := range []string{"500us", "1h"} {
		if _, err := loadConfig([]string{"-stomp-heartbeat=" + v}); err == nil {
			t.Errorf("-stomp-heartbeat=%s accepted", v)
		}
	}
}

func TestTuningNotifyQueueCeilingStopsAMakechanPanic(t *testing.T) {
	clearTuningEnv(t)
	_, err := loadConfig([]string{"-sep2-notify-queue-size=9223372036854775807"})
	if err == nil || !strings.Contains(err.Error(), "SEP2_NOTIFY_QUEUE_SIZE") {
		t.Fatalf("max-int queue size: err = %v, want a refusal naming SEP2_NOTIFY_QUEUE_SIZE", err)
	}
}
