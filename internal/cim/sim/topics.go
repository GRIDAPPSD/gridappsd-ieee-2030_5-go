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
	baseApplicationTopic   = "/topic/goss.gridappsd.application"
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
// output on: "{base}.{app}.{sim}.output", or "{BASE_TOPIC}.{app}.output"
// without a simulation id. An empty appID returns "" so a caller that
// hands the result to a publisher fails closed.
// The segment is "application" by operator decision; gridappsd-python uses "simulation" there.
func ApplicationOutputTopic(appID, simID string) string {
	if appID == "" {
		return ""
	}
	if simID != "" {
		return baseApplicationTopic + "." + appID + "." + simID + ".output"
	}
	return baseTopic + "." + appID + ".output"
}

// ApplicationInputTopic returns the topic an application receives its
// input on: "{base}.{app}.{sim}.input", or "{BASE_TOPIC}.{app}.input"
// without a simulation id. An empty appID returns "" so a caller that
// hands the result to a subscriber fails closed.
// The segment is "application" by operator decision; gridappsd-python uses "simulation" there.
func ApplicationInputTopic(appID, simID string) string {
	if appID == "" {
		return ""
	}
	if simID != "" {
		return baseApplicationTopic + "." + appID + "." + simID + ".input"
	}
	return baseTopic + "." + appID + ".input"
}
