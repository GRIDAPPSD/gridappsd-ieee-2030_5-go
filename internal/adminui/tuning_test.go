package adminui

import (
	"testing"
	"time"
)

func TestTimeoutsOverrides(t *testing.T) {
	t.Parallel()

	got := defaultTimeouts.withOverrides(Config{
		ReadHeaderTimeout: 1 * time.Second,
		ReadTimeout:       2 * time.Second,
		WriteTimeout:      3 * time.Second,
		IdleTimeout:       4 * time.Second,
		ShutdownTimeout:   6 * time.Second,
	})
	want := timeouts{readHeader: time.Second, read: 2 * time.Second, write: 3 * time.Second, idle: 4 * time.Second, shutdown: 6 * time.Second}
	if got != want {
		t.Errorf("overridden timeouts = %+v, want %+v", got, want)
	}

	if got := defaultTimeouts.withOverrides(Config{}); got != defaultTimeouts {
		t.Errorf("zero Config changed the defaults: %+v, want %+v", got, defaultTimeouts)
	}
	want = timeouts{readHeader: 5 * time.Second, read: 10 * time.Second, write: 10 * time.Second, idle: time.Minute, shutdown: 5 * time.Second}
	if defaultTimeouts != want {
		t.Errorf("defaultTimeouts = %+v, want %+v", defaultTimeouts, want)
	}
}

func TestNewAppliesConfiguredTimeouts(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{
		Key:               testKey,
		ReadHeaderTimeout: 11 * time.Second,
		ReadTimeout:       12 * time.Second,
		WriteTimeout:      13 * time.Second,
		IdleTimeout:       14 * time.Second,
		ShutdownTimeout:   15 * time.Second,
	}, testSources())
	want := timeouts{readHeader: 11 * time.Second, read: 12 * time.Second, write: 13 * time.Second, idle: 14 * time.Second, shutdown: 15 * time.Second}
	if s.timeouts != want {
		t.Errorf("server timeouts = %+v, want %+v", s.timeouts, want)
	}

	d := newServer(t, Config{Key: testKey}, testSources())
	if d.timeouts != defaultTimeouts {
		t.Errorf("unset timeouts = %+v, want the defaults %+v", d.timeouts, defaultTimeouts)
	}
}
