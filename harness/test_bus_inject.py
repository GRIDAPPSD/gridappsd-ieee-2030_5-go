"""Unit tests for the live control-delta injector.

These assert the WIRE BYTES and the encoded FIELD VALUES against what the
bridge's Go parser actually accepts, not merely that a dict was built.
Per data-invariants Rule 1, an envelope test that only proves "no
exception" would pass while shipping a shape the bridge silently refuses,
or a mantissa that means a different physical power than requested.

Run:
    <venv>/bin/python -m unittest test_bus_inject -v
"""
from __future__ import annotations

import json
import unittest

import bus_inject as bi


class TestEncodePower(unittest.TestCase):
    def test_5000_watts_encodes_exactly(self):
        w = bi.encode_power(5000.0)
        self.assertEqual(w.multiplier, 0)
        self.assertEqual(w.value, 5000)
        self.assertEqual(w.physical, 5000.0)

    def test_value_stays_within_int16(self):
        # internal/sep2embed/control.go decodeActivePower narrows the
        # mantissa via toInt16Checked; a mantissa outside int16 would be
        # refused by the bridge, so the encoder must shift the multiplier.
        w = bi.encode_power(100000.0)
        self.assertTrue(-32768 <= w.value <= 32767)
        self.assertEqual(w.physical, 100000.0)
        self.assertEqual(w.multiplier, 1)
        self.assertEqual(w.value, 10000)

    def test_negative_power_encodes_with_sign_preserved(self):
        w = bi.encode_power(-5000.0)
        self.assertEqual(w.value, -5000)
        self.assertEqual(w.physical, -5000.0)

    def test_zero(self):
        w = bi.encode_power(0.0)
        self.assertEqual((w.multiplier, w.value), (0, 0))
        self.assertEqual(w.physical, 0.0)

    def test_inexact_quantity_refused_not_truncated(self):
        # A value that cannot be represented exactly must be refused. A
        # truncated mantissa commands a physically different setpoint with
        # no downstream symptom (data-invariants Rule 2).
        with self.assertRaises(ValueError):
            bi.encode_power(1234.5)

    def test_unrepresentable_large_value_refused(self):
        # 327671 exceeds int16 and is not divisible by 10, so no exact
        # (multiplier, mantissa) pair exists. Refuse rather than round.
        with self.assertRaises(ValueError):
            bi.encode_power(327671.0)

    def test_multiplier_shift_never_loses_magnitude(self):
        for q in [40000.0, 100000.0, 1000000.0, -80000.0]:
            with self.subTest(q=q):
                w = bi.encode_power(q)
                self.assertEqual(
                    w.physical, q, f"round trip changed the commanded power for {q}"
                )


class TestEnvelopeShape(unittest.TestCase):
    """The envelope must match internal/cim/diff/diff.go:32-64 exactly."""

    def setUp(self):
        self.env = bi.build_envelope(
            sim_id="gago094live",
            device_mrid="50B15A48-9611-40DF-983E-93679DA72871",
            attribute=bi.DELTA_ATTR_TARGET_W,
            forward=bi.encode_power(5000.0),
            epoch=1700000000,
            difference_mrid="11111111-2222-4333-8444-555555555555",
        )

    def test_top_level_keys_match_diff_message(self):
        self.assertEqual(self.env["command"], "update")
        self.assertIn("input", self.env)

    def test_simulation_id_present_under_input(self):
        self.assertEqual(self.env["input"]["simulation_id"], "gago094live")

    def test_message_payload_fields(self):
        msg = self.env["input"]["message"]
        self.assertEqual(msg["timestamp"], 1700000000)
        self.assertEqual(msg["difference_mrid"], "11111111-2222-4333-8444-555555555555")
        self.assertIn("forward_differences", msg)
        self.assertIn("reverse_differences", msg)

    def test_forward_difference_is_what_apply_control_delta_accepts(self):
        fwd = self.env["input"]["message"]["forward_differences"]
        self.assertEqual(len(fwd), 1)
        d = fwd[0]
        # runControlSubscriber iterates ForwardDifferences only
        # (cmd/bridge/main.go:1030) and ApplyControlDelta requires the
        # "DERControl.DERControlBase." prefix (control.go:38, :132-135).
        self.assertEqual(d["attribute"], "DERControl.DERControlBase.opModTargetW")
        self.assertTrue(d["attribute"].startswith("DERControl.DERControlBase."))
        self.assertEqual(d["object"], "50B15A48-9611-40DF-983E-93679DA72871")
        # decodeActivePower's map branch requires BOTH keys
        # (control.go:434-442: missing either is an error).
        self.assertEqual(set(d["value"].keys()), {"multiplier", "value"})
        self.assertEqual(d["value"]["multiplier"], 0)
        self.assertEqual(d["value"]["value"], 5000)

    def test_reverse_defaults_to_zero_same_multiplier(self):
        rev = self.env["input"]["message"]["reverse_differences"][0]
        self.assertEqual(rev["value"], {"multiplier": 0, "value": 0})

    def test_empty_lists_are_lists_not_null(self):
        # An empty collection and a missing field are different wire
        # signals; the Go side unmarshals []Difference.
        msg = self.env["input"]["message"]
        self.assertIsInstance(msg["forward_differences"], list)
        self.assertIsInstance(msg["reverse_differences"], list)

    def test_serialized_bytes_are_valid_json_and_round_trip(self):
        raw = json.dumps(self.env).encode("utf-8")
        back = json.loads(raw)
        self.assertEqual(
            back["input"]["message"]["forward_differences"][0]["value"]["value"], 5000
        )

    def test_bad_attribute_prefix_refused_at_build_time(self):
        # Better to fail here than to publish a frame the bridge logs and
        # skips, which would look like a silent no-op on the bus.
        with self.assertRaises(ValueError):
            bi.build_envelope(
                sim_id="x",
                device_mrid="m",
                attribute="DERStatus.something",
                forward=bi.encode_power(1000.0),
            )

    def test_empty_device_mrid_refused(self):
        with self.assertRaises(ValueError):
            bi.build_envelope(
                sim_id="x",
                device_mrid="",
                attribute=bi.DELTA_ATTR_TARGET_W,
                forward=bi.encode_power(1000.0),
            )


class TestTopic(unittest.TestCase):
    def test_input_topic_matches_sim_topics_go(self):
        # internal/cim/sim/topics.go:29-31 plus the STOMP "/topic/" prefix.
        self.assertEqual(
            bi.input_topic("abc123"),
            "/topic/goss.gridappsd.simulation.input.abc123",
        )

    def test_empty_sim_id_refused(self):
        with self.assertRaises(ValueError):
            bi.input_topic("")


if __name__ == "__main__":
    unittest.main()
