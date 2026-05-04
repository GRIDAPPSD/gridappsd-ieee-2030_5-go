package diff

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrInvalidDifference is returned by AddDifference when Object or
// Attribute is empty.
var ErrInvalidDifference = errors.New("diff: invalid difference: object and attribute must be non-empty")

// Difference is one forward-or-reverse change to a CIM object's
// attribute. Value is held as any so callers can pass strings, numbers,
// or nested maps as the Python upstream does.
type Difference struct {
	Object    string `json:"object"`
	Attribute string `json:"attribute"`
	Value     any    `json:"value"`
}

// MessagePayload is the inner "message" object inside the wire envelope.
type MessagePayload struct {
	Timestamp          int64        `json:"timestamp"`
	DifferenceMRID     string       `json:"difference_mrid"`
	ReverseDifferences []Difference `json:"reverse_differences"`
	ForwardDifferences []Difference `json:"forward_differences"`
}

// Input is the input wrapper inside the wire envelope. SimulationID is
// a *string so it can be omitted when nil and present (even empty) when
// non-nil. Python upstream uses str|int|None; the Go bridge needs only
// strings, and empty string at NewBuilder is treated as None.
type Input struct {
	SimulationID *string        `json:"simulation_id,omitempty"`
	Message      MessagePayload `json:"message"`
}

// Message is the top-level wire envelope DifferenceBuilder.get_message()
// produces in Python. Marshaling Message to JSON yields the exact bytes
// the GridAPPS-D simulation input topic expects.
type Message struct {
	Command string `json:"command"`
	Input   Input  `json:"input"`
}

// Builder accumulates Differences and emits Messages. Not safe for
// concurrent use; one Builder per goroutine. Reuse across messages is
// intended (the bridge will typically construct one builder per
// downstream device and call Message repeatedly).
type Builder struct {
	simulationID string
	forward      []Difference
	reverse      []Difference
}

// NewBuilder returns a Builder. If simulationID is empty, the resulting
// messages will omit the simulation_id JSON field, matching the Python
// upstream's None semantics.
func NewBuilder(simulationID string) *Builder {
	return &Builder{
		simulationID: simulationID,
		forward:      []Difference{},
		reverse:      []Difference{},
	}
}

// AddDifference appends a forward and reverse pair to the builder. Empty
// Object or Attribute returns ErrInvalidDifference and the builder is
// not modified. Value semantics match the Python upstream: the passed
// reference is stored verbatim. Callers must not mutate the value after
// AddDifference returns.
func (b *Builder) AddDifference(object, attribute string, forward, reverse any) error {
	if object == "" || attribute == "" {
		return fmt.Errorf("AddDifference(%q, %q): %w", object, attribute, ErrInvalidDifference)
	}
	b.forward = append(b.forward, Difference{Object: object, Attribute: attribute, Value: forward})
	b.reverse = append(b.reverse, Difference{Object: object, Attribute: attribute, Value: reverse})
	return nil
}

// MustAddDifference is the fluent variant of AddDifference. It panics on
// validation failure. Use only when inputs are statically known to be
// valid (literals, hard-coded mRIDs); use AddDifference otherwise.
func (b *Builder) MustAddDifference(object, attribute string, forward, reverse any) *Builder {
	if err := b.AddDifference(object, attribute, forward, reverse); err != nil {
		panic(err)
	}
	return b
}

// Reset clears the accumulated diffs but keeps the simulation ID.
func (b *Builder) Reset() *Builder {
	b.forward = b.forward[:0]
	b.reverse = b.reverse[:0]
	return b
}

// Len returns the number of diff pairs accumulated.
func (b *Builder) Len() int {
	return len(b.forward)
}

// Message builds the wire envelope using the supplied Unix epoch. A
// fresh UUIDv4 difference_mrid is generated on each call (matching the
// Python upstream). The returned Message references the builder's
// internal slices for the diff arrays; do not mutate the builder
// concurrently with consumers of the returned Message. For independent
// copies, marshal to JSON via Bytes.
func (b *Builder) Message(epoch int64) *Message {
	msg := &Message{
		Command: "update",
		Input: Input{
			Message: MessagePayload{
				Timestamp:          epoch,
				DifferenceMRID:     newUUIDv4(),
				ReverseDifferences: b.reverse,
				ForwardDifferences: b.forward,
			},
		},
	}
	if b.simulationID != "" {
		s := b.simulationID
		msg.Input.SimulationID = &s
	}
	return msg
}

// MessageNow is Message with the current Unix epoch (UTC seconds).
func (b *Builder) MessageNow() *Message {
	return b.Message(time.Now().UTC().Unix())
}

// Bytes returns the JSON-encoded wire envelope at the supplied epoch.
// Each call generates a fresh difference_mrid.
func (b *Builder) Bytes(epoch int64) ([]byte, error) {
	out, err := json.Marshal(b.Message(epoch))
	if err != nil {
		return nil, fmt.Errorf("diff.Bytes: %w", err)
	}
	return out, nil
}

// BytesNow is Bytes with the current Unix epoch (UTC seconds).
func (b *Builder) BytesNow() ([]byte, error) {
	return b.Bytes(time.Now().UTC().Unix())
}

// newUUIDv4 returns a v4 UUID string built from crypto/rand. We avoid
// pulling in github.com/google/uuid; the standard library suffices.
//
// Layout (RFC 4122 section 4.4): 16 random bytes with two fixed bits in
// byte 6 (version=4) and byte 8 (variant=10).
func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read on Linux is documented as never failing in
		// practice (urandom backed). Surfacing the error here would
		// require AddDifference and Message to return errors, which
		// pollutes the API for an impossible case. Panic is the
		// pragmatic choice; fall-through behavior is undefined anyway.
		panic(fmt.Errorf("diff: crypto/rand failed: %w", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}
