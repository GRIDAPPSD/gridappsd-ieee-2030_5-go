package cim

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// maxErrorText bounds the platform error text carried into an error, so
// a hostile or runaway response cannot flood logs.
const maxErrorText = 512

// Requester is the minimal STOMP request/reply primitive this package
// needs. *cimstomp.Client satisfies it. The interface is declared on
// the consumer side per Go convention so cim does not import cimstomp,
// keeping the dependency one-directional.
type Requester interface {
	Request(ctx context.Context, destination string, body []byte) ([]byte, error)
}

// Client builds JSON request envelopes, calls Requester.Request, and
// parses the response into typed Go structs. It holds no state beyond
// the Requester reference; concurrency safety is delegated to the
// underlying Requester.
type Client struct {
	r Requester
}

// NewClient returns a Client backed by the given Requester. Passing nil
// is allowed at construction; calls will panic with a descriptive
// message if the field is dereferenced. We do not return an error here
// to keep the constructor allocation-only and consistent with similar
// Go wrapper packages.
func NewClient(r Requester) *Client {
	return &Client{r: r}
}

// envelopeMeta is the outer wrapper most CIM responses share:
// {"data":..., "responseComplete":true, "id":"..."} plus the optional
// "error" field. Only error and responseComplete are decoded eagerly;
// data is left as RawMessage for the per-call typed decode step.
type envelopeMeta struct {
	Error            envelopeError   `json:"error,omitempty"`
	ResponseComplete *bool           `json:"responseComplete,omitempty"`
	Data             json.RawMessage `json:"data,omitempty"`
}

// envelopeError is the "error" member of a response envelope. The
// platform sends either a string or an object; null and absent both mean
// no error, and so does an empty string. An object or any other
// non-string value always counts as an error, even when it carries no
// message.
type envelopeError struct {
	set  bool
	text string
}

// UnmarshalJSON accepts any JSON value; it never fails, so a new error
// shape surfaces as a server error rather than a decode failure.
func (e *envelopeError) UnmarshalJSON(b []byte) error {
	// A repeated "error" member never replaces the first one that set it.
	if e.set {
		return nil
	}
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(b, &s) == nil {
		if s != "" {
			*e = envelopeError{set: true, text: boundText(s)}
		}
		return nil
	}
	*e = envelopeError{set: true, text: describeErrorValue(b)}
	return nil
}

// messageKeys are the object members shown as the error text, in order.
// The platform's object shape is not documented; when none of these is
// present the object's key names are shown, so the first real failure
// says which key to add.
var messageKeys = []string{"message", "msg", "detail", "reason", "description", "error"}

const (
	maxErrorDepth   = 3
	maxErrorKeys    = 8
	maxErrorKeyName = 32
)

// describeErrorValue renders a non-string error value by position, never
// by content: an object shows only a message-like string member (or the
// sorted key names), a scalar shows its literal, and an array shows its
// length. No other value is rendered, so a credential under any other
// member never reaches the text.
func describeErrorValue(b []byte) string {
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) == 0 {
		return "unrecognized error value"
	}
	switch b[0] {
	case '{':
		return describeErrorObject(b, 1)
	case '[':
		var elems []json.RawMessage
		if json.Unmarshal(b, &elems) != nil {
			return "unrecognized error value"
		}
		return fmt.Sprintf("array error, %d elements", len(elems))
	case 't', 'f':
		if string(b) == "true" || string(b) == "false" {
			return string(b)
		}
	default:
		var n json.Number
		if json.Unmarshal(b, &n) == nil {
			return boundText(n.String())
		}
	}
	return "unrecognized error value"
}

// describeErrorObject applies the object rule at the given depth
// (1 for the outermost object).
func describeErrorObject(b []byte, depth int) string {
	var members map[string]json.RawMessage
	if json.Unmarshal(b, &members) != nil {
		return "unrecognized error value"
	}
	for _, k := range messageKeys {
		var m string
		if raw, ok := members[k]; ok && json.Unmarshal(raw, &m) == nil && m != "" {
			return boundText(m)
		}
	}
	if raw, ok := members["error"]; ok && depth < maxErrorDepth && len(raw) > 0 && raw[0] == '{' {
		return describeErrorObject(raw, depth+1)
	}
	names := make([]string, 0, len(members))
	for k := range members {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) > maxErrorKeys {
		names = names[:maxErrorKeys]
	}
	for i, k := range names {
		names[i] = fmt.Sprintf("%q", cutString(k, maxErrorKeyName))
	}
	if len(names) == 0 {
		return "object error, no keys"
	}
	return boundText("object error, keys: " + strings.Join(names, ", "))
}

// boundText truncates s to maxErrorText bytes on a rune boundary.
func boundText(s string) string {
	if len(s) <= maxErrorText {
		return s
	}
	return cutString(s, maxErrorText) + "..."
}

// cutString truncates s to at most n bytes on a rune boundary.
func cutString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// decodeEnvelope inspects the standard CIM response envelope. It
// returns the inner Data bytes on success, or a wrapped sentinel error
// on a server-error or incomplete-response envelope. checkComplete
// controls whether responseComplete=false is treated as an error;
// platform-status and config responses do not carry that field.
func decodeEnvelope(raw []byte, checkComplete bool) (json.RawMessage, error) {
	var env envelopeMeta
	// A type error in another field must not hide a present error member:
	// json.Unmarshal still fills the fields it could decode.
	if err := json.Unmarshal(raw, &env); err != nil && !env.Error.set {
		return nil, fmt.Errorf("cim: decode envelope: %w", err)
	}
	if env.Error.set {
		return nil, fmt.Errorf("%w: %s", ErrServerError, env.Error.text)
	}
	if checkComplete && env.ResponseComplete != nil && !*env.ResponseComplete {
		return nil, fmt.Errorf("%w", ErrIncompleteResponse)
	}
	return env.Data, nil
}

// callEnveloped issues a request whose response is wrapped in the
// standard {"data":...} envelope, decodes the envelope, and unmarshals
// data into T. checkComplete=true rejects responseComplete=false with
// ErrIncompleteResponse. The opName is the public method name used in
// error wrapping so callers can read it directly.
func callEnveloped[T any](ctx context.Context, c *Client, opName, destination string, req map[string]any, checkComplete bool) (*T, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("cim.%s: marshal request: %w", opName, err)
	}

	raw, err := c.r.Request(ctx, destination, body)
	if err != nil {
		return nil, fmt.Errorf("cim.%s: %w", opName, err)
	}

	data, err := decodeEnvelope(raw, checkComplete)
	if err != nil {
		return nil, fmt.Errorf("cim.%s: %w", opName, err)
	}

	var res T
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("cim.%s: decode result: %w", opName, err)
	}
	return &res, nil
}

// QueryData issues a SPARQL QUERY against the powergrid-model service.
// The result is the parsed bindings. Multi-frame responses
// (responseComplete=false) surface as ErrIncompleteResponse; the bridge
// does not consume streaming results in v0.
func (c *Client) QueryData(ctx context.Context, sparql string) (*QueryDataResult, error) {
	return callEnveloped[QueryDataResult](ctx, c, "QueryData", RequestPowergridModel, map[string]any{
		"requestType":  "QUERY",
		"resultFormat": "JSON",
		"queryString":  sparql,
	}, true)
}

// QueryModelInfo lists the CIM models registered with the platform.
func (c *Client) QueryModelInfo(ctx context.Context) (*ModelInfoResult, error) {
	return callEnveloped[ModelInfoResult](ctx, c, "QueryModelInfo", RequestPowergridModel, map[string]any{
		"requestType":  "QUERY_MODEL_INFO",
		"resultFormat": "JSON",
	}, false)
}

// QueryObjectDict returns the dictionary of objects of a given type for
// a model. The response shape varies by objectType, so the result is
// surfaced as a map; callers decode further as needed.
func (c *Client) QueryObjectDict(ctx context.Context, modelID, objectType string) (*ObjectDictResult, error) {
	return callEnveloped[ObjectDictResult](ctx, c, "QueryObjectDict", RequestPowergridModel, map[string]any{
		"requestType":  "QUERY_OBJECT_DICT",
		"resultFormat": "JSON",
		"modelId":      modelID,
		"objectType":   objectType,
	}, false)
}

// GetCIMDictionary fetches the CIM Dictionary feeder summary for a
// model. This is a denormalized blob the bridge uses for DER and
// measurement enumeration; it is distinct from raw CIM SPARQL output.
func (c *Client) GetCIMDictionary(ctx context.Context, modelID string) (*CIMDictionary, error) {
	return callEnveloped[CIMDictionary](ctx, c, "GetCIMDictionary", RequestConfig, map[string]any{
		"configurationType": "CIM Dictionary",
		"parameters": map[string]any{
			"model_id": modelID,
		},
	}, false)
}

// GetPlatformStatus returns a diagnostic snapshot of the platform's
// registered apps, services, and instance counts. The fields are
// platform-defined raw JSON and are not consumed by the bridge for
// behavior; v0 keeps them as json.RawMessage so upstream changes do not
// break the wrapper.
func (c *Client) GetPlatformStatus(ctx context.Context) (*PlatformStatus, error) {
	body, err := json.Marshal(map[string]any{
		"applications":     true,
		"services":         true,
		"appInstances":     true,
		"serviceInstances": true,
	})
	if err != nil {
		return nil, fmt.Errorf("cim.GetPlatformStatus: marshal request: %w", err)
	}

	raw, err := c.r.Request(ctx, RequestPlatformStatus, body)
	if err != nil {
		return nil, fmt.Errorf("cim.GetPlatformStatus: %w", err)
	}

	// Platform status responses do not wrap in a "data" envelope per the
	// catalog; check for an error key, then decode the body directly.
	var probe envelopeMeta
	_ = json.Unmarshal(raw, &probe) // a type error elsewhere must not hide the error member
	if probe.Error.set {
		return nil, fmt.Errorf("cim.GetPlatformStatus: %w: %s", ErrServerError, probe.Error.text)
	}

	var res PlatformStatus
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("cim.GetPlatformStatus: decode result: %w", err)
	}
	return &res, nil
}
