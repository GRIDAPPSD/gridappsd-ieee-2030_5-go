package telemetrypub

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// fakeSend is one recorded bus publish.
type fakeSend struct {
	dest        string
	contentType string
	body        []byte
}

// fakeBus records every Send call for assertion, and can be made to fail.
type fakeBus struct {
	mu    sync.Mutex
	sends []fakeSend
	err   error
}

func (f *fakeBus) Send(_ context.Context, destination, contentType string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sends = append(f.sends, fakeSend{dest: destination, contentType: contentType, body: append([]byte(nil), body...)})
	return nil
}

func (f *fakeBus) snapshot() []fakeSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeSend, len(f.sends))
	copy(out, f.sends)
	return out
}

func (f *fakeBus) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// fakeSource is a StatusSource whose returned snapshot the test controls.
type fakeSource struct {
	mu    sync.Mutex
	snaps []sep2embed.DERStatusSnapshot
	err   error
	calls int
}

func (f *fakeSource) DERStatusSnapshots(context.Context) ([]sep2embed.DERStatusSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]sep2embed.DERStatusSnapshot, len(f.snaps))
	copy(out, f.snaps)
	return out, nil
}

func (f *fakeSource) set(snaps ...sep2embed.DERStatusSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snaps = snaps
}

func (f *fakeSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// snapWithMode builds a snapshot for mrid whose only mapped field is
// operationalModeStatus, so a test can change exactly one published value.
func snapWithMode(mrid string, mode uint8) sep2embed.DERStatusSnapshot {
	m := sep2.OperationalModeStatusType{Value: mode}
	return sep2embed.DERStatusSnapshot{
		MRID:   mrid,
		EDevID: "edev-" + mrid,
		DERID:  "1",
		Status: sep2.DERStatus{OperationalModeStatus: &m},
	}
}

// diffMessageView decodes a diff.Builder payload back into the fields
// this package's assertions care about, without depending on the diff
// package's internals beyond its public Message/Difference shapes.
type diffMessageView struct {
	Command string `json:"command"`
	Input   struct {
		SimulationID *string `json:"simulation_id,omitempty"`
		Message      struct {
			Timestamp          int64             `json:"timestamp"`
			DifferenceMRID     string            `json:"difference_mrid"`
			ForwardDifferences []diff.Difference `json:"forward_differences"`
			ReverseDifferences []diff.Difference `json:"reverse_differences"`
		} `json:"message"`
	} `json:"input"`
}

func decodeDiffMessage(t *testing.T, body []byte) diffMessageView {
	t.Helper()
	var v diffMessageView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode diff message: %v\nbody=%s", err, body)
	}
	return v
}
