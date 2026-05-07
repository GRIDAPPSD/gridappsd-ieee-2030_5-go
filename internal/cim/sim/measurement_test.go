package sim

import (
	"encoding/json"
	"testing"
)

// Sample simulation-output frame shape, derived from
// gridappsd-python-lib/gridappsd/simulation.py __onmeasurement (uses
// message["message"]["timestamp"] and message["message"]["measurements"])
// and the helics-goss bridge measurement assembly at
// GOSS-GridAPPS-D/services/helicsgossbridge/service/helics_goss_bridge.py
// (sets measurement["measurement_mrid"], "magnitude", "angle", "value").
//
// The wire body is JSON UTF-8.

const sampleOutputFrame = `{
  "simulation_id": "12345",
  "message": {
    "timestamp": 1714502400,
    "measurements": {
      "_meas-001": {
        "measurement_mrid": "_meas-001",
        "magnitude": 7199.557856794634,
        "angle": -0.5253516
      },
      "_meas-002": {
        "measurement_mrid": "_meas-002",
        "value": 1
      }
    }
  }
}`

// TestMeasurementFrame_Unmarshal verifies the typed struct round-trips a
// realistic platform output frame. The test fixture mirrors the platform's
// shape exactly: top-level simulation_id and a nested message object that
// carries timestamp plus a measurements map keyed by mRID.
func TestMeasurementFrame_Unmarshal(t *testing.T) {
	var frame MeasurementFrame
	if err := json.Unmarshal([]byte(sampleOutputFrame), &frame); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if frame.SimulationID != "12345" {
		t.Errorf("SimulationID = %q, want %q", frame.SimulationID, "12345")
	}
	if frame.Message.Timestamp != 1714502400 {
		t.Errorf("Message.Timestamp = %d, want 1714502400", frame.Message.Timestamp)
	}
	if got := len(frame.Message.Measurements); got != 2 {
		t.Fatalf("len(Message.Measurements) = %d, want 2", got)
	}

	m1, ok := frame.Message.Measurements["_meas-001"]
	if !ok {
		t.Fatalf("missing _meas-001 in measurements")
	}
	if m1.MeasurementMRID != "_meas-001" {
		t.Errorf("m1.MeasurementMRID = %q, want %q", m1.MeasurementMRID, "_meas-001")
	}
	if m1.Magnitude != 7199.557856794634 {
		t.Errorf("m1.Magnitude = %v, want 7199.557856794634", m1.Magnitude)
	}
	if m1.Angle != -0.5253516 {
		t.Errorf("m1.Angle = %v, want -0.5253516", m1.Angle)
	}

	m2, ok := frame.Message.Measurements["_meas-002"]
	if !ok {
		t.Fatalf("missing _meas-002 in measurements")
	}
	if m2.Value != 1 {
		t.Errorf("m2.Value = %v, want 1", m2.Value)
	}
}

// TestMeasurementFrame_EmptyMeasurements covers the platform's pre-step
// frames where no measurements have been recorded yet. Decoder must not
// panic and must yield an empty (or nil) map.
func TestMeasurementFrame_EmptyMeasurements(t *testing.T) {
	body := `{"simulation_id":"x","message":{"timestamp":0,"measurements":{}}}`
	var frame MeasurementFrame
	if err := json.Unmarshal([]byte(body), &frame); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(frame.Message.Measurements) != 0 {
		t.Errorf("expected empty measurements, got %d entries", len(frame.Message.Measurements))
	}
}

// TestMeasurementFrame_UnknownFieldsTolerated verifies the decoder ignores
// extra fields the platform may add (e.g., schema-version, log markers).
// json.Unmarshal does this by default; this test pins the expectation so a
// future switch to DisallowUnknownFields surfaces as a named failure.
func TestMeasurementFrame_UnknownFieldsTolerated(t *testing.T) {
	body := `{
		"simulation_id": "x",
		"message": {
			"timestamp": 1,
			"measurements": {},
			"some_future_field": "ignored"
		},
		"top_level_extra": 42
	}`
	var frame MeasurementFrame
	if err := json.Unmarshal([]byte(body), &frame); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if frame.SimulationID != "x" {
		t.Errorf("SimulationID = %q, want %q", frame.SimulationID, "x")
	}
}
