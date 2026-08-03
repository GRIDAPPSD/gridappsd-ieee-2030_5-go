package telemetrypub

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// ContentTypeJSON is the content type stamped on every message this
// package's built-in builder produces. diff.Builder always emits JSON.
const ContentTypeJSON = "application/json"

// ErrNoContent is returned by a MessageBuilder when the devices it was
// handed carry nothing publishable (every one of them maps to zero
// values). It is not a failure: the publisher treats it as "there is
// genuinely nothing to send this interval" and sends no envelope, rather
// than putting an empty one on the bus.
var ErrNoContent = errors.New("telemetrypub: nothing to publish")

// Message is one interval's built payload: the exact bytes to put on the
// bus, plus the content type to stamp them with.
type Message struct {
	ContentType string
	Body        []byte
}

// MessageBuilder turns one interval's device statuses into a single
// aggregate Message.
//
// THIS IS THE MESSAGE-SHAPE SEAM. The bridge currently publishes the
// internal/cim/diff envelope (DiffMessageBuilder below), matching what
// the per-PUT relay published before GAGO-121. The agreed eventual
// target is a CIM AnalogValue payload, as the Python upstream's
// get_message_for_bus builds, which is deferred pending confirmation of
// who actually subscribes. Swapping shapes means writing a second
// MessageBuilder and passing it as Config.Build; nothing in Publisher
// knows the shape, and nothing else in this package needs to change.
// Change the paired Fingerprinter (Config.Fingerprint) at the same time,
// since "unchanged" is defined in terms of what is actually published.
type MessageBuilder func(devices []sep2embed.DERStatusSnapshot, now time.Time) (Message, error)

// Fingerprinter renders one device's publishable content as a
// comparable string. Two snapshots that would publish identical values
// must fingerprint equal, and two that would publish anything different
// (including for different devices) must not.
//
// It is paired with MessageBuilder: "unchanged" means "would publish the
// same thing", so a new message shape needs a matching fingerprint or
// suppression silently starts measuring the wrong thing.
type Fingerprinter func(device sep2embed.DERStatusSnapshot) (string, error)

// DiffMessageBuilder returns the MessageBuilder that produces the
// internal/cim/diff envelope this bridge publishes today: one envelope
// per interval, carrying every device's mapped differences under that
// device's own CIM mRID, stamped with simulationID and the interval's
// timestamp.
//
// Per-device content is identical to what the removed per-PUT relay
// produced for the same DERStatus (pinned by
// TestDiffMessageBuilderPerDeviceContentMatchesThePerPUTPath): same
// mapping, same forward-equals-reverse pairing, same envelope fields.
// Only the batching changed, from one envelope per PUT to one envelope
// per interval covering every changed device.
//
// A device that maps to no differences contributes nothing rather than
// failing the whole batch: one silent device must not suppress its
// noisy siblings. When NO device contributes anything, ErrNoContent is
// returned so the caller can decline to send an empty envelope.
func DiffMessageBuilder(simulationID string) MessageBuilder {
	return func(devices []sep2embed.DERStatusSnapshot, now time.Time) (Message, error) {
		b := diff.NewBuilder(simulationID)
		for _, device := range devices {
			diffs, err := MapDERStatusToDifferences(device.MRID, device.Status)
			if err != nil {
				return Message{}, fmt.Errorf("telemetrypub: map edev=%s der=%s: %w", device.EDevID, device.DERID, err)
			}
			for _, d := range diffs {
				// Reverse equals forward: a status report is an
				// observation, not a revertible command. See
				// MapDERStatusToDifferences's doc comment.
				if err := b.AddDifference(d.Object, d.Attribute, d.Value, d.Value); err != nil {
					return Message{}, fmt.Errorf("telemetrypub: build envelope for mrid=%s: %w", device.MRID, err)
				}
			}
		}

		if b.Len() == 0 {
			return Message{}, ErrNoContent
		}

		body, err := b.Bytes(now.UTC().Unix())
		if err != nil {
			return Message{}, fmt.Errorf("telemetrypub: encode envelope: %w", err)
		}
		return Message{ContentType: ContentTypeJSON, Body: body}, nil
	}
}

// DiffFingerprint is the Fingerprinter paired with DiffMessageBuilder:
// it renders exactly the differences that would be published for one
// device, so a change that the mapping drops (a per-field dateTime, for
// instance) is correctly seen as no change at all, and a change to any
// published value is always seen.
//
// The device's mRID is part of every rendered difference (it is each
// difference's Object), so two devices reporting identical values never
// share a fingerprint.
func DiffFingerprint(device sep2embed.DERStatusSnapshot) (string, error) {
	diffs, err := MapDERStatusToDifferences(device.MRID, device.Status)
	if err != nil {
		return "", fmt.Errorf("telemetrypub: fingerprint edev=%s der=%s: %w", device.EDevID, device.DERID, err)
	}
	// json.Marshal over a slice of structs with concrete field types is
	// deterministic for a given input (no map iteration is involved), so
	// it is a stable rendering rather than merely a convenient one.
	encoded, err := json.Marshal(diffs)
	if err != nil {
		return "", fmt.Errorf("telemetrypub: fingerprint mrid=%s: %w", device.MRID, err)
	}
	return string(encoded), nil
}
