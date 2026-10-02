package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/gridappsdclient"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// defaultHeartbeat is gridappsd-go's own STOMP heartbeat default
// (gridappsd.Config.HeartBeat, zero value). It is restated so the value is
// visible in config and docs; a zero here would mean the same thing.
const defaultHeartbeat = 10 * time.Second

// tuning holds the intervals and timeouts an operator may change. Every
// field is positive after loadConfig; an unset knob carries its compiled-in
// default.
type tuning struct {
	ProbeInterval        time.Duration
	ProbeTimeout         time.Duration
	ReconnectBackoffBase time.Duration
	ReconnectBackoffMax  time.Duration
	UnsubscribeTimeout   time.Duration
	Heartbeat            time.Duration
	ConnectTimeout       time.Duration
	CIMQueryTimeout      time.Duration
	HistoryLogInterval   time.Duration

	ServerReadHeaderTimeout time.Duration
	ServerReadTimeout       time.Duration
	ServerWriteTimeout      time.Duration
	ServerIdleTimeout       time.Duration
	ServerShutdownTimeout   time.Duration

	AdminReadHeaderTimeout time.Duration
	AdminReadTimeout       time.Duration
	AdminWriteTimeout      time.Duration
	AdminIdleTimeout       time.Duration
	AdminShutdownTimeout   time.Duration

	ControlSweepInterval time.Duration

	// NotifyWorkers and NotifyQueueSize are counts, not durations.
	NotifyWorkers   int
	NotifyQueueSize int
}

func defaultTuning() tuning {
	adminReadHeader, adminRead, adminWrite, adminIdle, adminShutdown := adminui.DefaultTimeouts()
	return tuning{
		ProbeInterval:        gridappsdclient.DefaultProbeInterval,
		ProbeTimeout:         gridappsdclient.DefaultProbeTimeout,
		ReconnectBackoffBase: gridappsdclient.DefaultRecoverBackoffBase,
		ReconnectBackoffMax:  gridappsdclient.DefaultRecoverBackoffMax,
		UnsubscribeTimeout:   gridappsdclient.DefaultUnsubscribeTimeout,
		Heartbeat:            defaultHeartbeat,
		ConnectTimeout:       connectTimeout,
		CIMQueryTimeout:      queryTimeout,
		HistoryLogInterval:   historyLogInterval,

		ServerReadHeaderTimeout: sep2srv.DefaultReadHeaderTimeout,
		ServerReadTimeout:       sep2srv.DefaultReadTimeout,
		ServerWriteTimeout:      sep2srv.DefaultWriteTimeout,
		ServerIdleTimeout:       sep2srv.DefaultIdleTimeout,
		ServerShutdownTimeout:   sep2srv.DefaultShutdownTimeout,

		AdminReadHeaderTimeout: adminReadHeader,
		AdminReadTimeout:       adminRead,
		AdminWriteTimeout:      adminWrite,
		AdminIdleTimeout:       adminIdle,
		AdminShutdownTimeout:   adminShutdown,

		ControlSweepInterval: sep2embed.DefaultControlSweepInterval,

		NotifyWorkers:   sep2embed.DefaultNotifyWorkers,
		NotifyQueueSize: sep2embed.DefaultNotifyQueueSize,
	}
}

// tuningKnob is one flag and env var pair. Exactly one of dur and count is
// set; raw receives the flag text, which stays empty when the flag is not
// passed so the env var and then the default can resolve in that order.
type tuningKnob struct {
	flag, env, help string
	dur             *time.Duration
	count           *int
	raw             string
}

func (k *tuningKnob) name() string { return "-" + k.flag + " / " + k.env }

func tuningKnobs(t *tuning) []*tuningKnob {
	d := func(flag, env, help string, p *time.Duration) *tuningKnob {
		return &tuningKnob{flag: flag, env: env, help: help, dur: p}
	}
	n := func(flag, env, help string, p *int) *tuningKnob {
		return &tuningKnob{flag: flag, env: env, help: help, count: p}
	}
	return []*tuningKnob{
		d("stomp-probe-interval", "SEP2_STOMP_PROBE_INTERVAL", "gap between broker liveness probes as a Go duration (default 5s)", &t.ProbeInterval),
		d("stomp-probe-timeout", "SEP2_STOMP_PROBE_TIMEOUT", "time one liveness probe may take before the bus is declared dead (default 10s)", &t.ProbeTimeout),
		d("stomp-reconnect-backoff-base", "SEP2_STOMP_RECONNECT_BACKOFF_BASE", "first delay between broker reconnect attempts, doubling up to the max (default 500ms)", &t.ReconnectBackoffBase),
		d("stomp-reconnect-backoff-max", "SEP2_STOMP_RECONNECT_BACKOFF_MAX", "longest delay between broker reconnect attempts (default 10s)", &t.ReconnectBackoffMax),
		d("stomp-unsubscribe-timeout", "SEP2_STOMP_UNSUBSCRIBE_TIMEOUT", "time to wait for the broker to acknowledge an unsubscribe at shutdown (default 5s)", &t.UnsubscribeTimeout),
		d("stomp-heartbeat", "SEP2_STOMP_HEARTBEAT", "STOMP heartbeat interval offered to the broker (default 10s)", &t.Heartbeat),
		d("stomp-connect-timeout", "SEP2_STOMP_CONNECT_TIMEOUT", "time allowed for the broker dial and token bootstrap (default 15s)", &t.ConnectTimeout),
		d("cim-query-timeout", "SEP2_CIM_QUERY_TIMEOUT", "time allowed for the start-up CIM feeder queries (default 30s)", &t.CIMQueryTimeout),
		d("history-log-interval", "SEP2_HISTORY_LOG_INTERVAL", "minimum gap between two history log lines of the same kind (default 30s)", &t.HistoryLogInterval),

		d("sep2-server-read-header-timeout", "SEP2_SERVER_READ_HEADER_TIMEOUT", "protocol listener request header read timeout (default 10s)", &t.ServerReadHeaderTimeout),
		d("sep2-server-read-timeout", "SEP2_SERVER_READ_TIMEOUT", "protocol listener full request read timeout (default 30s)", &t.ServerReadTimeout),
		d("sep2-server-write-timeout", "SEP2_SERVER_WRITE_TIMEOUT", "protocol listener response write timeout (default 30s)", &t.ServerWriteTimeout),
		d("sep2-server-idle-timeout", "SEP2_SERVER_IDLE_TIMEOUT", "protocol listener keep-alive idle timeout (default 2m)", &t.ServerIdleTimeout),
		d("sep2-server-shutdown-timeout", "SEP2_SERVER_SHUTDOWN_TIMEOUT", "protocol listener graceful drain bound at shutdown (default 5s)", &t.ServerShutdownTimeout),

		d("admin-ui-read-header-timeout", "SEP2_ADMIN_UI_READ_HEADER_TIMEOUT", "admin UI request header read timeout (default 5s)", &t.AdminReadHeaderTimeout),
		d("admin-ui-read-timeout", "SEP2_ADMIN_UI_READ_TIMEOUT", "admin UI full request read timeout (default 10s)", &t.AdminReadTimeout),
		d("admin-ui-write-timeout", "SEP2_ADMIN_UI_WRITE_TIMEOUT", "admin UI response write timeout (default 10s)", &t.AdminWriteTimeout),
		d("admin-ui-idle-timeout", "SEP2_ADMIN_UI_IDLE_TIMEOUT", "admin UI keep-alive idle timeout (default 1m)", &t.AdminIdleTimeout),
		d("admin-ui-shutdown-timeout", "SEP2_ADMIN_UI_SHUTDOWN_TIMEOUT", "admin UI graceful drain bound at shutdown (default 5s)", &t.AdminShutdownTimeout),

		d("sep2-control-sweep-interval", "SEP2_CONTROL_SWEEP_INTERVAL", "how often ended DERControls are expired fleet-wide (default 10s)", &t.ControlSweepInterval),

		n("sep2-notify-workers", "SEP2_NOTIFY_WORKERS", "subscription notification worker count (default 4)", &t.NotifyWorkers),
		n("sep2-notify-queue-size", "SEP2_NOTIFY_QUEUE_SIZE", "subscription notification queue length (default 100)", &t.NotifyQueueSize),
	}
}

// registerTuningFlags registers every knob with an empty-string default,
// the "operator said nothing" sentinel, so no registered default masks the
// env var and the compiled-in default behind it.
func registerTuningFlags(fs *flag.FlagSet, knobs []*tuningKnob) {
	for _, k := range knobs {
		fs.StringVar(&k.raw, k.flag, "", k.help)
	}
}

// resolveTuning applies flag, then env, then the default already held in
// each field, and refuses a value that is not positive. The error names the
// flag and the variable.
func resolveTuning(knobs []*tuningKnob) error {
	for _, k := range knobs {
		raw := k.raw
		if raw == "" {
			raw = os.Getenv(k.env)
		}
		if raw == "" {
			continue
		}
		if k.dur != nil {
			v, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("config: %s value %q must be a Go duration such as \"15s\" or \"1m\"", k.name(), raw)
			}
			if v <= 0 {
				return fmt.Errorf("config: %s value %q must be greater than zero", k.name(), raw)
			}
			*k.dur = v
			continue
		}
		v, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("config: %s value %q must be a positive integer", k.name(), raw)
		}
		if v <= 0 {
			return fmt.Errorf("config: %s value %q must be greater than zero", k.name(), raw)
		}
		*k.count = v
	}
	return nil
}

// validate refuses a combination no single knob rules out.
func (t tuning) validate() error {
	if t.ReconnectBackoffMax < t.ReconnectBackoffBase {
		return fmt.Errorf("config: -stomp-reconnect-backoff-max / SEP2_STOMP_RECONNECT_BACKOFF_MAX (%s) must not be less than -stomp-reconnect-backoff-base / SEP2_STOMP_RECONNECT_BACKOFF_BASE (%s)",
			t.ReconnectBackoffMax, t.ReconnectBackoffBase)
	}
	return nil
}
