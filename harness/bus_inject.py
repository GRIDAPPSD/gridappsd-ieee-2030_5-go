"""GAGO-094 live control-delta injector: the bus end of the loop.

This module publishes a real GridAPPS-D DifferenceBuilder envelope onto
the LIVE ActiveMQ simulation-input destination the bridge's
runControlSubscriber actually reads (cmd/bridge/main.go:1019-1023,
"dest := sim.InputTopic(simID)"). It is the entry point of the live
path that replaces the offline bridge_capture.json replay:

    bus delta (here) -> bridge runControlSubscriber -> ApplyControlDelta
    -> DERControl in the store -> 2030.5 client GET -> setpoint file
    -> HELICS publication -> OpenDSS DER

Envelope shape is NOT invented here. It matches internal/cim/diff's
Message exactly (internal/cim/diff/diff.go:32-64):

    {"command":"update",
     "input":{"simulation_id":"<id>",
              "message":{"timestamp":<epoch>,
                         "difference_mrid":"<uuidv4>",
                         "reverse_differences":[...],
                         "forward_differences":[
                            {"object":"<device mRID>",
                             "attribute":"DERControl.DERControlBase.opModTargetW",
                             "value":{"multiplier":<int8>,"value":<int16>}}]}}}

The bridge decodes this with encoding/json into diff.Message and applies
every entry of forward_differences (cmd/bridge/main.go:1030-1041). The
attribute prefix "DERControl.DERControlBase." is mandatory: any other
shape is refused with ErrUnsupportedControlAttribute
(internal/sep2embed/control.go:38, :132-135).

UNITS AND RANGE (the highest-risk surface, see coupling doc):
the sep2 ActivePower/ReactivePower wire type is an int8 multiplier plus
an INT16 value (internal/sep2embed/control.go:391, toInt16Checked), so
the physical quantity is value * 10^multiplier watts (or vars). A
plain 5000 W target does NOT fit as {"multiplier":0,"value":5000} in a
safe margin sense but it does fit int16 (max 32767); a 100 kW target
does not. encode_power below therefore picks the multiplier explicitly
and REFUSES rather than truncating when the mantissa will not fit
int16, because a silent truncation here is exactly the invisible
wrong-setpoint failure data-invariants Rule 2 forbids.
"""
from __future__ import annotations

import json
import time
import uuid
from dataclasses import dataclass

__all__ = [
    "PowerWire",
    "encode_power",
    "build_envelope",
    "input_topic",
    "DELTA_ATTR_TARGET_W",
    "DELTA_ATTR_TARGET_VAR",
]

# Attribute strings the bridge accepts. Prefix is load bearing:
# internal/sep2embed/control.go:38 derControlAttributePrefix.
DELTA_ATTR_TARGET_W = "DERControl.DERControlBase.opModTargetW"
DELTA_ATTR_TARGET_VAR = "DERControl.DERControlBase.opModTargetVar"

# int16 bounds on the sep2 ActivePower/ReactivePower mantissa
# (internal/sep2embed/control.go decodeActivePower -> toInt16Checked).
_INT16_MIN = -32768
_INT16_MAX = 32767


@dataclass(frozen=True)
class PowerWire:
    """One sep2 ActivePower/ReactivePower wire value.

    multiplier is the int8 power-of-ten exponent; value is the int16
    mantissa. The physical quantity is value * 10**multiplier, in watts
    for ActivePower and vars for ReactivePower.
    """

    multiplier: int
    value: int

    @property
    def physical(self) -> float:
        """The quantity in base SI units: watts for P, vars for Q."""
        return float(self.value) * (10.0**self.multiplier)

    def as_json(self) -> dict[str, int]:
        return {"multiplier": self.multiplier, "value": self.value}


def encode_power(quantity: float) -> PowerWire:
    """Encode a base-SI quantity (watts or vars) as a sep2 multiplier and
    int16 mantissa, exactly.

    Raises ValueError when the quantity cannot be represented exactly in
    the int8-multiplier / int16-mantissa pair. Refusing beats truncating:
    a truncated mantissa would command a physically different setpoint
    than the caller asked for, and nothing downstream could detect it
    (data-invariants Rule 2: no synthesized wrong value).
    """
    if quantity != round(quantity):
        raise ValueError(
            f"encode_power({quantity!r}): non-integer base-SI quantities are "
            f"not supported; the sep2 mantissa is an integer"
        )
    q = int(round(quantity))
    if q == 0:
        return PowerWire(multiplier=0, value=0)

    # Raise the multiplier only as far as needed to fit int16, and only
    # while the value stays exactly divisible: an inexact shift would
    # silently change the commanded power.
    multiplier = 0
    mantissa = q
    while not (_INT16_MIN <= mantissa <= _INT16_MAX):
        if mantissa % 10 != 0:
            raise ValueError(
                f"encode_power({quantity!r}): cannot represent exactly; "
                f"mantissa {mantissa} exceeds int16 and is not divisible by 10"
            )
        mantissa //= 10
        multiplier += 1
        if multiplier > 127:
            raise ValueError(f"encode_power({quantity!r}): multiplier overflows int8")

    wire = PowerWire(multiplier=multiplier, value=mantissa)
    # Round-trip assertion: the encoded pair must reproduce the requested
    # quantity. This is the guard against a 10x/1000x slip introduced
    # here rather than downstream.
    if wire.physical != float(q):
        raise ValueError(
            f"encode_power({quantity!r}): round trip failed, encoded to "
            f"{wire.physical} (multiplier={wire.multiplier} value={wire.value})"
        )
    return wire


def input_topic(sim_id: str) -> str:
    """The destination runControlSubscriber subscribes to.

    Mirrors internal/cim/sim/topics.go:29-31 (InputTopic) with the
    "/topic/" STOMP destination prefix ActiveMQ requires.
    """
    if not sim_id:
        raise ValueError("input_topic: sim_id must be non-empty")
    return f"/topic/goss.gridappsd.simulation.input.{sim_id}"


def build_envelope(
    sim_id: str,
    device_mrid: str,
    attribute: str,
    forward: PowerWire,
    reverse: PowerWire | None = None,
    epoch: int | None = None,
    difference_mrid: str | None = None,
) -> dict[str, object]:
    """Build the diff.Message wire envelope for one control delta.

    reverse defaults to a zero-valued power of the same multiplier, which
    is what a DifferenceBuilder caller reverting to no-target would emit.
    The bridge ignores reverse_differences entirely
    (cmd/bridge/main.go:1030, only ForwardDifferences is iterated), but
    the field is present and well formed because the envelope contract is
    the platform's, not this harness's: emitting a shape the platform
    would reject would make the test prove less than it appears to.
    """
    if not device_mrid:
        raise ValueError("build_envelope: device_mrid must be non-empty")
    if not attribute.startswith("DERControl.DERControlBase."):
        raise ValueError(
            f"build_envelope: attribute {attribute!r} lacks the "
            f"'DERControl.DERControlBase.' prefix the bridge requires "
            f"(internal/sep2embed/control.go:38)"
        )
    if reverse is None:
        reverse = PowerWire(multiplier=forward.multiplier, value=0)

    return {
        "command": "update",
        "input": {
            "simulation_id": sim_id,
            "message": {
                "timestamp": int(time.time()) if epoch is None else epoch,
                "difference_mrid": difference_mrid or str(uuid.uuid4()),
                "reverse_differences": [
                    {
                        "object": device_mrid,
                        "attribute": attribute,
                        "value": reverse.as_json(),
                    }
                ],
                "forward_differences": [
                    {
                        "object": device_mrid,
                        "attribute": attribute,
                        "value": forward.as_json(),
                    }
                ],
            },
        },
    }


def envelope_bytes(**kwargs: object) -> bytes:
    """build_envelope serialized to the exact bytes put on the wire."""
    return json.dumps(build_envelope(**kwargs)).encode("utf-8")  # type: ignore[arg-type]
