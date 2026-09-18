package adminui

import (
	"encoding/json"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

// fixtureHistorySnapshot returns two series exercising both lanes: a
// percent reported-state series and a watts commanded-setpoint series,
// with distinct sample counts so SampleCount is asserted by value rather
// than by coincidence.
func fixtureHistorySnapshot() []telemetryhistory.SeriesSnapshot {
	return []telemetryhistory.SeriesSnapshot{
		{
			Key: telemetryhistory.SeriesKey{Object: "device-1", Attribute: "DERStatus.stateOfChargeStatus"},
			Samples: []telemetryhistory.Sample{
				{At: 1785390800, Value: 62.5},
				{At: 1785390815, Value: 65.0},
			},
		},
		{
			Key: telemetryhistory.SeriesKey{Object: "device-1", Attribute: "DERControl.DERControlBase.opModTargetW"},
			Samples: []telemetryhistory.Sample{
				{At: 1785390891, Value: 5000},
			},
		},
	}
}

// TestHandleHistoryMethodsRejected asserts every non-GET method is
// rejected, per method, at /api/history specifically.
func TestHandleHistoryMethodsRejected(t *testing.T) {
	t.Parallel()

	s := newTestServerWithHistory(t, Config{Key: testKey},
		&fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{}, &fakeIdentity{}, &fakeStomp{}, &fakeClientObserver{},
		&fakeHistory{snap: fixtureHistorySnapshot()})

	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			rec := doRequest(t, s.Handler(), method, "/api/history", "Bearer "+testKey, "localhost")
			if rec.Code != 405 {
				t.Errorf("%s /api/history status = %d, want 405", method, rec.Code)
			}
		})
	}
}

// TestHandleHistoryBehindSameMiddlewareChain asserts /api/history sits
// behind the identical host-allowlist and Bearer chain every other
// endpoint in this package does: 403 on a disallowed Host header, 401 on
// a missing or wrong Bearer token.
func TestHandleHistoryBehindSameMiddlewareChain(t *testing.T) {
	t.Parallel()

	s := newTestServerWithHistory(t, Config{Key: testKey},
		&fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{}, &fakeIdentity{}, &fakeStomp{}, &fakeClientObserver{},
		&fakeHistory{snap: fixtureHistorySnapshot()})

	assertGETWithHost(t, s.Handler(), "/api/history", "Bearer "+testKey, "evil.example.com", 403)
	assertGETWithHost(t, s.Handler(), "/api/history", "", "localhost", 401)
	assertGETWithHost(t, s.Handler(), "/api/history", "Bearer wrong-token", "localhost", 401)
	assertGETWithHost(t, s.Handler(), "/api/history", "Bearer "+testKey, "localhost", 200)
}

// TestHandleHistoryReturnsFieldValues is the data-invariants field-value
// test: every response field (configured, topics, seriesCap, and each
// series' object/attribute/lane/unit/sampleCount/cap/samples) is
// asserted against the injected fixture, not merely "the response
// parsed."
func TestHandleHistoryReturnsFieldValues(t *testing.T) {
	t.Parallel()

	s := newTestServerWithHistory(t, Config{Key: testKey, HistoryTopics: []string{"/topic/a", "/topic/b"}},
		&fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{}, &fakeIdentity{}, &fakeStomp{}, &fakeClientObserver{},
		&fakeHistory{snap: fixtureHistorySnapshot()})

	rec := doRequest(t, s.Handler(), "GET", "/api/history", "Bearer "+testKey, "localhost")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var got historyResponse
	decodeJSON(t, rec.Body.Bytes(), &got)

	if !got.Configured {
		t.Error("Configured = false, want true (HistoryTopics is non-empty)")
	}
	if len(got.Topics) != 2 || got.Topics[0] != "/topic/a" || got.Topics[1] != "/topic/b" {
		t.Errorf("Topics = %v, want [/topic/a /topic/b]", got.Topics)
	}
	if got.SeriesCap != telemetryhistory.MaxSeries {
		t.Errorf("SeriesCap = %d, want %d", got.SeriesCap, telemetryhistory.MaxSeries)
	}
	if len(got.Series) != 2 {
		t.Fatalf("len(Series) = %d, want 2: %+v", len(got.Series), got.Series)
	}

	byAttr := map[string]historySeriesResponse{}
	for _, s := range got.Series {
		byAttr[s.Attribute] = s
	}

	soc, ok := byAttr["DERStatus.stateOfChargeStatus"]
	if !ok {
		t.Fatalf("no series for DERStatus.stateOfChargeStatus in %+v", got.Series)
	}
	if soc.Object != "device-1" {
		t.Errorf("SOC series Object = %q, want %q", soc.Object, "device-1")
	}
	if soc.Lane != string(telemetryhistory.LaneReportedState) {
		t.Errorf("SOC series Lane = %q, want %q", soc.Lane, telemetryhistory.LaneReportedState)
	}
	if soc.Unit != "percent" {
		t.Errorf("SOC series Unit = %q, want %q", soc.Unit, "percent")
	}
	if soc.SampleCount != 2 {
		t.Errorf("SOC series SampleCount = %d, want 2", soc.SampleCount)
	}
	if soc.Cap != telemetryhistory.SamplesPerSeries {
		t.Errorf("SOC series Cap = %d, want %d", soc.Cap, telemetryhistory.SamplesPerSeries)
	}
	if len(soc.Samples) != 2 || soc.Samples[1].Value != 65.0 {
		t.Errorf("SOC series Samples = %+v, want second sample Value=65.0", soc.Samples)
	}

	target, ok := byAttr["DERControl.DERControlBase.opModTargetW"]
	if !ok {
		t.Fatalf("no series for DERControl.DERControlBase.opModTargetW in %+v", got.Series)
	}
	if target.Lane != string(telemetryhistory.LaneCommandedSetpoint) {
		t.Errorf("target series Lane = %q, want %q", target.Lane, telemetryhistory.LaneCommandedSetpoint)
	}
	if target.Unit != "watts" {
		t.Errorf("target series Unit = %q, want %q", target.Unit, "watts")
	}
	if target.SampleCount != 1 {
		t.Errorf("target series SampleCount = %d, want 1", target.SampleCount)
	}
	if len(target.Samples) != 1 || target.Samples[0].Value != 5000 {
		t.Errorf("target series Samples = %+v, want one sample Value=5000", target.Samples)
	}
	// Percent and watts must never collapse to the same unit string: this
	// is the axis-separation invariant the response exists to carry.
	if soc.Unit == target.Unit {
		t.Fatalf("SOC unit %q equals target unit %q; percent and watts must be distinguishable", soc.Unit, target.Unit)
	}
}

// TestHandleHistoryUnconfiguredReturnsWellFormedEmptyResult: with no
// HistoryTopics configured, the endpoint must return 200 with an empty,
// well-formed result (Configured=false, empty Topics and Series), never
// an error status and never a nil/null body for either slice field.
func TestHandleHistoryUnconfiguredReturnsWellFormedEmptyResult(t *testing.T) {
	t.Parallel()

	s := newTestServerWithHistory(t, Config{Key: testKey},
		&fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{}, &fakeIdentity{}, &fakeStomp{}, &fakeClientObserver{},
		&fakeHistory{}) // no HistoryTopics, no retained series

	rec := doRequest(t, s.Handler(), "GET", "/api/history", "Bearer "+testKey, "localhost")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	// Assert against the raw JSON, not just the decoded struct: a Go nil
	// slice and an empty slice both decode into the same zero-length
	// slice, which would hide a "null" that violates the "well-formed
	// empty result" contract at the wire level.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal raw response: %v", err)
	}
	if string(raw["topics"]) == "null" {
		t.Error(`"topics" serialized as null, want "[]"`)
	}
	if string(raw["series"]) == "null" {
		t.Error(`"series" serialized as null, want "[]"`)
	}

	var got historyResponse
	decodeJSON(t, rec.Body.Bytes(), &got)
	if got.Configured {
		t.Error("Configured = true, want false with no HistoryTopics set")
	}
	if len(got.Topics) != 0 {
		t.Errorf("Topics = %v, want empty", got.Topics)
	}
	if len(got.Series) != 0 {
		t.Errorf("Series = %v, want empty", got.Series)
	}
}

// TestHandleHistoryConfiguredButNoSamplesYet is the "no data yet" state,
// distinct from "unconfigured": Configured is true (topics are set) but
// the store has recorded nothing, still a 200 with an empty Series list.
func TestHandleHistoryConfiguredButNoSamplesYet(t *testing.T) {
	t.Parallel()

	s := newTestServerWithHistory(t, Config{Key: testKey, HistoryTopics: []string{"/topic/a"}},
		&fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{}, &fakeIdentity{}, &fakeStomp{}, &fakeClientObserver{},
		&fakeHistory{}) // topics configured, but nothing retained yet

	rec := doRequest(t, s.Handler(), "GET", "/api/history", "Bearer "+testKey, "localhost")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got historyResponse
	decodeJSON(t, rec.Body.Bytes(), &got)
	if !got.Configured {
		t.Error("Configured = false, want true: this is the 'no data yet', not 'unconfigured', state")
	}
	if len(got.Series) != 0 {
		t.Errorf("Series = %v, want empty (no samples have arrived yet)", got.Series)
	}
}

// TestHandleHistoryIgnoresQueryParameters asserts that no query
// parameter can raise a cap, widen retention, or add a series: a request
// carrying parameters that would attempt exactly that must produce the
// identical response to one with no query string at all, and must never
// touch the underlying store.
func TestHandleHistoryIgnoresQueryParameters(t *testing.T) {
	t.Parallel()

	history := &fakeHistory{snap: fixtureHistorySnapshot()}
	s := newTestServerWithHistory(t, Config{Key: testKey, HistoryTopics: []string{"/topic/a"}},
		&fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{}, &fakeIdentity{}, &fakeStomp{}, &fakeClientObserver{},
		history)

	baseline := doRequest(t, s.Handler(), "GET", "/api/history", "Bearer "+testKey, "localhost")
	tampered := doRequest(t, s.Handler(), "GET",
		"/api/history?cap=999999&seriesCap=999999&limit=0&samplesPerSeries=999999&object=anything&attribute=anything",
		"Bearer "+testKey, "localhost")

	if baseline.Body.String() != tampered.Body.String() {
		t.Errorf("response changed under query parameters:\nbaseline = %s\ntampered = %s", baseline.Body.String(), tampered.Body.String())
	}
	// The fixture's own snapshot slice must be untouched: handleHistory
	// must never mutate what HistorySource.Snapshot returned.
	if len(history.snap) != 2 {
		t.Errorf("fakeHistory.snap mutated: len = %d, want 2", len(history.snap))
	}
}

// TestHandleHistoryResponseFieldsAreLimitedToPermittedSet asserts the
// serialized JSON's field set directly (not merely that it parses):
// this is the no-secret/no-certificate/no-pIN acceptance criterion. A
// field name outside this permitted set fails the test even if its
// value happens to be harmless, since the point is a structural
// guarantee, not a value inspection.
func TestHandleHistoryResponseFieldsAreLimitedToPermittedSet(t *testing.T) {
	t.Parallel()

	s := newTestServerWithHistory(t, Config{Key: testKey, HistoryTopics: []string{"/topic/a"}},
		&fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{}, &fakeIdentity{}, &fakeStomp{}, &fakeClientObserver{},
		&fakeHistory{snap: fixtureHistorySnapshot()})

	rec := doRequest(t, s.Handler(), "GET", "/api/history", "Bearer "+testKey, "localhost")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal raw response: %v", err)
	}
	permittedTop := map[string]bool{"configured": true, "topics": true, "seriesCap": true, "series": true}
	for k := range raw {
		if !permittedTop[k] {
			t.Errorf("top-level field %q not in the permitted set %v", k, permittedTop)
		}
	}

	var series []map[string]json.RawMessage
	if err := json.Unmarshal(raw["series"], &series); err != nil {
		t.Fatalf("unmarshal series: %v", err)
	}
	permittedSeries := map[string]bool{
		"object": true, "attribute": true, "lane": true, "unit": true,
		"sampleCount": true, "cap": true, "samples": true,
	}
	for _, s := range series {
		for k := range s {
			if !permittedSeries[k] {
				t.Errorf("series field %q not in the permitted set %v", k, permittedSeries)
			}
		}
		var samples []map[string]json.RawMessage
		if err := json.Unmarshal(s["samples"], &samples); err != nil {
			t.Fatalf("unmarshal samples: %v", err)
		}
		permittedSample := map[string]bool{"at": true, "value": true}
		for _, smp := range samples {
			for k := range smp {
				if !permittedSample[k] {
					t.Errorf("sample field %q not in the permitted set %v", k, permittedSample)
				}
			}
		}
	}

	// Belt-and-suspenders substring check against a few forbidden-shaped
	// tokens: none of these appear anywhere the field-set check above
	// would already have failed on, so this only catches something that
	// snuck into a permitted field's own VALUE, which the field-set
	// check does not cover.
	body := rec.Body.String()
	for _, forbidden := range []string{"pIN", "pin\":", "certificate", "privateKey", "BEGIN CERTIFICATE", "BEGIN PRIVATE KEY"} {
		if containsFold(body, forbidden) {
			t.Errorf("response body contains forbidden token %q", forbidden)
		}
	}
}

// TestHandleHistorySkipsSeriesWithNoClassification is the defensive
// branch: a series key whose attribute telemetryhistory.MetaFor cannot
// classify (something the store should never actually hold, since the
// decoder only ever writes allowlisted attributes) is omitted from the
// response rather than serialized with a fabricated lane/unit.
func TestHandleHistorySkipsSeriesWithNoClassification(t *testing.T) {
	t.Parallel()

	snap := []telemetryhistory.SeriesSnapshot{
		{
			Key:     telemetryhistory.SeriesKey{Object: "device-1", Attribute: "DERStatus.alarmStatus"},
			Samples: []telemetryhistory.Sample{{At: 1, Value: 1}},
		},
		{
			Key:     telemetryhistory.SeriesKey{Object: "device-1", Attribute: "DERStatus.stateOfChargeStatus"},
			Samples: []telemetryhistory.Sample{{At: 1, Value: 50}},
		},
	}
	s := newTestServerWithHistory(t, Config{Key: testKey, HistoryTopics: []string{"/topic/a"}},
		&fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{}, &fakeIdentity{}, &fakeStomp{}, &fakeClientObserver{},
		&fakeHistory{snap: snap})

	rec := doRequest(t, s.Handler(), "GET", "/api/history", "Bearer "+testKey, "localhost")
	var got historyResponse
	decodeJSON(t, rec.Body.Bytes(), &got)

	if len(got.Series) != 1 {
		t.Fatalf("len(Series) = %d, want 1 (the unclassifiable alarmStatus series must be omitted): %+v", len(got.Series), got.Series)
	}
	if got.Series[0].Attribute != "DERStatus.stateOfChargeStatus" {
		t.Errorf("Series[0].Attribute = %q, want the classifiable one", got.Series[0].Attribute)
	}
}

// containsFold reports whether body contains substr, ASCII case
// insensitively.
func containsFold(body, substr string) bool {
	for i := 0; i+len(substr) <= len(body); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			b, s := body[i+j], substr[j]
			if 'A' <= b && b <= 'Z' {
				b += 'a' - 'A'
			}
			if 'A' <= s && s <= 'Z' {
				s += 'a' - 'A'
			}
			if b != s {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
