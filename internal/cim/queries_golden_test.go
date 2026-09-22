package cim

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// update regenerates the golden files in testdata/queries from the
// current template output when run as:
//
//	go test ./internal/cim/... -run TestSPARQLQueriesGolden -update
//
// This is the deliberate refresh path for an intentional template
// change: render, inspect the diff, re-run with -update, commit the
// new golden alongside the template edit.
var update = flag.Bool("update", false, "update golden files")

// goldenQueryCase names a SPARQL template wrapper and the golden file
// that pins its exact rendered output (byte-for-byte, not substring).
// This closes the Dutch M2 gap: substring assertions in
// TestSPARQLQueriesEnvelope missed a trailing-whitespace divergence
// because they only check that expected fragments are present, never
// that nothing else in the template drifted.
type goldenQueryCase struct {
	name string
	call func(c *Client, ctx context.Context, feederID string) (*QueryDataResult, error)
}

func goldenQueryCases() []goldenQueryCase {
	return []goldenQueryCase{
		{name: "solar", call: (*Client).QuerySolar},
		{name: "battery", call: (*Client).QueryBattery},
		{name: "inverter", call: (*Client).QueryInverter},
		{name: "all_der_groups", call: (*Client).QueryAllDERGroups},
		{name: "pec_count", call: (*Client).QueryPECCount},
		{name: "energy_consumers", call: (*Client).QueryEnergyConsumers},
	}
}

// goldenFeederID is fixed across all golden cases so the rendered
// VALUES clause is deterministic and comparable across regenerations.
const goldenFeederID = "_DEADBEEF-0000-0000-0000-000000000123"

// TestSPARQLQueriesGolden renders every SPARQL query template through
// its public Query* wrapper and compares the full queryString against a
// checked-in golden file in testdata/queries/. Unlike
// TestSPARQLQueriesEnvelope's substring checks, this catches ANY
// character-level drift in the rendered template, including trailing
// whitespace, reordered clauses, or an accidental edit to an unrelated
// line.
func TestSPARQLQueriesGolden(t *testing.T) {
	for _, tc := range goldenQueryCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			mr := &mockRequester{resp: []byte(okEnvelope)}
			c := NewClient(mr)

			if _, err := tc.call(c, context.Background(), goldenFeederID); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			body := decodeBody(t, mr.gotBody)
			qs, ok := body["queryString"].(string)
			if !ok {
				t.Fatalf(`body["queryString"] missing or not a string: %v`, body["queryString"])
			}

			goldenPath := filepath.Join("testdata", "queries", tc.name+".sparql")

			if *update {
				if err := os.WriteFile(goldenPath, []byte(qs), 0o644); err != nil {
					t.Fatalf("write golden %s: %v", goldenPath, err)
				}
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s: %v (run with -update to generate it)", goldenPath, err)
			}
			if qs != string(want) {
				t.Errorf("rendered queryString for %s does not match golden %s\n--- got ---\n%s\n--- want ---\n%s", tc.name, goldenPath, qs, string(want))
			}
		})
	}
}
