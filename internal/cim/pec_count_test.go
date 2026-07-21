package cim

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestQueryPECCountRendersMinimalTemplate pins the GAGO-051 discovery-count
// template shape: it must carry the same identity-plus-feeder-membership
// triples the device-enumeration templates (QuerySolar/QueryBattery/
// QueryInverter) use, but MUST NOT carry any of their additional mandatory
// attribute joins (ratedS, maxIFault, the Terminal/ConnectivityNode bus
// link, etc). Those extra joins are exactly what makes a PEC silently
// vanish from the device-enumeration rows on an incompletely attributed
// feeder; this count exists to detect that drop, so it cannot share the
// same drop-prone shape.
func TestQueryPECCountRendersMinimalTemplate(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(`{"data":{"head":{"vars":["count"]},"results":{"bindings":[{"count":{"type":"literal","value":"14"}}]}},"responseComplete":true,"id":"x"}`)}
	c := NewClient(mr)

	res, err := c.QueryPECCount(context.Background(), "_FEEDER123")
	if err != nil {
		t.Fatalf("QueryPECCount: %v", err)
	}

	body := decodeBody(t, mr.gotBody)
	qs, ok := body["queryString"].(string)
	if !ok {
		t.Fatalf(`body["queryString"] missing or not a string: %v`, body["queryString"])
	}

	for _, want := range []string{
		`VALUES ?fdrid {"FEEDER123"}`,
		"COUNT(DISTINCT ?pec)",
		"?pec a c:PowerElectronicsConnection.",
		"?pec c:Equipment.EquipmentContainer ?fdr.",
	} {
		if !strings.Contains(qs, want) {
			t.Errorf("queryString missing %q\nfull queryString:\n%s", want, qs)
		}
	}

	for _, unwanted := range []string{
		"PowerElectronicsConnection.ratedS",
		"PowerElectronicsConnection.maxIFault",
		"Terminal.ConductingEquipment",
	} {
		if strings.Contains(qs, unwanted) {
			t.Errorf("queryString unexpectedly contains %q; QueryPECCount must not carry the mandatory attribute joins it exists to bypass", unwanted)
		}
	}

	if len(res.Results.Bindings) != 1 {
		t.Fatalf("len(Bindings) = %d, want 1", len(res.Results.Bindings))
	}
	if v := res.Results.Bindings[0]["count"].Value; v != "14" {
		t.Errorf(`count binding = %q, want "14"`, v)
	}
}

// TestQueryPECCountInvalidFeederID confirms QueryPECCount shares the same
// feederID validation as the other Query* wrappers (queryFeederTemplate).
func TestQueryPECCountInvalidFeederID(t *testing.T) {
	t.Parallel()

	mr := &mockRequester{resp: []byte(okEnvelope)}
	c := NewClient(mr)

	_, err := c.QueryPECCount(context.Background(), "")
	if !errors.Is(err, ErrInvalidFeederID) {
		t.Errorf("err = %v, want ErrInvalidFeederID", err)
	}
}
