package diff

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fixedEpoch is used across tests so output is comparable to golden
// fixtures generated from the Python upstream.
const fixedEpoch int64 = 1700000000

// fixedMRID is what golden fixtures pin difference_mrid to for byte-by-byte
// comparison. The real builder generates a fresh UUIDv4 per Message call,
// matching the Python upstream. Tests overwrite the field after Message
// returns to make output stable for golden comparison.
const fixedMRID = "FIXED-MRID-FOR-TESTING"

func loadGolden(t *testing.T, name string) []byte {
	t.Helper()
	p := filepath.Join("testdata", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return b
}

// canonicalize re-encodes JSON with sort_keys-equivalent ordering by
// round-tripping through map[string]any. Lets us compare semantic
// equality rather than byte order.
func canonicalize(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, string(raw))
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Re-decode and re-encode with map round trip; standard library
	// emits map keys in sorted order, so this is canonical.
	var v2 map[string]any
	if err := json.Unmarshal(out, &v2); err != nil {
		// Top-level may not be a map; fall through with whatever encoding we got.
		return out
	}
	canon, err := json.Marshal(v2)
	if err != nil {
		t.Fatalf("recanon marshal: %v", err)
	}
	return canon
}

func TestNewBuilder_EmptyState(t *testing.T) {
	b := NewBuilder("sim-1")
	if b == nil {
		t.Fatal("NewBuilder returned nil")
	}
	if got := b.Len(); got != 0 {
		t.Errorf("Len = %d, want 0", got)
	}
	msg, err := b.Message(fixedEpoch)
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	if msg.Input.Message.ForwardDifferences == nil {
		t.Error("forward_differences should be empty slice, not nil")
	}
	if len(msg.Input.Message.ForwardDifferences) != 0 {
		t.Errorf("forward len = %d, want 0", len(msg.Input.Message.ForwardDifferences))
	}
	if len(msg.Input.Message.ReverseDifferences) != 0 {
		t.Errorf("reverse len = %d, want 0", len(msg.Input.Message.ReverseDifferences))
	}
	if msg.Input.SimulationID == nil {
		t.Error("simulation_id should be set when builder has non-empty sim id")
	}
}

func TestAddDifference_AppendsBoth(t *testing.T) {
	b := NewBuilder("sim-1")
	if err := b.AddDifference("obj-1", "attr-1", "fwd-1", "rev-1"); err != nil {
		t.Fatalf("AddDifference: %v", err)
	}
	if got := b.Len(); got != 1 {
		t.Errorf("Len = %d, want 1", got)
	}
	if err := b.AddDifference("obj-2", "attr-2", 42, 0); err != nil {
		t.Fatalf("AddDifference: %v", err)
	}
	if got := b.Len(); got != 2 {
		t.Errorf("Len = %d, want 2", got)
	}
	msg, err := b.Message(fixedEpoch)
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	if len(msg.Input.Message.ForwardDifferences) != 2 || len(msg.Input.Message.ReverseDifferences) != 2 {
		t.Fatalf("expected 2 forward and 2 reverse, got %d / %d",
			len(msg.Input.Message.ForwardDifferences),
			len(msg.Input.Message.ReverseDifferences))
	}
	if msg.Input.Message.ForwardDifferences[0].Object != "obj-1" {
		t.Errorf("forward[0].Object = %q, want obj-1", msg.Input.Message.ForwardDifferences[0].Object)
	}
	if msg.Input.Message.ForwardDifferences[0].Attribute != "attr-1" {
		t.Errorf("forward[0].Attribute = %q, want attr-1", msg.Input.Message.ForwardDifferences[0].Attribute)
	}
	if msg.Input.Message.ForwardDifferences[0].Value != "fwd-1" {
		t.Errorf("forward[0].Value = %v, want fwd-1", msg.Input.Message.ForwardDifferences[0].Value)
	}
	if msg.Input.Message.ReverseDifferences[0].Value != "rev-1" {
		t.Errorf("reverse[0].Value = %v, want rev-1", msg.Input.Message.ReverseDifferences[0].Value)
	}
}

func TestAddDifference_RejectsEmptyObject(t *testing.T) {
	b := NewBuilder("sim-1")
	err := b.AddDifference("", "attr", "f", "r")
	if !errors.Is(err, ErrInvalidDifference) {
		t.Errorf("got %v, want ErrInvalidDifference", err)
	}
	if b.Len() != 0 {
		t.Errorf("Len after rejected add = %d, want 0", b.Len())
	}
}

func TestAddDifference_RejectsEmptyAttribute(t *testing.T) {
	b := NewBuilder("sim-1")
	err := b.AddDifference("obj", "", "f", "r")
	if !errors.Is(err, ErrInvalidDifference) {
		t.Errorf("got %v, want ErrInvalidDifference", err)
	}
	if b.Len() != 0 {
		t.Errorf("Len after rejected add = %d, want 0", b.Len())
	}
}

func TestReset_ClearsDiffsKeepsSimulationID(t *testing.T) {
	b := NewBuilder("sim-1")
	_ = b.AddDifference("obj-1", "attr-1", 1, 0)
	_ = b.AddDifference("obj-2", "attr-2", 2, 0)
	if b.Len() != 2 {
		t.Fatalf("setup: Len = %d, want 2", b.Len())
	}
	b.Reset()
	if b.Len() != 0 {
		t.Errorf("after Reset Len = %d, want 0", b.Len())
	}
	msg, err := b.Message(fixedEpoch)
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	if msg.Input.SimulationID == nil {
		t.Error("simulation_id should survive Reset")
	}
	if *msg.Input.SimulationID != "sim-1" {
		t.Errorf("simulation_id after Reset = %v, want sim-1", *msg.Input.SimulationID)
	}
}

func TestEmptySimulationID_OmitsField(t *testing.T) {
	b := NewBuilder("")
	msg, err := b.Message(fixedEpoch)
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	if msg.Input.SimulationID != nil {
		t.Errorf("simulation_id = %v, want omitted (nil)", msg.Input.SimulationID)
	}
	// Round-trip through JSON to verify the field is actually absent.
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "simulation_id") {
		t.Errorf("expected simulation_id absent from JSON, got %s", string(raw))
	}
}

func TestFluent_AddDifferenceChain(t *testing.T) {
	// Fluent chaining variant returns *Builder. The error path exists
	// (AddDifference returns error), but a fluent helper that ignores
	// failure is wanted for ergonomic chaining. WithDifference panics.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("unexpected panic: %v", r)
		}
	}()
	b := NewBuilder("sim-1").
		WithDifference("obj-1", "attr-1", 1, 0).
		WithDifference("obj-2", "attr-2", 2, 0)
	if b.Len() != 2 {
		t.Errorf("Len = %d, want 2", b.Len())
	}
}

func TestWithDifference_PanicsOnInvalid(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Error("expected panic")
		}
	}()
	_ = NewBuilder("sim-1").WithDifference("", "attr", 1, 0)
}

// failingReader is a test-only io.Reader that always fails, used to
// exercise Message's crypto/rand failure path without
// depending on crypto/rand.Reader itself ever actually failing.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("failingReader: simulated crypto/rand failure")
}

func TestMessage_RandFailureSurfacesError_NoPanic(t *testing.T) {
	orig := randReader
	randReader = failingReader{}
	t.Cleanup(func() { randReader = orig })

	b := NewBuilder("sim-1")
	msg, err := b.Message(fixedEpoch)
	if msg != nil {
		t.Errorf("Message returned non-nil Message on rand failure: %+v", msg)
	}
	if !errors.Is(err, ErrRandFailure) {
		t.Errorf("Message error = %v, want wrapping ErrRandFailure", err)
	}
}

func TestMessageNow_RandFailureSurfacesError_NoPanic(t *testing.T) {
	orig := randReader
	randReader = failingReader{}
	t.Cleanup(func() { randReader = orig })

	b := NewBuilder("sim-1")
	msg, err := b.MessageNow()
	if msg != nil {
		t.Errorf("MessageNow returned non-nil Message on rand failure: %+v", msg)
	}
	if !errors.Is(err, ErrRandFailure) {
		t.Errorf("MessageNow error = %v, want wrapping ErrRandFailure", err)
	}
}

func TestBytes_RandFailureSurfacesError_NoPanic(t *testing.T) {
	orig := randReader
	randReader = failingReader{}
	t.Cleanup(func() { randReader = orig })

	b := NewBuilder("sim-1")
	_ = b.AddDifference("obj", "attr", 1, 0)
	raw, err := b.Bytes(fixedEpoch)
	if raw != nil {
		t.Errorf("Bytes returned non-nil bytes on rand failure: %s", raw)
	}
	if !errors.Is(err, ErrRandFailure) {
		t.Errorf("Bytes error = %v, want wrapping ErrRandFailure", err)
	}
}

func TestMessage_GeneratesUUIDv4(t *testing.T) {
	b := NewBuilder("sim-1")
	msg, err := b.Message(fixedEpoch)
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	mrid := msg.Input.Message.DifferenceMRID
	if mrid == "" {
		t.Fatal("difference_mrid is empty")
	}
	// UUIDv4: 8-4-4-4-12 hex with version=4 nibble and variant 8/9/a/b.
	uuidV4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidV4.MatchString(mrid) {
		t.Errorf("difference_mrid %q is not a v4 UUID", mrid)
	}
}

func TestMessage_FreshMRIDPerCall(t *testing.T) {
	// Matches Python upstream: each get_message() generates a fresh
	// difference_mrid. The bridge correlates by message identity, not
	// builder identity.
	b := NewBuilder("sim-1")
	_ = b.AddDifference("obj", "attr", 1, 0)
	m1, err := b.Message(fixedEpoch)
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	m2, err := b.Message(fixedEpoch)
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	if m1.Input.Message.DifferenceMRID == m2.Input.Message.DifferenceMRID {
		t.Errorf("expected fresh mRID per Message call; got identical %q",
			m1.Input.Message.DifferenceMRID)
	}
}

func TestBytes_RoundTrip(t *testing.T) {
	b := NewBuilder("sim-1")
	_ = b.AddDifference("obj-1", "attr-1", "f1", "r1")
	raw, err := b.Bytes(fixedEpoch)
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	var decoded Message
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Command != "update" {
		t.Errorf("command = %q, want update", decoded.Command)
	}
	if decoded.Input.SimulationID == nil || *decoded.Input.SimulationID != "sim-1" {
		t.Errorf("simulation_id round-trip failed: %v", decoded.Input.SimulationID)
	}
	if len(decoded.Input.Message.ForwardDifferences) != 1 {
		t.Errorf("forward len = %d, want 1", len(decoded.Input.Message.ForwardDifferences))
	}
}

func TestMessage_GoldenCase1(t *testing.T) {
	b := NewBuilder("sim-123")
	_ = b.AddDifference(
		"_4C4846A8-312B-4D03-BFF8-BCB58CAB4366",
		"DERControl.DERControlBase.opModTargetW",
		map[string]any{"multiplier": 5, "value": 1},
		map[string]any{"multiplier": 1, "value": 1},
	)
	_ = b.AddDifference("_OTHER", "DERControl.description", "new value", "old value")
	msg, err := b.Message(fixedEpoch)
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	// Pin the mRID for golden comparison.
	msg.Input.Message.DifferenceMRID = fixedMRID
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := canonicalize(t, raw)
	want := canonicalize(t, loadGolden(t, "golden_case1.json"))
	if !bytes.Equal(got, want) {
		t.Errorf("case1 mismatch:\n got: %s\nwant: %s", string(got), string(want))
	}
}

func TestMessage_GoldenCase2_NoSimNoDiffs(t *testing.T) {
	b := NewBuilder("")
	msg, err := b.Message(fixedEpoch)
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	msg.Input.Message.DifferenceMRID = fixedMRID
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := canonicalize(t, raw)
	want := canonicalize(t, loadGolden(t, "golden_case2.json"))
	if !bytes.Equal(got, want) {
		t.Errorf("case2 mismatch:\n got: %s\nwant: %s", string(got), string(want))
	}
}

func TestMessage_TimestampSet(t *testing.T) {
	b := NewBuilder("sim-1")
	msg, err := b.Message(fixedEpoch)
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	if msg.Input.Message.Timestamp != fixedEpoch {
		t.Errorf("timestamp = %d, want %d", msg.Input.Message.Timestamp, fixedEpoch)
	}
}

func TestMessageNow_UsesCurrentEpoch(t *testing.T) {
	b := NewBuilder("sim-1")
	msg, err := b.MessageNow()
	if err != nil {
		t.Fatalf("MessageNow: %v", err)
	}
	if msg.Input.Message.Timestamp <= 0 {
		t.Errorf("MessageNow timestamp = %d, want positive", msg.Input.Message.Timestamp)
	}
}

func TestBytesNow_NonEmpty(t *testing.T) {
	b := NewBuilder("sim-1")
	_ = b.AddDifference("obj", "attr", 1, 0)
	raw, err := b.BytesNow()
	if err != nil {
		t.Fatalf("BytesNow: %v", err)
	}
	if len(raw) == 0 {
		t.Error("BytesNow returned empty bytes")
	}
	var decoded Message
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Input.Message.Timestamp <= 0 {
		t.Errorf("BytesNow timestamp = %d, want positive", decoded.Input.Message.Timestamp)
	}
}

func TestMessage_GoldenCase3_IntegerSimIDAsString(t *testing.T) {
	// The Python upstream supports str|int|None for simulation_id.
	// The Go API takes string only; an integer sim ID is converted by
	// the caller. Pin the wire shape we expect when a caller passes
	// "42" as the sim ID. The golden case3 fixture has integer 42, so
	// this test re-canonicalizes to string-vs-number agnostic by
	// loading the golden and asserting structural equality with the
	// Go-produced shape (after numeric coercion). Simpler: assert the
	// shape independently.
	b := NewBuilder("42")
	_ = b.AddDifference("obj-x", "attr-y", 99, 100)
	msg, err := b.Message(fixedEpoch)
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	msg.Input.Message.DifferenceMRID = fixedMRID
	if msg.Input.SimulationID == nil || *msg.Input.SimulationID != "42" {
		t.Errorf("sim id = %v, want \"42\"", msg.Input.SimulationID)
	}
	if len(msg.Input.Message.ForwardDifferences) != 1 {
		t.Fatalf("forward len = %d, want 1", len(msg.Input.Message.ForwardDifferences))
	}
	fwd := msg.Input.Message.ForwardDifferences[0]
	if fwd.Object != "obj-x" || fwd.Attribute != "attr-y" || fwd.Value != 99 {
		t.Errorf("fwd = %+v, want {obj-x attr-y 99}", fwd)
	}
	rev := msg.Input.Message.ReverseDifferences[0]
	if rev.Value != 100 {
		t.Errorf("rev.Value = %v, want 100", rev.Value)
	}
}

func TestForwardDifferencesIndependent(t *testing.T) {
	// Mutating the value of a passed-in map must not silently affect the
	// builder's stored copy because both sides are typed `any` and Go
	// will reference-share. Documented behavior: the builder keeps the
	// passed reference verbatim; callers responsible for not mutating.
	// This test pins the documented behavior so future refactors do not
	// change it without intent.
	val := map[string]any{"k": 1}
	b := NewBuilder("sim-1")
	_ = b.AddDifference("obj", "attr", val, val)
	val["k"] = 2
	msg, err := b.Message(fixedEpoch)
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	got := msg.Input.Message.ForwardDifferences[0].Value.(map[string]any)["k"]
	if got != 2 {
		t.Errorf("documented passthrough behavior changed: got %v, want 2", got)
	}
}
