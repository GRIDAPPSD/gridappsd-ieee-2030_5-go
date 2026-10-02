package sim

// Topic helpers mirror the Python upstream in
// gridappsd-python-lib/gridappsd/topics.py:
//
//	BASE_SIMULATION_TOPIC     = "/topic/goss.gridappsd.simulation"
//	BASE_SIMULATION_LOG_TOPIC = "/topic/goss.gridappsd.simulation.log"
//
//	simulation_output_topic(sim_id) -> "{base}.output.{sim_id}"
//	simulation_input_topic(sim_id)  -> "{base}.input.{sim_id}"
//	simulation_log_topic(sim_id)    -> "{baseLog}.{sim_id}"
//
// Catalog open question 5 resolved: simulation pub/sub uses the
// /topic/ form on send and on subscribe.

const (
	baseTopic              = "/topic/goss.gridappsd"
	baseSimulationTopic    = "/topic/goss.gridappsd.simulation"
	baseSimulationLogTopic = "/topic/goss.gridappsd.simulation.log"
)

// OutputTopic returns the per-simulation measurement output topic. The
// bridge subscribes to this topic to receive timestep frames.
func OutputTopic(simID string) string {
	return baseSimulationTopic + ".output." + simID
}

// InputTopic returns the per-simulation input topic. The bridge
// publishes DifferenceBuilder messages here (see cim/diff).
func InputTopic(simID string) string {
	return baseSimulationTopic + ".input." + simID
}

// LogTopic returns the per-simulation log topic. The bridge can
// subscribe here for simulation-side log frames (timestep markers,
// completion notices) emitted by the platform.
func LogTopic(simID string) string {
	return baseSimulationLogTopic + "." + simID
}

// ApplicationOutputTopic returns the topic an application publishes its
// output on. It follows gridappsd-python v2026.09.0 topics.py
// application_output_topic: with a simulation id the form is
// "{BASE_SIMULATION_TOPIC}.{app}.{sim}.output", without one
// "{BASE_TOPIC}.{app}.output". An empty appID returns "" so a caller
// that hands the result to a publisher fails closed instead of sending
// to a malformed destination (the Python helper asserts instead).
func ApplicationOutputTopic(appID, simID string) string {
	if appID == "" {
		return ""
	}
	if simID != "" {
		return baseSimulationTopic + "." + appID + "." + simID + ".output"
	}
	return baseTopic + "." + appID + ".output"
}

// ApplicationInputTopic returns the topic an application receives its
// input on. It follows gridappsd-python v2026.09.0 topics.py
// application_input_topic: with a simulation id the form is
// "{BASE_SIMULATION_TOPIC}.{app}.{sim}.input", without one
// "{BASE_TOPIC}.{app}.input". An empty appID returns "" so a caller
// that hands the result to a subscriber fails closed instead of
// listening on a malformed destination (the Python helper asserts
// instead).
func ApplicationInputTopic(appID, simID string) string {
	if appID == "" {
		return ""
	}
	if simID != "" {
		return baseSimulationTopic + "." + appID + "." + simID + ".input"
	}
	return baseTopic + "." + appID + ".input"
}
