"""Unit tests for the live-DERControl to setpoint-line writer.

These assert the DECODED WATTS and the correlation mapping, not merely
that a line was produced. A decode that silently dropped the multiplier
would emit a line 10x or 1000x off with no downstream symptom, which is
the exact failure class data-invariants Rule 1 exists to catch.

Run:
    <venv>/bin/python -m unittest test_derc_to_setpoint_file -v
"""
from __future__ import annotations

import json
import unittest

import derc_to_setpoint_file as d2s

# A real response body captured from the running bridge on 2026-07-30
# (sim_id gago094p2d, device LFDI 6847CD...59D3) after a 5000 W delta was
# injected on the live bus. Used verbatim so the parser is tested against
# the server's actual output, not a hand-idealized shape.
LIVE_BODY = (
    '<DERControlList xmlns="urn:ieee:std:2030.5:ns" '
    'href="/edev/6847CD3F190C1220A93267225DE57AF1949A59D3/fsa/1/derp/1/derc" '
    'all="1" results="1" pollRate="900"><DERControl '
    'xmlns="urn:ieee:std:2030.5:ns" '
    'href="/edev/6847CD3F190C1220A93267225DE57AF1949A59D3/fsa/1/derp/1/derc/active">'
    "<mRID>6847CD3F190C1220A93267225DE57AF1949A59D3-active</mRID>"
    "<EventStatus><currentStatus>1</currentStatus><dateTime>1785390031</dateTime>"
    "<potentiallySuperseded>false</potentiallySuperseded></EventStatus>"
    "<DERControlBase><opModTargetW><multiplier>0</multiplier><value>5000</value>"
    "</opModTargetW></DERControlBase></DERControl></DERControlList>"
)


class TestDecode(unittest.TestCase):
    def test_multiplier_zero_is_identity(self):
        self.assertEqual(d2s.decode_target_w(0, 5000), 5000.0)

    def test_multiplier_applied_not_ignored(self):
        # The exact bug this guards: dropping the multiplier would yield
        # 10000.0 instead of 100000.0.
        self.assertEqual(d2s.decode_target_w(1, 10000), 100000.0)
        self.assertNotEqual(d2s.decode_target_w(1, 10000), 10000.0)

    def test_negative_multiplier(self):
        self.assertEqual(d2s.decode_target_w(-1, 55), 5.5)

    def test_negative_value_sign_preserved(self):
        self.assertEqual(d2s.decode_target_w(0, -4200), -4200.0)

    def test_matches_the_c_expression_over_a_range(self):
        # Same arithmetic as map_l3_imm_controls.c:87.
        for mult, val in [(0, 1), (0, 32767), (1, 10000), (2, 500), (-2, 12345)]:
            with self.subTest(mult=mult, val=val):
                self.assertEqual(
                    d2s.decode_target_w(mult, val), float(val) * (10.0**mult)
                )


class TestParseLiveBody(unittest.TestCase):
    def test_parses_the_real_served_body(self):
        mrid, mult, val = d2s.parse_derc(LIVE_BODY)
        self.assertEqual(mrid, "6847CD3F190C1220A93267225DE57AF1949A59D3-active")
        self.assertEqual(mult, 0)
        self.assertEqual(val, 5000)

    def test_decoded_watts_from_the_real_body_is_5000(self):
        _, mult, val = d2s.parse_derc(LIVE_BODY)
        self.assertEqual(d2s.decode_target_w(mult, val), 5000.0)

    def test_missing_target_refused_not_zeroed(self):
        body = LIVE_BODY.replace(
            "<opModTargetW><multiplier>0</multiplier><value>5000</value>"
            "</opModTargetW>",
            "",
        )
        with self.assertRaises(ValueError) as ctx:
            d2s.parse_derc(body)
        self.assertIn("not a zero target", str(ctx.exception))

    def test_missing_mrid_refused(self):
        body = LIVE_BODY.replace(
            "<mRID>6847CD3F190C1220A93267225DE57AF1949A59D3-active</mRID>", ""
        )
        with self.assertRaises(ValueError) as ctx:
            d2s.parse_derc(body)
        self.assertIn("mRID", str(ctx.exception))

    def test_multiplier_one_body_decodes_to_100kw(self):
        body = LIVE_BODY.replace(
            "<multiplier>0</multiplier><value>5000</value>",
            "<multiplier>1</multiplier><value>10000</value>",
        )
        _, mult, val = d2s.parse_derc(body)
        self.assertEqual(d2s.decode_target_w(mult, val), 100000.0)


class TestCaseId(unittest.TestCase):
    def test_deterministic(self):
        a = d2s.mrid_to_case_id("some-mrid-active")
        b = d2s.mrid_to_case_id("some-mrid-active")
        self.assertEqual(a, b)

    def test_distinct_mrids_distinct_ids(self):
        self.assertNotEqual(
            d2s.mrid_to_case_id("device-a-active"),
            d2s.mrid_to_case_id("device-b-active"),
        )

    def test_fits_exactly_in_a_float64(self):
        # The OpenDSS federate carries case_id as a HELICS double; the id
        # must survive the float round trip exactly or the correlation
        # join silently mismatches.
        cid = d2s.mrid_to_case_id(
            "6847CD3F190C1220A93267225DE57AF1949A59D3-active"
        )
        self.assertEqual(int(float(cid)), cid)
        self.assertLess(cid, 2**53)

    def test_empty_mrid_refused(self):
        with self.assertRaises(ValueError):
            d2s.mrid_to_case_id("")


class TestBuildLine(unittest.TestCase):
    def test_line_is_parseable_by_the_helics_glue_shape(self):
        line, prov = d2s.build_line(
            "6847CD3F190C1220A93267225DE57AF1949A59D3-active", 0, 5000
        )
        self.assertTrue(line.endswith("\n"))
        obj = json.loads(line)
        # The keys helics_setpoint_bridge.parse_setpoint_line requires.
        self.assertIn("txid", obj)
        self.assertIn("opModTargetW", obj)
        self.assertEqual(obj["opModTargetW"], 5000.0)

    def test_provenance_keeps_the_full_mrid_not_only_the_numeric_id(self):
        # The numeric case id is a reduction; the ledger must retain the
        # authoritative identifier so the correlation is resolvable.
        _, prov = d2s.build_line("dev-x-active", 1, 10000)
        self.assertEqual(prov["derc_mrid"], "dev-x-active")
        self.assertEqual(prov["decoded_watts"], 100000.0)
        self.assertEqual(prov["wire_multiplier"], 1)
        self.assertEqual(prov["wire_value"], 10000)

    def test_txid_in_line_matches_case_id_in_provenance(self):
        line, prov = d2s.build_line("dev-y-active", 0, 2500)
        self.assertEqual(json.loads(line)["txid"], prov["case_id"])


if __name__ == "__main__":
    unittest.main()
