package cim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// mockRequester captures the last (destination, body) pair passed to Request
// and returns a pre-configured response or error. It is the substrate for
// every test in this file; nothing in this package should hit a real broker.
type mockRequester struct {
	gotDestination string
	gotBody        []byte
	gotCtx         context.Context

	resp []byte
	err  error
}

func (m *mockRequester) Request(ctx context.Context, destination string, body []byte) ([]byte, error) {
	m.gotCtx = ctx
	m.gotDestination = destination
	// Copy body so callers cannot mutate the captured bytes after the fact.
	m.gotBody = append([]byte(nil), body...)
	if m.err != nil {
		return nil, m.err
	}
	return m.resp, nil
}

func decodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decoding captured body %s: %v", raw, err)
	}
	return m
}

// ---------- QueryData ----------

func TestQueryDataEnvelope(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{
		resp: []byte(`{"data":{"head":{"vars":[]},"results":{"bindings":[]}},"responseComplete":true,"id":"x"}`),
	}
	c := NewClient(mr)

	const sparql = "SELECT ?s WHERE { ?s ?p ?o }"
	if _, err := c.QueryData(context.Background(), sparql); err != nil {
		t.Fatalf("QueryData: %v", err)
	}

	if mr.gotDestination != RequestPowergridModel {
		t.Errorf("destination = %q, want %q", mr.gotDestination, RequestPowergridModel)
	}
	body := decodeBody(t, mr.gotBody)
	if body["requestType"] != "QUERY" {
		t.Errorf(`body["requestType"] = %v, want "QUERY"`, body["requestType"])
	}
	if body["resultFormat"] != "JSON" {
		t.Errorf(`body["resultFormat"] = %v, want "JSON"`, body["resultFormat"])
	}
	if body["queryString"] != sparql {
		t.Errorf(`body["queryString"] = %v, want %q`, body["queryString"], sparql)
	}
}

func TestQueryDataParse(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(`{
		"data":{
			"head":{"vars":["name","bus"]},
			"results":{"bindings":[
				{"name":{"type":"literal","value":"Load1"},"bus":{"value":"BusA"}},
				{"name":{"type":"literal","value":"Load2"},"bus":{"value":"BusB"}}
			]}
		},
		"responseComplete":true,
		"id":"abc"
	}`)}
	c := NewClient(mr)

	res, err := c.QueryData(context.Background(), "SELECT *")
	if err != nil {
		t.Fatalf("QueryData: %v", err)
	}

	if got := res.Head.Vars; len(got) != 2 || got[0] != "name" || got[1] != "bus" {
		t.Errorf("Head.Vars = %v, want [name bus]", got)
	}
	if len(res.Results.Bindings) != 2 {
		t.Fatalf("len(Bindings) = %d, want 2", len(res.Results.Bindings))
	}
	if v := res.Results.Bindings[0]["name"].Value; v != "Load1" {
		t.Errorf("first name binding = %q, want Load1", v)
	}
	if ty := res.Results.Bindings[0]["name"].Type; ty != "literal" {
		t.Errorf("first name binding type = %q, want literal", ty)
	}
	if v := res.Results.Bindings[1]["bus"].Value; v != "BusB" {
		t.Errorf("second bus binding = %q, want BusB", v)
	}
}

func TestQueryDataIncompleteResponse(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(`{"data":{"head":{"vars":[]},"results":{"bindings":[]}},"responseComplete":false}`)}
	c := NewClient(mr)

	_, err := c.QueryData(context.Background(), "SELECT *")
	if !errors.Is(err, ErrIncompleteResponse) {
		t.Fatalf("err = %v, want ErrIncompleteResponse", err)
	}
}

func TestQueryDataErrorEnvelope(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(`{"error":"something broke","header":{},"message":"bad input"}`)}
	c := NewClient(mr)

	_, err := c.QueryData(context.Background(), "SELECT *")
	if err == nil {
		t.Fatal("expected error envelope to surface")
	}
	if !errors.Is(err, ErrServerError) {
		t.Errorf("err = %v, want errors.Is ErrServerError", err)
	}
	if !strings.Contains(err.Error(), "something broke") {
		t.Errorf("err = %q, want containing 'something broke'", err)
	}
}

func TestQueryDataRequesterError(t *testing.T) {
	t.Parallel()

	want := errors.New("transport down")
	mr := &mockRequester{err: want}
	c := NewClient(mr)

	_, err := c.QueryData(context.Background(), "SELECT *")
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrapping %v", err, want)
	}
}

func TestQueryDataContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	mr := &mockRequester{err: context.Canceled}
	c := NewClient(mr)

	_, err := c.QueryData(ctx, "SELECT *")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// ---------- QueryModelInfo ----------

func TestQueryModelInfoEnvelope(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(`{"data":{"models":[]}}`)}
	c := NewClient(mr)

	if _, err := c.QueryModelInfo(context.Background()); err != nil {
		t.Fatalf("QueryModelInfo: %v", err)
	}

	if mr.gotDestination != RequestPowergridModel {
		t.Errorf("destination = %q, want %q", mr.gotDestination, RequestPowergridModel)
	}
	body := decodeBody(t, mr.gotBody)
	if body["requestType"] != "QUERY_MODEL_INFO" {
		t.Errorf(`body["requestType"] = %v, want "QUERY_MODEL_INFO"`, body["requestType"])
	}
	if body["resultFormat"] != "JSON" {
		t.Errorf(`body["resultFormat"] = %v, want "JSON"`, body["resultFormat"])
	}
	if _, present := body["queryString"]; present {
		t.Error(`body["queryString"] should not be present for QUERY_MODEL_INFO`)
	}
}

func TestQueryModelInfoParse(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(`{"data":{"models":[
		{"modelId":"_abc","modelName":"ieee123","stationName":"S1","subRegionName":"SR1","regionName":"R1"},
		{"modelId":"_def","modelName":"ieee13","stationName":"S2","subRegionName":"SR2","regionName":"R2"}
	]}}`)}
	c := NewClient(mr)

	res, err := c.QueryModelInfo(context.Background())
	if err != nil {
		t.Fatalf("QueryModelInfo: %v", err)
	}
	if len(res.Models) != 2 {
		t.Fatalf("len(Models) = %d, want 2", len(res.Models))
	}
	if res.Models[0].ModelID != "_abc" {
		t.Errorf("Models[0].ModelID = %q, want _abc", res.Models[0].ModelID)
	}
	if res.Models[0].ModelName != "ieee123" {
		t.Errorf("Models[0].ModelName = %q, want ieee123", res.Models[0].ModelName)
	}
	if res.Models[1].StationName != "S2" {
		t.Errorf("Models[1].StationName = %q, want S2", res.Models[1].StationName)
	}
}

// ---------- QueryObjectDict ----------

func TestQueryObjectDictEnvelope(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(`{"data":{}}`)}
	c := NewClient(mr)

	const modelID = "_xyz"
	const objectType = "PowerElectronicsConnection"
	if _, err := c.QueryObjectDict(context.Background(), modelID, objectType); err != nil {
		t.Fatalf("QueryObjectDict: %v", err)
	}

	if mr.gotDestination != RequestPowergridModel {
		t.Errorf("destination = %q, want %q", mr.gotDestination, RequestPowergridModel)
	}
	body := decodeBody(t, mr.gotBody)
	if body["requestType"] != "QUERY_OBJECT_DICT" {
		t.Errorf(`body["requestType"] = %v, want "QUERY_OBJECT_DICT"`, body["requestType"])
	}
	if body["modelId"] != modelID {
		t.Errorf(`body["modelId"] = %v, want %q`, body["modelId"], modelID)
	}
	if body["objectType"] != objectType {
		t.Errorf(`body["objectType"] = %v, want %q`, body["objectType"], objectType)
	}
}

func TestQueryObjectDictParse(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(`{"data":{"PowerElectronicsConnection":[{"mRID":"_p1","name":"PEC1"}]}}`)}
	c := NewClient(mr)

	res, err := c.QueryObjectDict(context.Background(), "_xyz", "PowerElectronicsConnection")
	if err != nil {
		t.Fatalf("QueryObjectDict: %v", err)
	}
	pecs, ok := res.Data["PowerElectronicsConnection"]
	if !ok {
		t.Fatalf("res.Data missing PowerElectronicsConnection key, got keys: %v", res.Data)
	}
	pecList, ok := pecs.([]any)
	if !ok || len(pecList) != 1 {
		t.Fatalf("PowerElectronicsConnection value = %v, want slice of 1", pecs)
	}
}

// ---------- GetCIMDictionary ----------

func TestGetCIMDictionaryEnvelope(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(`{"data":{"feeders":[]}}`)}
	c := NewClient(mr)

	const modelID = "_feeder1"
	if _, err := c.GetCIMDictionary(context.Background(), modelID); err != nil {
		t.Fatalf("GetCIMDictionary: %v", err)
	}

	if mr.gotDestination != RequestConfig {
		t.Errorf("destination = %q, want %q", mr.gotDestination, RequestConfig)
	}
	body := decodeBody(t, mr.gotBody)
	if body["configurationType"] != "CIM Dictionary" {
		t.Errorf(`body["configurationType"] = %v, want "CIM Dictionary"`, body["configurationType"])
	}
	params, ok := body["parameters"].(map[string]any)
	if !ok {
		t.Fatalf(`body["parameters"] not a map: %v`, body["parameters"])
	}
	if params["model_id"] != modelID {
		t.Errorf(`parameters.model_id = %v, want %q`, params["model_id"], modelID)
	}
}

func TestGetCIMDictionaryParse(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(`{"data":{"feeders":[{
		"energyconsumers":[{"mRID":"_ec1","name":"EC1","phases":"ABC"}],
		"capacitors":[{"mRID":"_cap1","name":"Cap1"}],
		"measurements":[{"mRID":"_m1","name":"M1"}],
		"solarpanels":[{"mRID":"_sp1","name":"SP1"}],
		"batteries":[{"mRID":"_b1","name":"B1"}]
	}]}}`)}
	c := NewClient(mr)

	res, err := c.GetCIMDictionary(context.Background(), "_feeder1")
	if err != nil {
		t.Fatalf("GetCIMDictionary: %v", err)
	}
	if len(res.Feeders) != 1 {
		t.Fatalf("len(Feeders) = %d, want 1", len(res.Feeders))
	}
	f := res.Feeders[0]
	if len(f.EnergyConsumers) != 1 || f.EnergyConsumers[0].MRID != "_ec1" {
		t.Errorf("EnergyConsumers = %v, want [{_ec1, EC1, ABC}]", f.EnergyConsumers)
	}
	if f.EnergyConsumers[0].Phases != "ABC" {
		t.Errorf("EnergyConsumers[0].Phases = %q, want ABC", f.EnergyConsumers[0].Phases)
	}
	if len(f.Measurements) != 1 || f.Measurements[0].MRID != "_m1" {
		t.Errorf("Measurements = %v", f.Measurements)
	}
	if len(f.SolarPanels) != 1 || f.SolarPanels[0].MRID != "_sp1" {
		t.Errorf("SolarPanels = %v", f.SolarPanels)
	}
	if len(f.Batteries) != 1 || f.Batteries[0].MRID != "_b1" {
		t.Errorf("Batteries = %v", f.Batteries)
	}
	if len(f.Capacitors) != 1 || f.Capacitors[0].MRID != "_cap1" {
		t.Errorf("Capacitors = %v", f.Capacitors)
	}
}

// ---------- GetPlatformStatus ----------

func TestGetPlatformStatusEnvelope(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(`{}`)}
	c := NewClient(mr)

	if _, err := c.GetPlatformStatus(context.Background()); err != nil {
		t.Fatalf("GetPlatformStatus: %v", err)
	}

	if mr.gotDestination != RequestPlatformStatus {
		t.Errorf("destination = %q, want %q", mr.gotDestination, RequestPlatformStatus)
	}
	body := decodeBody(t, mr.gotBody)
	for _, k := range []string{"applications", "services", "appInstances", "serviceInstances"} {
		v, ok := body[k]
		if !ok {
			t.Errorf("body[%q] missing", k)
			continue
		}
		if b, ok := v.(bool); !ok || !b {
			t.Errorf("body[%q] = %v, want true", k, v)
		}
	}
}

func TestGetPlatformStatusParse(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(`{"applications":[{"id":"app1"}],"services":[],"appInstances":[],"serviceInstances":[]}`)}
	c := NewClient(mr)

	res, err := c.GetPlatformStatus(context.Background())
	if err != nil {
		t.Fatalf("GetPlatformStatus: %v", err)
	}
	if res == nil {
		t.Fatal("GetPlatformStatus returned nil result")
	}
	if res.Applications == nil {
		t.Error("Applications is nil; expected raw JSON")
	}
}

// ---------- error envelope, generic ----------

func TestQueryModelInfoErrorEnvelope(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(`{"error":"boom"}`)}
	c := NewClient(mr)

	_, err := c.QueryModelInfo(context.Background())
	if !errors.Is(err, ErrServerError) {
		t.Errorf("err = %v, want errors.Is ErrServerError", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %q, want containing 'boom'", err)
	}
}

func TestQueryObjectDictErrorEnvelope(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(`{"error":"missing model"}`)}
	c := NewClient(mr)

	_, err := c.QueryObjectDict(context.Background(), "_x", "PEC")
	if !errors.Is(err, ErrServerError) {
		t.Errorf("err = %v, want ErrServerError", err)
	}
	if !strings.Contains(err.Error(), "missing model") {
		t.Errorf("err = %q, want containing 'missing model'", err)
	}
}

func TestQueryObjectDictRequesterError(t *testing.T) {
	t.Parallel()

	want := errors.New("subscribe failed")
	mr := &mockRequester{err: want}
	c := NewClient(mr)

	_, err := c.QueryObjectDict(context.Background(), "_x", "PEC")
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrapping %v", err, want)
	}
}

func TestGetCIMDictionaryErrorEnvelope(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(`{"error":"no such feeder"}`)}
	c := NewClient(mr)

	_, err := c.GetCIMDictionary(context.Background(), "_x")
	if !errors.Is(err, ErrServerError) {
		t.Errorf("err = %v, want ErrServerError", err)
	}
	if !strings.Contains(err.Error(), "no such feeder") {
		t.Errorf("err = %q, want containing 'no such feeder'", err)
	}
}

func TestGetCIMDictionaryRequesterError(t *testing.T) {
	t.Parallel()

	want := errors.New("network down")
	mr := &mockRequester{err: want}
	c := NewClient(mr)

	_, err := c.GetCIMDictionary(context.Background(), "_x")
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrapping %v", err, want)
	}
}

func TestGetPlatformStatusErrorEnvelope(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(`{"error":"platform unavailable"}`)}
	c := NewClient(mr)

	_, err := c.GetPlatformStatus(context.Background())
	if !errors.Is(err, ErrServerError) {
		t.Errorf("err = %v, want ErrServerError", err)
	}
	if !strings.Contains(err.Error(), "platform unavailable") {
		t.Errorf("err = %q, want containing 'platform unavailable'", err)
	}
}

func TestGetPlatformStatusRequesterError(t *testing.T) {
	t.Parallel()

	want := errors.New("timed out")
	mr := &mockRequester{err: want}
	c := NewClient(mr)

	_, err := c.GetPlatformStatus(context.Background())
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrapping %v", err, want)
	}
}

func TestQueryModelInfoRequesterError(t *testing.T) {
	t.Parallel()

	want := errors.New("conn closed")
	mr := &mockRequester{err: want}
	c := NewClient(mr)

	_, err := c.QueryModelInfo(context.Background())
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrapping %v", err, want)
	}
}

func TestDecodeEnvelopeErrorShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		wantErr   bool
		wantInErr []string
		notInErr  []string
	}{
		{name: "string", body: `{"error":"boom"}`, wantErr: true, wantInErr: []string{"boom"}},
		{name: "empty string is no error", body: `{"error":"","data":{}}`},
		{name: "null is no error", body: `{"error":null,"data":{}}`},
		{name: "absent is no error", body: `{"data":{}}`},
		{
			name: "object with message", wantErr: true,
			body:      `{"error":{"code":500,"message":"model not found"},"data":{}}`,
			wantInErr: []string{"model not found"},
		},
		{
			name: "object without message", wantErr: true,
			body:      `{"error":{"code":500,"status":"x"}}`,
			wantInErr: []string{`"code"`, `"status"`},
			notInErr:  []string{"500"},
		},
		{name: "empty object still fails", body: `{"error":{}}`, wantErr: true, wantInErr: []string{"no keys"}},
		{
			name: "credential values never shown", wantErr: true,
			body: `{"error":{"password":"hunter2","token":"abc123","code":1,` +
				`"auth":"a1","bearer":"b1","cookie":"c1","pwd":"p1"}}`,
			wantInErr: []string{`"code"`, `"password"`, `"token"`, `"auth"`, `"bearer"`, `"cookie"`, `"pwd"`},
			notInErr:  []string{"hunter2", "abc123", "a1\"", "b1\"", "c1\"", "p1\""},
		},
		{
			name: "message is bounded", wantErr: true,
			body:     `{"error":{"message":"` + strings.Repeat("a", 5000) + `"}}`,
			notInErr: []string{strings.Repeat("a", 1000)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeEnvelope([]byte(tc.body), true)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			if !errors.Is(err, ErrServerError) {
				t.Errorf("err = %v, want errors.Is ErrServerError", err)
			}
			for _, s := range tc.wantInErr {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("err = %q, want containing %q", err, s)
				}
			}
			for _, s := range tc.notInErr {
				if strings.Contains(err.Error(), s) {
					t.Errorf("err contains %q", s)
				}
			}
		})
	}
}

func TestGetPlatformStatusObjectError(t *testing.T) {
	t.Parallel()

	c := NewClient(&mockRequester{resp: []byte(`{"error":{"message":"not ready"}}`)})
	_, err := c.GetPlatformStatus(context.Background())
	if !errors.Is(err, ErrServerError) || !strings.Contains(err.Error(), "not ready") {
		t.Errorf("err = %v, want ErrServerError containing 'not ready'", err)
	}
}

func TestErrorSurvivesOtherFieldTypeError(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		`{"error":"boom","responseComplete":"no"}`,
		`{"error":{"message":"boom"},"responseComplete":"no"}`,
		`{"error":{"message":"boom"},"responseComplete":1}`,
	} {
		_, err := decodeEnvelope([]byte(body), true)
		if !errors.Is(err, ErrServerError) || !strings.Contains(err.Error(), "boom") {
			t.Errorf("decodeEnvelope(%s) err = %v, want ErrServerError boom", body, err)
		}
		_, err = NewClient(&mockRequester{resp: []byte(body)}).GetPlatformStatus(context.Background())
		if !errors.Is(err, ErrServerError) || !strings.Contains(err.Error(), "boom") {
			t.Errorf("GetPlatformStatus(%s) err = %v, want ErrServerError boom", body, err)
		}
	}
}

func TestFirstErrorSticks(t *testing.T) {
	t.Parallel()

	_, err := decodeEnvelope([]byte(`{"error":"first","error":null}`), true)
	if !errors.Is(err, ErrServerError) || !strings.Contains(err.Error(), "first") {
		t.Errorf("err = %v, want first error", err)
	}
	_, err = decodeEnvelope([]byte(`{"error":"first","error":"second"}`), true)
	if err == nil || !strings.Contains(err.Error(), "first") || strings.Contains(err.Error(), "second") {
		t.Errorf("err = %v, want only first", err)
	}
}

// errText decodes body and returns the error text, failing the test
// unless it is an ErrServerError.
func errText(t *testing.T, body string) string {
	t.Helper()
	_, err := decodeEnvelope([]byte(body), true)
	if !errors.Is(err, ErrServerError) {
		t.Fatalf("decodeEnvelope(%.80s) err = %v, want ErrServerError", body, err)
	}
	return err.Error()
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// secretTexts are the shapes the earlier regex rounds chased. Under a
// non-message key none may appear; as a string error or message they are
// shown verbatim, the same as before this change.
var secretTexts = []struct{ text, secret string }{
	{"access_token=AAA1 failed", "AAA1"},
	{"client_secret=BBB2 failed", "BBB2"},
	{"db_password=CCC3 failed", "CCC3"},
	{`password="DDD4" failed`, "DDD4"},
	{`password: 'EEE5' failed`, "EEE5"},
	{`{'password': 'FFF6'}`, "FFF6"},
	{`{"password": "GGG7"}`, "GGG7"},
	{"Authorization: Basic HHH8user", "HHH8user"},
	{"x-api-key: III9", "III9"},
	{`password="ab\"LLL1"`, "LLL1"},
	{`Digest response="MMM2", username="u"`, "MMM2"},
}

func TestErrorObjectNeverShowsNonMessageValues(t *testing.T) {
	t.Parallel()

	for _, tc := range secretTexts {
		body := mustJSON(t, map[string]any{"error": map[string]any{"code": 1, "note": tc.text}})
		got := errText(t, body)
		if strings.Contains(got, tc.secret) {
			t.Errorf("%q: %q leaks %q", tc.text, got, tc.secret)
		}
		if !strings.Contains(got, `"code"`) || !strings.Contains(got, `"note"`) {
			t.Errorf("%q: %q missing key names", tc.text, got)
		}
	}
	got := errText(t, `{"error":[{"password":"hunter2"}]}`)
	if !strings.Contains(got, "array error, 1 elements") || strings.Contains(got, "hunter2") {
		t.Errorf("array error text = %q", got)
	}
	got = errText(t, `{"error":{"code":1,"note":"password=\"DDD4\""}}`)
	if strings.Contains(got, "DDD4") {
		t.Errorf("escaped note leaked: %q", got)
	}
}

func TestErrorTextShownVerbatim(t *testing.T) {
	t.Parallel()

	texts := []string{
		"missing key: feeder_id", "bypass: model not loaded", "test pass: 2",
		"Authorization: failed for user admin",
		// Pinned on purpose: a message the platform writes is shown as is,
		// exactly like a string error at the base commit.
		"login failed token=abc123",
	}
	for _, tc := range secretTexts {
		texts = append(texts, tc.text)
	}
	for _, text := range texts {
		for _, body := range []string{
			mustJSON(t, map[string]any{"error": text}),
			mustJSON(t, map[string]any{"error": map[string]any{"message": text}}),
		} {
			if got := errText(t, body); !strings.HasSuffix(got, ": "+text) {
				t.Errorf("error text = %q, want it to end with %q", got, text)
			}
		}
	}
}

func TestErrorScalarsAndEmptyShapesFail(t *testing.T) {
	t.Parallel()

	tests := []struct{ body, want string }{
		{`{"error":0}`, ": 0"},
		{`{"error":false}`, ": false"},
		{`{"error":true}`, ": true"},
		{`{"error":{}}`, "no keys"},
		{`{"error":[]}`, "array error, 0 elements"},
		{`{"error":{"message":""}}`, `"message"`},
	}
	for _, tc := range tests {
		if got := errText(t, tc.body); !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q does not contain %q", tc.body, got, tc.want)
		}
	}
}

func TestErrorNestedObjectDepth(t *testing.T) {
	t.Parallel()

	got := errText(t, `{"error":{"error":{"message":"feeder not loaded"}}}`)
	if !strings.Contains(got, "feeder not loaded") {
		t.Errorf("text = %q, want nested message", got)
	}
	got = errText(t, `{"error":{"error":{"error":{"error":{"message":"too deep"}}}}}`)
	if strings.Contains(got, "too deep") || !strings.Contains(got, `"error"`) {
		t.Errorf("text = %q, want key names at depth cap", got)
	}
}

func TestErrorTextLengthBounds(t *testing.T) {
	t.Parallel()

	const limit = 512 + 128 // text bound plus the fixed prefix and ellipsis
	deep := strings.Repeat(`{"a":`, 9000) + `1` + strings.Repeat(`}`, 9000)
	var keys strings.Builder
	keys.WriteString(`{"error":{`)
	for i := 0; i < 50; i++ {
		if i > 0 {
			keys.WriteByte(',')
		}
		fmt.Fprintf(&keys, `"k%02d%s":1`, i, strings.Repeat("n", 1024))
	}
	keys.WriteString(`}}`)
	tests := map[string]string{
		"deep object":   `{"error":` + deep + `}`,
		"20 MiB object": `{"error":{"note":"` + strings.Repeat("x", 20<<20) + `"}}`,
		"16 MiB string": `{"error":"` + strings.Repeat("password=x ", 1<<20) + `"}`,
		"50 long keys":  keys.String(),
		"long message":  `{"error":{"message":"` + strings.Repeat("m", 5000) + `"}}`,
		"long number":   `{"error":` + strings.Repeat("9", 5000) + `}`,
		"20 MiB array":  `{"error":[` + strings.Repeat("1,", 1<<20) + `1]}`,
	}
	for name, body := range tests {
		if got := errText(t, body); len(got) > limit {
			t.Errorf("%s: error length %d, want at most %d", name, len(got), limit)
		}
	}
	got := errText(t, keys.String())
	if n := strings.Count(got, `"k`); n > 8 {
		t.Errorf("50 keys showed %d names, want at most 8", n)
	}
}

func TestErrorTextStaysValidUTF8AtTheCut(t *testing.T) {
	t.Parallel()

	for pad := 0; pad < 4; pad++ {
		msg := strings.Repeat("a", 510+pad) + strings.Repeat("\u00e9", 8)
		for _, body := range []string{
			`{"error":"` + msg + `"}`,
			`{"error":{"message":"` + msg + `"}}`,
		} {
			if got := errText(t, body); !utf8.ValidString(got) {
				t.Errorf("pad %d: invalid UTF-8 in %q", pad, got)
			}
		}
	}
	long := strings.Repeat("\u00e9", 40)
	got := errText(t, `{"error":{"`+long+`":1}}`)
	if !utf8.ValidString(got) {
		t.Errorf("key name cut produced invalid UTF-8: %q", got)
	}
}
