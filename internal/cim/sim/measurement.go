package sim

import (
	"encoding/json"
	"fmt"
)

// MeasurementFrame is the typical simulation-output frame the platform
// publishes on /topic/goss.gridappsd.simulation.output.<sim_id>. The
// outer envelope carries simulation_id; the nested message object carries
// a timestep timestamp and a measurements map keyed by measurement mRID.
//
// Reference shape from gridappsd-python-lib/gridappsd/simulation.py
// (__onmeasurement: timestamp = message["message"]["timestamp"];
// measurements = message["message"]["measurements"]) and the measurement
// assembly in
// GOSS-GridAPPS-D/services/helicsgossbridge/service/helics_goss_bridge.py
// (sets measurement["measurement_mrid"], "magnitude", "angle", "value").
//
// Decoder is permissive: extra fields (e.g., schema-version markers the
// platform may add later) are tolerated. Callers needing custom fields
// can json.Unmarshal the raw frame body into their own type instead.
type MeasurementFrame struct {
	SimulationID string                  `json:"simulation_id"`
	Message      MeasurementFrameMessage `json:"message"`
}

// MeasurementFrameMessage is the inner "message" object inside a
// simulation-output frame.
type MeasurementFrameMessage struct {
	Timestamp    int64                  `json:"timestamp"`
	Measurements map[string]Measurement `json:"measurements"`
}

// Measurement is a single point measurement keyed by its mRID inside
// MeasurementFrameMessage.Measurements. Different conducting-equipment
// types fill different fields:
//   - voltage, current, power: magnitude + angle (polar form)
//   - switch position, breaker state, tap position: value (scalar)
//
// Callers should expect at most one of (Magnitude/Angle) vs Value to be
// populated per measurement; the platform produces one or the other based
// on the underlying object class.
type Measurement struct {
	MeasurementMRID string  `json:"measurement_mrid"`
	Value           float64 `json:"value,omitempty"`
	Magnitude       float64 `json:"magnitude,omitempty"`
	Angle           float64 `json:"angle,omitempty"`
}

// decodeFrame is a thin shared wrapper around json.Unmarshal. Centralized
// so a future change (e.g., switch to a streaming decoder for very large
// frames, or DisallowUnknownFields gating) lands in one place.
func decodeFrame(body []byte, out *MeasurementFrame) error {
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("sim: decode measurement frame: %w", err)
	}
	return nil
}
