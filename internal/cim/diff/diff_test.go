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
	msg := b.Message(fixedEpoch)
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
	msg := b.Message(fixedEpoch)
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
	msg := b.Message(fixedEpoch)
	if msg.Input.SimulationID == nil {
		t.Error("simulation_id should survive Reset")
	}
	if *msg.Input.SimulationID != "sim-1" {
		t.Errorf("simulation_id after Reset = %v, want sim-1", *msg.Input.SimulationID)
	}
}

func TestEmptySimulationID_OmitsField(t *testing.T) {
	b := NewBuilder("")
	msg := b.Message(fixedEpoch)
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
	// failure is wanted for ergonomic chaining. MustAddDifference panics.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("unexpected panic: %v", r)
		}
	}()
	b := NewBuilder("sim-1").
		MustAddDifference("obj-1", "attr-1", 1, 0).
		MustAddDifference("obj-2", "attr-2", 2, 0)
	if b.Len() != 2 {
		t.Errorf("Len = %d, want 2", b.Len())
	}
}

func TestMustAddDifference_PanicsOnInvalid(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Error("expected panic")
		}
	}()
	_ = NewBuilder("sim-1").MustAddDifference("", "attr", 1, 0)
}

func TestMessage_GeneratesUUIDv4(t *testing.T) {
	b := NewBuilder("sim-1")
	msg := b.Message(fixedEpoch)
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
	m1 := b.Message(fixedEpoch)
	m2 := b.Message(fixedEpoch)
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
	msg := b.Message(fixedEpoch)
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
	msg := b.Message(fixedEpoch)
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
	msg := b.Message(fixedEpoch)
	if msg.Input.Message.Timestamp != fixedEpoch {
		t.Errorf("timestamp = %d, want %d", msg.Input.Message.Timestamp, fixedEpoch)
	}
}

func TestMessageNow_UsesCurrentEpoch(t *testing.T) {
	b := NewBuilder("sim-1")
	msg := b.MessageNow()
	if msg.Input.Message.Timestamp <= 0 {
		t.Errorf("MessageNow timestamp = %d, want positive", msg.Input.Message.Timestamp)
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
	msg := b.Message(fixedEpoch)
	got := msg.Input.Message.ForwardDifferences[0].Value.(map[string]any)["k"]
	if got != 2 {
		t.Errorf("documented passthrough behavior changed: got %v, want 2", got)
	}
}
