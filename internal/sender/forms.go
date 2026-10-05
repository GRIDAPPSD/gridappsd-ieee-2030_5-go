package sender

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
)

// The attributes the input subscriber acts on.
const (
	AttrActivePower   = "DERControl.DERControlBase.opModTargetW"
	AttrReactivePower = "DERControl.DERControlBase.opModTargetVar"
	AttrConnect       = "DERControl.DERControlBase.opModConnect"
	AttrEnergize      = "DERControl.DERControlBase.opModEnergize"
)

// Form bounds: the power multiplier is a power of ten, and the value is the
// int16 the 2030.5 ActivePower and ReactivePower types carry.
const (
	MinMultiplier = -9
	MaxMultiplier = 9
)

// ErrFieldRange is returned by a form builder for a field outside its bound.
var ErrFieldRange = errors.New("sender: form field out of range")

// ActivePowerMessage builds the message that sets a device's active power
// target to value x 10^multiplier watts.
func ActivePowerMessage(now time.Time, deviceMRID string, multiplier, value int) (*diff.Message, error) {
	return powerMessage(now, deviceMRID, AttrActivePower, multiplier, value)
}

// ReactivePowerMessage is ActivePowerMessage for the reactive target, in var.
func ReactivePowerMessage(now time.Time, deviceMRID string, multiplier, value int) (*diff.Message, error) {
	return powerMessage(now, deviceMRID, AttrReactivePower, multiplier, value)
}

// ConnectMessage builds the message that connects or disconnects a device.
func ConnectMessage(now time.Time, deviceMRID string, connect bool) (*diff.Message, error) {
	return newMessage(now, deviceMRID, AttrConnect, connect)
}

// EnergizeMessage builds the message that energizes or de-energizes a device.
func EnergizeMessage(now time.Time, deviceMRID string, energize bool) (*diff.Message, error) {
	return newMessage(now, deviceMRID, AttrEnergize, energize)
}

func powerMessage(now time.Time, deviceMRID, attribute string, multiplier, value int) (*diff.Message, error) {
	if multiplier < MinMultiplier || multiplier > MaxMultiplier {
		return nil, fmt.Errorf("%w: multiplier %d, want %d to %d", ErrFieldRange, multiplier, MinMultiplier, MaxMultiplier)
	}
	if value < math.MinInt16 || value > math.MaxInt16 {
		return nil, fmt.Errorf("%w: value %d, want %d to %d", ErrFieldRange, value, math.MinInt16, math.MaxInt16)
	}
	return newMessage(now, deviceMRID, attribute, map[string]any{"multiplier": multiplier, "value": value})
}

// newMessage wraps one forward difference in the envelope the subscriber
// reads. reverse_differences is an empty array, not null, and no
// simulation_id is set: the topic carries none.
func newMessage(now time.Time, deviceMRID, attribute string, value any) (*diff.Message, error) {
	if deviceMRID == "" {
		return nil, fmt.Errorf("%w: device mRID is empty", ErrFieldRange)
	}
	msg, err := diff.NewBuilder("").Message(now.UTC().Unix())
	if err != nil {
		return nil, err
	}
	msg.Input.Message.ForwardDifferences = []diff.Difference{{Object: deviceMRID, Attribute: attribute, Value: value}}
	msg.Input.Message.ReverseDifferences = []diff.Difference{}
	return msg, nil
}
