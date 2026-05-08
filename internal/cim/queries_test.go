package cim

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// queryFn names the four GAGO-010 wrappers in a table-driven shape.
// Each entry binds a wrapper method to (a) the expected SPARQL substrings
// that prove the query template was selected and the feederID substituted,
// and (b) a representative requestType field on the JSON envelope.
type queryCase struct {
	name           string
	call           func(c *Client, ctx context.Context, feederID string) (*QueryDataResult, error)
	wantSubstrings []string // substrings expected in the queryString after feederID substitution
}

func queryCases() []queryCase {
	// Substrings asserted against each rendered template. Two invariants
	// worth flagging:
	//
	//   1. The leading underscore on the input feederID "_FEEDER123" is
	//      stripped before substitution into the SPARQL VALUES clause,
	//      so the on-wire form is "FEEDER123". This matches the
	//      gridappsd-docker:develop dataset shape, which stores
	//      c:IdentifiedObject.mRID as a bare uppercase UUID without
	//      the underscore prefix the Python upstream's call sites use.
	//
	//   2. The PowerElectronicsUnit relationship is OPTIONAL in three
	//      of four templates (Solar, Battery, Inverter) so the query
	//      returns one row per PowerElectronicsConnection regardless of
	//      whether a child Unit exists. COALESCE binds prefer the
	//      Unit's name/mRID when present; fall back to the PEC's when
	//      absent. See the doc block at the top of queries.go.
	return []queryCase{
		{
			name: "QuerySolar",
			call: (*Client).QuerySolar,
			wantSubstrings: []string{
				"# Solar - DistSolar",
				`VALUES ?fdrid {"FEEDER123"}`,
				"OPTIONAL {",
				"c:PowerElectronicsConnection.PowerElectronicsUnit",
				"c:PhotovoltaicUnit",
				"BIND(COALESCE(?unitName, ?pecName) AS ?name)",
				"BIND(COALESCE(?unitID, ?pecid) AS ?id)",
			},
		},
		{
			name: "QueryBattery",
			call: (*Client).QueryBattery,
			wantSubstrings: []string{
				"# Storage - DistStorage",
				`VALUES ?fdrid {"FEEDER123"}`,
				"OPTIONAL {",
				"c:BatteryUnit",
				"c:BatteryUnit.ratedE",
				"c:BatteryUnit.storedE",
				"c:BatteryUnit.batteryState",
				"BIND(COALESCE(?unitName, ?pecName) AS ?name)",
				"BIND(COALESCE(?unitID, ?pecid) AS ?id)",
			},
		},
		{
			name: "QueryInverter",
			call: (*Client).QueryInverter,
			wantSubstrings: []string{
				`VALUES ?fdrid {"FEEDER123"}`,
				"?pec a c:PowerElectronicsConnection.",
				"?pec c:IdentifiedObject.mRID ?pecid",
				"c:PowerElectronicsConnection.ratedS",
				"OPTIONAL {",
				"c:PowerElectronicsConnection.PowerElectronicsUnit",
				"BIND(COALESCE(?unitName, ?pecName) AS ?name)",
				"BIND(COALESCE(?unitID, ?pecid) AS ?id)",
			},
		},
		{
			name: "QueryAllDERGroups",
			call: (*Client).QueryAllDERGroups,
			wantSubstrings: []string{
				"#get all EndDeviceGroup",
				`VALUES ?fdrid {"FEEDER123"}`,
				"?q1 a c:EndDeviceGroup",
				"c:EndDeviceGroup.EndDevice",
				"c:DERFunction",
			},
		},
	}
}

// okEnvelope is the empty-but-valid SPARQL response shape.
const okEnvelope = `{"data":{"head":{"vars":[]},"results":{"bindings":[]}},"responseComplete":true,"id":"x"}`

func TestSPARQLQueriesEnvelope(t *testing.T) {
	t.Parallel()

	for _, tc := range queryCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mr := &mockRequester{resp: []byte(okEnvelope)}
			c := NewClient(mr)

			if _, err := tc.call(c, context.Background(), "_FEEDER123"); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
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
			qs, ok := body["queryString"].(string)
			if !ok {
				t.Fatalf(`body["queryString"] missing or not a string: %v`, body["queryString"])
			}
			for _, want := range tc.wantSubstrings {
				if !strings.Contains(qs, want) {
					t.Errorf("queryString missing %q\nfull body:\n%s", want, qs)
				}
			}
		})
	}
}

func TestSPARQLQueriesParseRoundTrip(t *testing.T) {
	t.Parallel()

	// Each query is asserted to return its bindings unchanged. v0 wrappers
	// do not project; the catalog defers typed projection to bridge code.
	const payload = `{
		"data":{
			"head":{"vars":["name","bus"]},
			"results":{"bindings":[
				{"name":{"type":"literal","value":"PV1"},"bus":{"value":"BusA"}},
				{"name":{"type":"literal","value":"PV2"},"bus":{"value":"BusB"}}
			]}
		},
		"responseComplete":true,
		"id":"abc"
	}`

	for _, tc := range queryCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mr := &mockRequester{resp: []byte(payload)}
			c := NewClient(mr)

			res, err := tc.call(c, context.Background(), "_FEEDER123")
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got := res.Head.Vars; len(got) != 2 || got[0] != "name" || got[1] != "bus" {
				t.Errorf("Head.Vars = %v, want [name bus]", got)
			}
			if len(res.Results.Bindings) != 2 {
				t.Fatalf("len(Bindings) = %d, want 2", len(res.Results.Bindings))
			}
			if v := res.Results.Bindings[0]["name"].Value; v != "PV1" {
				t.Errorf("first name binding = %q, want PV1", v)
			}
			if v := res.Results.Bindings[1]["bus"].Value; v != "BusB" {
				t.Errorf("second bus binding = %q, want BusB", v)
			}
		})
	}
}

func TestSPARQLQueriesInvalidFeederID(t *testing.T) {
	t.Parallel()

	bad := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"double-quote", `bad"id`},
		{"angle-open", "bad<id"},
		{"angle-close", "bad>id"},
		{"newline", "bad\nid"},
		{"carriage-return", "bad\rid"},
		{"backslash", `bad\id`},
	}

	for _, b := range bad {
		b := b
		for _, tc := range queryCases() {
			tc := tc
			t.Run(tc.name+"/"+b.name, func(t *testing.T) {
				t.Parallel()

				mr := &mockRequester{resp: []byte(okEnvelope)}
				c := NewClient(mr)

				_, err := tc.call(c, context.Background(), b.id)
				if !errors.Is(err, ErrInvalidFeederID) {
					t.Errorf("err = %v, want ErrInvalidFeederID", err)
				}
				if mr.gotDestination != "" {
					t.Errorf("Requester should not be invoked for invalid feederID; destination = %q", mr.gotDestination)
				}
			})
		}
	}
}

func TestSPARQLQueriesContextCancelled(t *testing.T) {
	t.Parallel()

	for _, tc := range queryCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			mr := &mockRequester{err: context.Canceled}
			c := NewClient(mr)

			_, err := tc.call(c, ctx, "_FEEDER123")
			if !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want context.Canceled", err)
			}
		})
	}
}

func TestSPARQLQueriesRequesterError(t *testing.T) {
	t.Parallel()

	for _, tc := range queryCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			want := errors.New("transport down")
			mr := &mockRequester{err: want}
			c := NewClient(mr)

			_, err := tc.call(c, context.Background(), "_FEEDER123")
			if !errors.Is(err, want) {
				t.Errorf("err = %v, want wrapping %v", err, want)
			}
		})
	}
}

func TestSPARQLQueriesIncompleteResponse(t *testing.T) {
	t.Parallel()

	for _, tc := range queryCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mr := &mockRequester{resp: []byte(`{"data":{"head":{"vars":[]},"results":{"bindings":[]}},"responseComplete":false}`)}
			c := NewClient(mr)

			_, err := tc.call(c, context.Background(), "_FEEDER123")
			if !errors.Is(err, ErrIncompleteResponse) {
				t.Errorf("err = %v, want ErrIncompleteResponse", err)
			}
		})
	}
}

// TestSPARQLQueriesFeederIDUnderscoreStripping pins the on-wire
// substitution shape for both forms of the feederID input. The
// gridappsd-docker:develop dataset stores c:IdentifiedObject.mRID as a
// bare uppercase UUID without an underscore prefix, while the Python
// upstream call sites and our bridge wiring frequently pass an
// underscore-prefixed UUID for legacy reasons. The wrapper accepts
// either form and emits the stripped form into the SPARQL VALUES
// clause, so the bridge's caller does not have to choose.
func TestSPARQLQueriesFeederIDUnderscoreStripping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		input    string
		wantSubs []string
	}{
		{
			name:     "underscore-prefixed-uuid-is-stripped",
			input:    "_E407CBB6-8C8D-9BC9-589C-AB83FBF0826D",
			wantSubs: []string{`VALUES ?fdrid {"E407CBB6-8C8D-9BC9-589C-AB83FBF0826D"}`},
		},
		{
			name:     "bare-uuid-passes-through",
			input:    "E407CBB6-8C8D-9BC9-589C-AB83FBF0826D",
			wantSubs: []string{`VALUES ?fdrid {"E407CBB6-8C8D-9BC9-589C-AB83FBF0826D"}`},
		},
		{
			name:     "no-double-strip-on-double-underscore",
			input:    "__abc",
			wantSubs: []string{`VALUES ?fdrid {"_abc"}`},
		},
	}

	for _, c := range cases {
		c := c
		for _, tc := range queryCases() {
			tc := tc
			t.Run(tc.name+"/"+c.name, func(t *testing.T) {
				t.Parallel()

				mr := &mockRequester{resp: []byte(okEnvelope)}
				cl := NewClient(mr)

				if _, err := tc.call(cl, context.Background(), c.input); err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}

				body := decodeBody(t, mr.gotBody)
				qs, ok := body["queryString"].(string)
				if !ok {
					t.Fatalf(`body["queryString"] missing or not a string: %v`, body["queryString"])
				}
				for _, want := range c.wantSubs {
					if !strings.Contains(qs, want) {
						t.Errorf("queryString missing %q\nfull queryString:\n%s", want, qs)
					}
				}
			})
		}
	}
}

// TestSPARQLQueriesSchemaVariantParse exercises the Go-side response
// parser against two recorded JSON envelope shapes (current and older
// CIM schemas). It does NOT execute SPARQL against a Blazegraph and
// does NOT prove that the templates render correctly under either
// schema; that verification belongs to the //go:build gridappsd
// integration tests against a live broker. Older-schema integration
// verification is deferred until an older-shape dataset is available.
//
// The two recorded shapes are:
//
//  1. current-schema (gridappsd-docker:develop): each PEC row has
//     ?id == ?pecid because no PowerElectronicsUnit child exists. The
//     OPTIONAL block did not bind, so COALESCE fell back to the PEC
//     fields.
//
//  2. older-schema (Python upstream's assumed shape): each PEC row has
//     a unit-level ?id distinct from ?pecid because the OPTIONAL
//     block bound a PhotovoltaicUnit/BatteryUnit child. COALESCE
//     selected the Unit's identity.
//
// Both shapes parse to non-zero rows through the same wrapper. The
// test does not assert business semantics on the bindings; it asserts
// the wrapper layer does not reject either shape and surfaces the
// expected row count and key field values.
func TestSPARQLQueriesSchemaVariantParse(t *testing.T) {
	t.Parallel()

	const currentSchema = `{
		"data":{
			"head":{"vars":["name","bus","ratedS","ratedU","ipu","p","q","fdrid","id","pecid","phases"]},
			"results":{"bindings":[
				{"name":{"value":"dg_84"},"bus":{"value":"84"},"ratedS":{"value":"120000.0"},"id":{"value":"PECID-1"},"pecid":{"value":"PECID-1"}},
				{"name":{"value":"dg_90"},"bus":{"value":"90"},"ratedS":{"value":"120000.0"},"id":{"value":"PECID-2"},"pecid":{"value":"PECID-2"}}
			]}
		},
		"responseComplete":true,
		"id":"req1"
	}`

	const olderSchema = `{
		"data":{
			"head":{"vars":["name","bus","ratedS","ratedU","ipu","p","q","fdrid","id","pecid","phases"]},
			"results":{"bindings":[
				{"name":{"value":"PV1"},"bus":{"value":"84"},"ratedS":{"value":"120000.0"},"id":{"value":"UNIT-1"},"pecid":{"value":"PECID-1"}},
				{"name":{"value":"PV2"},"bus":{"value":"90"},"ratedS":{"value":"120000.0"},"id":{"value":"UNIT-2"},"pecid":{"value":"PECID-2"}}
			]}
		},
		"responseComplete":true,
		"id":"req2"
	}`

	shapes := []struct {
		name        string
		payload     string
		wantIDEqPEC bool
	}{
		{"current-schema-PEC-as-leaf", currentSchema, true},
		{"older-schema-PEC-plus-Unit", olderSchema, false},
	}

	for _, s := range shapes {
		s := s
		for _, tc := range queryCases() {
			if tc.name == "QueryAllDERGroups" {
				// AllDERGroups operates on EndDeviceGroup, not PEC; the
				// schema variants in this test do not apply.
				continue
			}
			tc := tc
			t.Run(tc.name+"/"+s.name, func(t *testing.T) {
				t.Parallel()

				mr := &mockRequester{resp: []byte(s.payload)}
				cl := NewClient(mr)

				res, err := tc.call(cl, context.Background(), "_FEEDER123")
				if err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}
				if got := len(res.Results.Bindings); got != 2 {
					t.Fatalf("len(Bindings) = %d, want 2", got)
				}
				row := res.Results.Bindings[0]
				if row["id"].Value == "" {
					t.Errorf("first row id is empty; want non-empty")
				}
				if row["pecid"].Value == "" {
					t.Errorf("first row pecid is empty; want non-empty")
				}
				idEqPEC := row["id"].Value == row["pecid"].Value
				if idEqPEC != s.wantIDEqPEC {
					t.Errorf("id == pecid: got %v, want %v (id=%q pecid=%q)",
						idEqPEC, s.wantIDEqPEC, row["id"].Value, row["pecid"].Value)
				}
			})
		}
	}
}
