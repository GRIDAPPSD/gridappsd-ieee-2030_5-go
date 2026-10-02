package sim

import "testing"

// The Python upstream defines these topic strings in
// gridappsd-python-lib/gridappsd/topics.py:
//
//	BASE_SIMULATION_TOPIC     = "/topic/goss.gridappsd.simulation"
//	BASE_SIMULATION_LOG_TOPIC = "/topic/goss.gridappsd.simulation.log"
//	simulation_output_topic(sim_id) -> "{base}.output.{sim_id}"
//	simulation_input_topic(sim_id)  -> "{base}.input.{sim_id}"
//	simulation_log_topic(sim_id)    -> "{baseLog}.{sim_id}"
//
// The Go helpers must produce the identical wire-form strings; the
// catalog open question 5 resolution is "always send the /topic/ form."

func TestOutputTopic(t *testing.T) {
	cases := []struct {
		simID string
		want  string
	}{
		{"12345", "/topic/goss.gridappsd.simulation.output.12345"},
		{"_49AD8E07-3BF9-A4E2-CB8F-C3722F837B62",
			"/topic/goss.gridappsd.simulation.output._49AD8E07-3BF9-A4E2-CB8F-C3722F837B62"},
		{"", "/topic/goss.gridappsd.simulation.output."},
	}
	for _, tc := range cases {
		t.Run(tc.simID, func(t *testing.T) {
			if got := OutputTopic(tc.simID); got != tc.want {
				t.Errorf("OutputTopic(%q) = %q, want %q", tc.simID, got, tc.want)
			}
		})
	}
}

func TestInputTopic(t *testing.T) {
	cases := []struct {
		simID string
		want  string
	}{
		{"12345", "/topic/goss.gridappsd.simulation.input.12345"},
		{"_49AD8E07-3BF9-A4E2-CB8F-C3722F837B62",
			"/topic/goss.gridappsd.simulation.input._49AD8E07-3BF9-A4E2-CB8F-C3722F837B62"},
		{"", "/topic/goss.gridappsd.simulation.input."},
	}
	for _, tc := range cases {
		t.Run(tc.simID, func(t *testing.T) {
			if got := InputTopic(tc.simID); got != tc.want {
				t.Errorf("InputTopic(%q) = %q, want %q", tc.simID, got, tc.want)
			}
		})
	}
}

func TestLogTopic(t *testing.T) {
	cases := []struct {
		simID string
		want  string
	}{
		{"12345", "/topic/goss.gridappsd.simulation.log.12345"},
		{"_49AD8E07-3BF9-A4E2-CB8F-C3722F837B62",
			"/topic/goss.gridappsd.simulation.log._49AD8E07-3BF9-A4E2-CB8F-C3722F837B62"},
	}
	for _, tc := range cases {
		t.Run(tc.simID, func(t *testing.T) {
			if got := LogTopic(tc.simID); got != tc.want {
				t.Errorf("LogTopic(%q) = %q, want %q", tc.simID, got, tc.want)
			}
		})
	}
}

// TestTopicsAreTopicForm locks in catalog open-question 5: subscribing or
// publishing on simulation streams uses the /topic/ form, not /queue/ and
// not the bare goss.gridappsd... form. A future refactor that changes the
// prefix would silently break the broker handshake.
func TestTopicsAreTopicForm(t *testing.T) {
	cases := []struct {
		name string
		got  string
	}{
		{"output", OutputTopic("X")},
		{"input", InputTopic("X")},
		{"log", LogTopic("X")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.got) < 7 || tc.got[:7] != "/topic/" {
				t.Errorf("%s topic %q does not start with /topic/", tc.name, tc.got)
			}
		})
	}
}

// The expected strings are literal on purpose: the segment is "application", not
// the "simulation" gridappsd-python uses.
func TestApplicationOutputTopic(t *testing.T) {
	cases := []struct {
		name, appID, simID, want string
	}{
		{"default app with sim", "IEEE_2030_5", "X", "/topic/goss.gridappsd.application.IEEE_2030_5.X.output"},
		{"mrid style sim", "IEEE_2030_5", "_49AD8E07", "/topic/goss.gridappsd.application.IEEE_2030_5._49AD8E07.output"},
		{"custom app", "my_app", "12345", "/topic/goss.gridappsd.application.my_app.12345.output"},
		{"no sim id", "IEEE_2030_5", "", "/topic/goss.gridappsd.IEEE_2030_5.output"},
		{"empty app fails closed", "", "X", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ApplicationOutputTopic(tc.appID, tc.simID); got != tc.want {
				t.Errorf("ApplicationOutputTopic(%q, %q) = %q, want %q", tc.appID, tc.simID, got, tc.want)
			}
		})
	}
}

// The expected strings are literal on purpose: the segment is "application", not
// the "simulation" gridappsd-python uses.
func TestApplicationInputTopic(t *testing.T) {
	cases := []struct {
		name, appID, simID, want string
	}{
		{"default app with sim", "IEEE_2030_5", "X", "/topic/goss.gridappsd.application.IEEE_2030_5.X.input"},
		{"mrid style sim", "IEEE_2030_5", "_49AD8E07", "/topic/goss.gridappsd.application.IEEE_2030_5._49AD8E07.input"},
		{"custom app", "my_app", "12345", "/topic/goss.gridappsd.application.my_app.12345.input"},
		{"no sim id", "IEEE_2030_5", "", "/topic/goss.gridappsd.IEEE_2030_5.input"},
		{"empty app fails closed", "", "X", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ApplicationInputTopic(tc.appID, tc.simID); got != tc.want {
				t.Errorf("ApplicationInputTopic(%q, %q) = %q, want %q", tc.appID, tc.simID, got, tc.want)
			}
		})
	}
}
