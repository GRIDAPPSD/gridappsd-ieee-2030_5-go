package cim

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestQueryEnergyConsumersRendersFeederRestrictedTemplate pins the
// EnergyConsumer enumeration template shape: it must restrict to the
// configured feeder via the same VALUES-then-container join every other
// template in this file uses (c:IdentifiedObject.name is not a unique
// key store-wide, so a join on name alone would be wrong), and it must
// carry the OPTIONAL c:House.EnergyConsumer join that flags a house
// load, per sparqlQueryEnergyConsumers's doc comment.
func TestQueryEnergyConsumersRendersFeederRestrictedTemplate(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(`{"data":{"head":{"vars":["ecid"]},"results":{"bindings":[]}},"responseComplete":true,"id":"x"}`)}
	c := NewClient(mr)

	if _, err := c.QueryEnergyConsumers(context.Background(), "_DEADBEEF-0000-0000-0000-000000000123"); err != nil {
		t.Fatalf("QueryEnergyConsumers: %v", err)
	}

	body := decodeBody(t, mr.gotBody)
	qs, ok := body["queryString"].(string)
	if !ok {
		t.Fatalf(`body["queryString"] missing or not a string: %v`, body["queryString"])
	}

	for _, want := range []string{
		`VALUES ?fdrid {"DEADBEEF-0000-0000-0000-000000000123"}`,
		"?ec a c:EnergyConsumer.",
		"?ec c:Equipment.EquipmentContainer ?fdr.",
		"?fdr c:IdentifiedObject.mRID ?fdrid.",
		"c:House.EnergyConsumer ?ec.",
		"GROUP by ?ecid ?ecname",
	} {
		if !strings.Contains(qs, want) {
			t.Errorf("queryString missing %q\nfull queryString:\n%s", want, qs)
		}
	}
}

// TestQueryEnergyConsumersInvalidFeederID confirms
// QueryEnergyConsumers shares the same feederID validation as the
// other Query* wrappers (queryFeederTemplate).
func TestQueryEnergyConsumersInvalidFeederID(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(okEnvelope)}
	c := NewClient(mr)

	_, err := c.QueryEnergyConsumers(context.Background(), "")
	if !errors.Is(err, ErrInvalidFeederID) {
		t.Errorf("err = %v, want ErrInvalidFeederID", err)
	}
}

// TestQueryEnergyConsumersParsesHouseAndNonHouseRows confirms the
// "house" binding round-trips through decoding exactly as SPARQL would
// emit it: present (non-empty) for a house EC, absent for one with no
// House link. This is the value assertion the golden/substring tests
// above cannot give: they pin the outgoing query, not what a caller
// gets back.
func TestQueryEnergyConsumersParsesHouseAndNonHouseRows(t *testing.T) {
	t.Parallel()

	body := `{"data":{"head":{"vars":["ecid","ecname","house"]},"results":{"bindings":[
		{"ecid":{"type":"literal","value":"HOUSE-EC-1"},"ecname":{"type":"literal","value":"tl_house_1_240v"},"house":{"type":"uri","value":"urn:uuid:house-1"}},
		{"ecid":{"type":"literal","value":"LEG-EC-1"},"ecname":{"type":"literal","value":"utility_bat1_a"}}
	]}},"responseComplete":true,"id":"x"}`
	mr := &mockRequester{resp: []byte(body)}
	c := NewClient(mr)

	res, err := c.QueryEnergyConsumers(context.Background(), "_DEADBEEF-0000-0000-0000-000000000123")
	if err != nil {
		t.Fatalf("QueryEnergyConsumers: %v", err)
	}
	if len(res.Results.Bindings) != 2 {
		t.Fatalf("len(Bindings) = %d, want 2", len(res.Results.Bindings))
	}
	if v := res.Results.Bindings[0]["house"].Value; v == "" {
		t.Errorf(`row 0 "house" binding is empty, want non-empty (house EC)`)
	}
	if v := res.Results.Bindings[1]["house"].Value; v != "" {
		t.Errorf(`row 1 "house" binding = %q, want empty (non-house EC, no OPTIONAL bind)`, v)
	}
}
