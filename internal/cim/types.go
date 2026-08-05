package cim

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Destination constants for the GridAPPS-D request queues exercised by
// this package. Bare destinations (no /queue/ prefix); the underlying
// Requester is responsible for any prefix normalization.
const (
	// RequestPowergridModel is the queue for all CIM model queries:
	// SPARQL via QUERY, plus QUERY_MODEL_INFO, QUERY_OBJECT_DICT, etc.
	RequestPowergridModel = "goss.gridappsd.process.request.data.powergridmodel"

	// RequestConfig is the queue for configuration blob fetches such as
	// the CIM Dictionary.
	RequestConfig = "goss.gridappsd.process.request.config"

	// RequestPlatformStatus is the queue for platform status: registered
	// applications, services, and instance counts.
	RequestPlatformStatus = "goss.gridappsd.process.request.status.platform"
)

// Sentinel errors returned by this package.
var (
	// ErrIncompleteResponse is returned when the server signals
	// responseComplete=false on a SPARQL data response. The Python
	// upstream treats responses as single-shot; this package surfaces
	// the multi-frame case as an error rather than silently returning
	// partial results. See catalog section 3.
	ErrIncompleteResponse = errors.New("cim: incomplete response from broker")

	// ErrServerError is returned when the response payload contains an
	// "error" key. The error string is wrapped into the returned error
	// so callers can read it, while still allowing errors.Is checks.
	ErrServerError = errors.New("cim: server returned error envelope")
)

// SPARQLHead carries the variable list returned by a SPARQL query.
type SPARQLHead struct {
	Vars []string `json:"vars"`
}

// SPARQLResults wraps the bindings returned by a SPARQL query. Each
// element of Bindings is one solution row mapping variable name to its
// Binding.
type SPARQLResults struct {
	Bindings []map[string]Binding `json:"bindings"`
}

// Binding is one variable's value within a SPARQL solution row. The
// Type field is "literal" or "uri" depending on the SPARQL term kind;
// some platform-emitted bindings omit it. DataType and Lang are XML
// Schema annotations the platform sometimes attaches.
//
// For bindings whose Value is itself a JSON-LD encoded string, callers
// should use AsJSONLD to double-decode rather than parsing Value
// directly. See catalog section 3 plus resolution #4.
type Binding struct {
	Type     string `json:"type,omitempty"`
	Value    string `json:"value"`
	DataType string `json:"datatype,omitempty"`
	Lang     string `json:"xml:lang,omitempty"`
}

// AsString returns the raw Value verbatim. It exists for symmetry with
// AsJSONLD; callers that want the string need not reach into the field.
func (b Binding) AsString() string {
	return b.Value
}

// AsJSONLD parses Value as JSON and returns the resulting object. It is
// intended for SPARQL bindings whose value field is itself a JSON-LD
// encoded string, e.g. {"@id":"_abc","@type":"PowerElectronicsConnection"}.
// If Value is not valid JSON, AsJSONLD returns a wrapped error.
func (b Binding) AsJSONLD() (map[string]any, error) {
	if b.Value == "" {
		return nil, fmt.Errorf("Binding.AsJSONLD: empty value")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(b.Value), &out); err != nil {
		return nil, fmt.Errorf("Binding.AsJSONLD: %w", err)
	}
	return out, nil
}

// QueryDataResult is the parsed payload of a QUERY (SPARQL) response.
type QueryDataResult struct {
	Head    SPARQLHead    `json:"head"`
	Results SPARQLResults `json:"results"`
}

// Model is one entry of the model-info response.
type Model struct {
	ModelID       string `json:"modelId"`
	ModelName     string `json:"modelName"`
	StationName   string `json:"stationName"`
	SubRegionName string `json:"subRegionName"`
	RegionName    string `json:"regionName"`
}

// ModelInfoResult is the parsed payload of a QUERY_MODEL_INFO response.
type ModelInfoResult struct {
	Models []Model `json:"models"`
}

// ObjectDictResult is the parsed payload of a QUERY_OBJECT_DICT
// response. The schema is server-defined and varies by objectType, so
// v0 surfaces it as a generic map. Callers that need typed access
// should switch on the objectType they requested and decode further.
type ObjectDictResult struct {
	Data map[string]any `json:"-"`
}

// UnmarshalJSON captures the entire object-dict payload into Data.
// QueryObjectDict already strips the outer {"data":...} envelope before
// invoking this, so the bytes here are the inner object directly.
func (r *ObjectDictResult) UnmarshalJSON(b []byte) error {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	r.Data = m
	return nil
}

// DictItem is one entry within a CIM Dictionary feeder summary list.
// Common fields are typed; additional server-emitted fields land in
// Extra to keep parsing forward-compatible.
type DictItem struct {
	MRID   string         `json:"mRID"`
	Name   string         `json:"name"`
	Phases string         `json:"phases,omitempty"`
	Extra  map[string]any `json:"-"`
}

// UnmarshalJSON captures all fields into a map then re-extracts the
// typed ones. The remaining fields are kept in Extra so the caller can
// access them without losing type safety on the common ones.
func (d *DictItem) UnmarshalJSON(b []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if v, ok := raw["mRID"].(string); ok {
		d.MRID = v
	}
	if v, ok := raw["name"].(string); ok {
		d.Name = v
	}
	if v, ok := raw["phases"].(string); ok {
		d.Phases = v
	}
	delete(raw, "mRID")
	delete(raw, "name")
	delete(raw, "phases")
	if len(raw) > 0 {
		d.Extra = raw
	}
	return nil
}

// Feeder is one feeder entry in the CIM Dictionary. The bridge needs
// EnergyConsumers and Measurements at minimum; SolarPanels and
// Batteries are required for DER enumeration. Other fields are kept so
// downstream code can grow without a types-package change.
type Feeder struct {
	EnergyConsumers []DictItem `json:"energyconsumers,omitempty"`
	Capacitors      []DictItem `json:"capacitors,omitempty"`
	Regulators      []DictItem `json:"regulators,omitempty"`
	Switches        []DictItem `json:"switches,omitempty"`
	Measurements    []DictItem `json:"measurements,omitempty"`
	SolarPanels     []DictItem `json:"solarpanels,omitempty"`
	Batteries       []DictItem `json:"batteries,omitempty"`
}

// CIMDictionary is the parsed payload of a config-type "CIM Dictionary"
// response.
type CIMDictionary struct {
	Feeders []Feeder `json:"feeders"`
}

// PlatformStatus is the parsed payload of a platform-status response.
// Field shapes are platform-defined and not consumed by the bridge for
// behavior, only diagnostics. Decoded as raw JSON so changes upstream
// do not break the wrapper.
type PlatformStatus struct {
	Applications     json.RawMessage `json:"applications,omitempty"`
	Services         json.RawMessage `json:"services,omitempty"`
	AppInstances     json.RawMessage `json:"appInstances,omitempty"`
	ServiceInstances json.RawMessage `json:"serviceInstances,omitempty"`
}
