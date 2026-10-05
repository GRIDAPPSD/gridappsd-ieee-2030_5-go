package sender

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

var t0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type sentFrame struct {
	dest, contentType string
	body              []byte
}

type fakeBus struct {
	mu   sync.Mutex
	sent []sentFrame
	err  error
}

func (b *fakeBus) Send(_ context.Context, dest, ct string, body []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	b.sent = append(b.sent, sentFrame{dest, ct, append([]byte(nil), body...)})
	return nil
}

func (b *fakeBus) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sent)
}

type fakeOutcomes struct {
	mu sync.Mutex
	m  map[string]controlobs.MessageOutcome
}

func (f *fakeOutcomes) Outcome(mrid string) (controlobs.MessageOutcome, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.m[mrid]
	return o, ok
}

func (f *fakeOutcomes) put(o controlobs.MessageOutcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]controlobs.MessageOutcome{}
	}
	f.m[o.DifferenceMRID] = o
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

const destTopic = "/topic/goss.gridappsd.IEEE_2030_5.input"

// Four devices, one unnamed.
var devices = []registry.Entry{
	{MRID: "_dev-b", Name: "Bravo", LFDI: strings.Repeat("B", 40)},
	{MRID: "_dev-a", Name: "Alpha", LFDI: strings.Repeat("A", 40)},
	{MRID: "_dev-c", Name: "Charlie", LFDI: strings.Repeat("C", 40)},
	{MRID: "_dev-u", Name: "", LFDI: strings.Repeat("D", 40)},
}

func newRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg := registry.New()
	for _, e := range devices {
		if err := reg.Add(e); err != nil {
			t.Fatalf("registry add %s: %v", e.MRID, err)
		}
	}
	return reg
}

type rig struct {
	s    *Sender
	bus  *fakeBus
	clk  *clock
	obs  *fakeOutcomes
	logs *logSink
	reg  *registry.Registry
}

func newRig(t *testing.T, on bool) *rig {
	t.Helper()
	r := &rig{bus: &fakeBus{}, clk: &clock{t: t0}, obs: &fakeOutcomes{}, logs: &logSink{}, reg: newRegistry(t)}
	s, err := New(Config{
		Bus: r.bus, Registry: r.reg, Destination: destTopic, Outcomes: r.obs,
		PublishAtStart: on, Now: r.clk.now, Logf: r.logs.logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.s = s
	return r
}
