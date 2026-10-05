package sender

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

const (
	// MaxRawBytes caps a raw-JSON message.
	MaxRawBytes = 16 * 1024
	// MaxForwardDifferences caps the forward differences in one message.
	MaxForwardDifferences = 16
	// maxMRIDBytes keeps difference_mrid inside what the outcome ring stores
	// unclipped, so the outcome lookup key is never truncated.
	maxMRIDBytes = 256

	msgPath = "input.message"
	fwdPath = msgPath + ".forward_differences"
	revPath = msgPath + ".reverse_differences"
)

// ValidationError is the first rule a raw message broke. Path is a JSON path
// from the document root, or "body" for a fault in the document as a whole.
type ValidationError struct {
	Path string
	Msg  string
}

func (e *ValidationError) Error() string { return e.Path + ": " + e.Msg }

func bad(path, format string, args ...any) *ValidationError {
	return &ValidationError{Path: path, Msg: fmt.Sprintf(format, args...)}
}

// parsedMessage is what a valid raw message carries into the audit trail.
type parsedMessage struct {
	DifferenceMRID string
	Forward        []diff.Difference
}

// validateRaw checks body against exactly what the input subscriber accepts,
// and refuses everything the subscriber would silently ignore. The final
// semantic check is sep2embed.ValidateControlDelta, the subscriber's own, run
// on the message decoded the way the subscriber decodes it.
func validateRaw(reg *registry.Registry, body []byte) (parsedMessage, error) {
	var out parsedMessage
	if len(body) == 0 {
		return out, bad("body", "empty")
	}
	if len(body) > MaxRawBytes {
		return out, bad("body", "%d bytes exceeds the %d byte limit", len(body), MaxRawBytes)
	}

	doc, err := decodeWithNumbers(body)
	if err != nil {
		return out, bad("body", "invalid JSON: %v", err)
	}
	if path, dup, err := firstDuplicateKey(body); err != nil {
		return out, bad("body", "invalid JSON: %v", err)
	} else if dup {
		return out, bad(path, "duplicate field")
	}
	top, ok := doc.(map[string]any)
	if !ok {
		return out, bad("body", "must be a JSON object")
	}
	if e := unknownKey(top, "", "command", "input"); e != nil {
		return out, e
	}
	if cmd, e := requireString(top, "", "command"); e != nil {
		return out, e
	} else if cmd != "update" {
		return out, bad("command", "%q is not \"update\"", cmd)
	}

	input, e := requireObject(top, "", "input")
	if e != nil {
		return out, e
	}
	if e := unknownKey(input, "input", "simulation_id", "message"); e != nil {
		return out, e
	}
	if _, present := input["simulation_id"]; present {
		if _, e := requireString(input, "input", "simulation_id"); e != nil {
			return out, e
		}
	}
	msg, e := requireObject(input, "input", "message")
	if e != nil {
		return out, e
	}
	if e := unknownKey(msg, msgPath, "timestamp", "difference_mrid", "reverse_differences", "forward_differences"); e != nil {
		return out, e
	}
	if ts, present := msg["timestamp"]; present {
		if n, isNum := ts.(json.Number); !isNum {
			return out, bad(msgPath+".timestamp", "must be an integer")
		} else if _, perr := n.Int64(); perr != nil {
			return out, bad(msgPath+".timestamp", "%s is not an integer", n)
		}
	}
	mrid, e := requireString(msg, msgPath, "difference_mrid")
	if e != nil {
		return out, e
	}
	if mrid == "" {
		return out, bad(msgPath+".difference_mrid", "must not be empty")
	}
	if len(mrid) > maxMRIDBytes {
		return out, bad(msgPath+".difference_mrid", "%d bytes exceeds the %d byte limit", len(mrid), maxMRIDBytes)
	}
	if e := checkReverse(msg); e != nil {
		return out, e
	}

	fwdRaw, present := msg["forward_differences"]
	if !present {
		return out, bad(fwdPath, "is required")
	}
	fwd, isArr := fwdRaw.([]any)
	if !isArr {
		return out, bad(fwdPath, "must be an array")
	}
	if len(fwd) < 1 || len(fwd) > MaxForwardDifferences {
		return out, bad(fwdPath, "holds %d entries, want 1 to %d", len(fwd), MaxForwardDifferences)
	}
	seen := make(map[[2]string]int, len(fwd))
	for i, item := range fwd {
		p := fmt.Sprintf("%s[%d]", fwdPath, i)
		obj, attr, e := checkDifference(item, p, true)
		if e != nil {
			return out, e
		}
		key := [2]string{obj, attr}
		if first, dup := seen[key]; dup {
			return out, bad(p, "repeats the object and attribute of forward_differences[%d]", first)
		}
		seen[key] = i
	}

	// The subscriber's own decode: the semantic check must see the same Go
	// values it will see, so numbers arrive as float64 here and not as
	// json.Number.
	var decoded diff.Message
	if err := json.Unmarshal(body, &decoded); err != nil {
		return out, bad("body", "does not decode as an input message: %v", err)
	}
	for i, d := range decoded.Input.Message.ForwardDifferences {
		if err := sep2embed.ValidateControlDelta(reg, d); err != nil {
			return out, bad(deltaErrPath(i, d, err), "%v", err)
		}
	}

	out.DifferenceMRID = mrid
	out.Forward = decoded.Input.Message.ForwardDifferences
	return out, nil
}

func decodeWithNumbers(body []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the first JSON value")
	}
	return v, nil
}

// firstDuplicateKey finds the first object key, in document order, that an
// object repeats, and returns its JSON path. The map view validateRaw checks
// keeps the last of two equal keys, while the subscriber's struct decode merges
// them, so a repeat would let the subscriber see fields validation never did.
func firstDuplicateKey(body []byte) (path string, dup bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	return scanForDuplicate(dec, "")
}

func scanForDuplicate(dec *json.Decoder, path string) (string, bool, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", false, err
	}
	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		return "", false, nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return "", false, err
			}
			key, _ := keyTok.(string)
			child := join(path, key)
			if _, again := seen[key]; again {
				return child, true, nil
			}
			seen[key] = struct{}{}
			if p, found, err := scanForDuplicate(dec, child); err != nil || found {
				return p, found, err
			}
		}
	case '[':
		for i := 0; dec.More(); i++ {
			if p, found, err := scanForDuplicate(dec, fmt.Sprintf("%s[%d]", path, i)); err != nil || found {
				return p, found, err
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return "", false, err
	}
	return "", false, nil
}

// deltaErrPath points at the member of forward_differences[i] the
// subscriber's validator refused. The sentinels name object and attribute. A
// power value is an object, and its decode errors name the offending member
// only in their text, so for those the text picks multiplier or value; any
// other refusal lands on the value as a whole.
func deltaErrPath(i int, d diff.Difference, err error) string {
	p := fmt.Sprintf("%s[%d]", fwdPath, i)
	switch {
	case errors.Is(err, sep2embed.ErrUnknownControlDevice):
		return p + ".object"
	case errors.Is(err, sep2embed.ErrUnsupportedControlAttribute):
		return p + ".attribute"
	}
	if _, isPower := d.Value.(map[string]any); isPower {
		if strings.Contains(err.Error(), "multiplier") {
			return p + ".value.multiplier"
		}
		return p + ".value.value"
	}
	return p + ".value"
}

func checkReverse(msg map[string]any) *ValidationError {
	raw, present := msg["reverse_differences"]
	if !present {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return bad(revPath, "must be an array")
	}
	for i, item := range arr {
		if _, _, e := checkDifference(item, fmt.Sprintf("%s[%d]", revPath, i), false); e != nil {
			return e
		}
	}
	return nil
}

// checkDifference validates one difference's shape and returns its object and
// attribute. A forward difference must carry a value; the subscriber ignores
// reverse differences, so only their shape is held.
func checkDifference(item any, path string, needValue bool) (object, attribute string, e *ValidationError) {
	m, ok := item.(map[string]any)
	if !ok {
		return "", "", bad(path, "must be an object")
	}
	if e := unknownKey(m, path, "object", "attribute", "value"); e != nil {
		return "", "", e
	}
	if object, e = requireString(m, path, "object"); e != nil {
		return "", "", e
	}
	if object == "" {
		return "", "", bad(path+".object", "must not be empty")
	}
	if attribute, e = requireString(m, path, "attribute"); e != nil {
		return "", "", e
	}
	if attribute == "" {
		return "", "", bad(path+".attribute", "must not be empty")
	}
	if _, has := m["value"]; needValue && !has {
		return "", "", bad(path+".value", "is required")
	}
	return object, attribute, nil
}

func join(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// unknownKey reports the alphabetically first key of m outside allowed, so the
// reported path does not depend on map iteration order.
func unknownKey(m map[string]any, parent string, allowed ...string) *ValidationError {
	var extra []string
	for k := range m {
		if !contains(allowed, k) {
			extra = append(extra, k)
		}
	}
	if len(extra) == 0 {
		return nil
	}
	sort.Strings(extra)
	return bad(join(parent, extra[0]), "unknown field")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func requireString(m map[string]any, parent, key string) (string, *ValidationError) {
	raw, present := m[key]
	if !present {
		return "", bad(join(parent, key), "is required")
	}
	s, ok := raw.(string)
	if !ok {
		return "", bad(join(parent, key), "must be a string")
	}
	return s, nil
}

func requireObject(m map[string]any, parent, key string) (map[string]any, *ValidationError) {
	raw, present := m[key]
	if !present {
		return nil, bad(join(parent, key), "is required")
	}
	o, ok := raw.(map[string]any)
	if !ok {
		return nil, bad(join(parent, key), "must be an object")
	}
	return o, nil
}
