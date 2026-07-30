"""Write a C-client-format setpoint line from the LIVE served DERControl.

WHY THIS EXISTS, stated plainly so nobody mistakes it for the real thing.

The intended hop 4 of the live chain is:

    the EPRI C client GETs the DERControl over mTLS and actuates it,
    appending a JSON line to its controls_request_file
    (M20305_file_derinfo/map_l3_imm_controls.c:81-95)

That hop is currently WALLED, for a reason located in the vendor client's
own option table rather than in this harness. Every oeg_client mode that
sets GET_DERC also sets REGISTER_TEST (oeg_client.c:169, :182, :192,
:200), and REGISTER_TEST refuses to proceed unless the EndDevice carries
a RegistrationLink (oeg_client.c:585-358: test_fail "EndDevice does not
contain RegistrationLink"). The bridge's embedded server does not serve
one, so the client aborts before it ever reaches the DERControl. A second
wall sits behind it: GET /edev/{lfdi}/fsa returns an EMPTY list
(all="0" results="0") because internal/sep2embed/stores.go:34 constructs
the FSAs store and nothing ever writes to it, so a link-traversing client
cannot discover the DERProgram even after registering. Both are reported
as findings; neither is fixed here, because the fix is bridge-server
feature work and not this card's lane.

WHAT THIS MODULE THEREFORE IS

A LIVE-DERIVED substitute for hop 4 only. It does NOT invent a setpoint
value. It performs a real mTLS GET against the running embedded 2030.5
server, parses the opModTargetW the bridge actually served, decodes it
with the same arithmetic the C client uses
(map_l3_imm_controls.c:87: targetw = value * pow(10, multiplier)), and
writes the C client's exact line shape to the controls_request_file.

So the value that reaches HELICS still originates from the bus delta that
the bridge applied. What is substituted is the C client's actuation
DECISION, not the setpoint number. That distinction is the whole
difference between this being a useful partial and being a replay, and it
is why this module reads the server rather than accepting a --watts
argument.

The txid it writes is NOT a fabricated correlation id in the sense
data-invariants Rule 2 forbids: it is the DERControl mRID served by the
bridge, reduced to the numeric form the OpenDSS federate's case_id
channel requires (a HELICS double). The full string mRID is carried
alongside in the ledger, so the numeric case id is always resolvable back
to the exact DERControl. See mrid_to_case_id.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import re
import ssl
import sys
import time
import urllib.request

# The bridge writes exactly one DERControl per device, id "active"
# (internal/sep2embed/control.go activeControlID), so a single-match
# extraction is correct rather than lucky.
_RE_TARGET_W = re.compile(
    r"<opModTargetW>\s*<multiplier>(-?\d+)</multiplier>\s*"
    r"<value>(-?\d+)</value>\s*</opModTargetW>"
)
_RE_MRID = re.compile(r"<mRID>([^<]+)</mRID>")

# The OpenDSS federate's case_id input is a HELICS double
# (opendss_der_federate.py:47), so the correlation key must be numeric.
# 2**40 keeps the value exactly representable in a float64 (which holds
# integers exactly to 2**53) while leaving collision probability
# negligible for a single run.
_CASE_ID_MODULUS = 2**40


def mrid_to_case_id(mrid: str) -> int:
    """Reduce a DERControl mRID to the numeric case id HELICS can carry.

    Deterministic and collision-resistant: the same mRID always maps to
    the same case id, so a measurement can be joined back to its
    DERControl. The full mRID string is preserved in the ledger, so this
    reduction never becomes the only record of which control acted.
    """
    if not mrid:
        raise ValueError("mrid_to_case_id: mRID must be non-empty")
    digest = hashlib.sha256(mrid.encode("utf-8")).digest()
    return int.from_bytes(digest[:8], "big") % _CASE_ID_MODULUS


def decode_target_w(multiplier: int, value: int) -> float:
    """Decode a sep2 ActivePower to watts.

    This is deliberately the SAME arithmetic the C client applies at
    map_l3_imm_controls.c:87 (targetw = value * pow(10, multiplier)), so
    the number written here is the number the C client would have
    written from the same DERControl. Any divergence between this and the
    C expression is a unit bug, which is why it is one line and cited.
    """
    return float(value) * (10.0**multiplier)


def fetch_derc(url: str, cert: str, key: str, cafile: str | None) -> str:
    """Real mTLS GET of the DERControlList. Returns the response body.

    Verification uses the harness CA when one is supplied. When it is
    not, verification is disabled explicitly and loudly rather than
    silently: this talks to a loopback dev server with a freshly minted
    self-signed CA, and a caller who omits the CA should see that choice
    in the output, not have it hidden.
    """
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    if cafile:
        ctx.load_verify_locations(cafile)
        ctx.check_hostname = False  # dev cert CN is not the loopback literal
        ctx.verify_mode = ssl.CERT_REQUIRED
    else:
        print(
            "[derc] WARNING: no --cafile given, server certificate is NOT "
            "verified (loopback dev server only)",
            file=sys.stderr,
            flush=True,
        )
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
    ctx.load_cert_chain(certfile=cert, keyfile=key)

    req = urllib.request.Request(url, headers={"Accept": "application/sep+xml"})
    with urllib.request.urlopen(req, context=ctx, timeout=15) as resp:
        if resp.status != 200:
            raise RuntimeError(f"GET {url} returned HTTP {resp.status}")
        return resp.read().decode("utf-8")


def parse_derc(body: str) -> tuple[str, int, int]:
    """Extract (mRID, multiplier, value) from a DERControlList body.

    Refuses rather than defaulting when the control carries no
    opModTargetW: a missing target is not a zero target, and treating it
    as one would command the DER to zero output with no error anywhere
    (data-invariants Rule 2).
    """
    m_mrid = _RE_MRID.search(body)
    if not m_mrid:
        raise ValueError("DERControlList carries no <mRID>; cannot correlate")
    m_pw = _RE_TARGET_W.search(body)
    if not m_pw:
        raise ValueError(
            "DERControlList carries no <opModTargetW>; refusing to synthesize "
            "a zero target (a missing target is not a zero target)"
        )
    return m_mrid.group(1), int(m_pw.group(1)), int(m_pw.group(2))


def build_line(mrid: str, multiplier: int, value: int) -> tuple[str, dict[str, object]]:
    """Build the C client's exact setpoint-line shape plus a provenance dict.

    Line shape mirrors map_l3_imm_controls.c:88-93: a single-line JSON
    object with GUID, ts, txid, type and the op-mode key, terminated by a
    newline. helics_setpoint_bridge.parse_setpoint_line consumes exactly
    this.
    """
    watts = decode_target_w(multiplier, value)
    case_id = mrid_to_case_id(mrid)
    rec = {
        "GUID": 0,
        "ts": int(time.time()),
        "txid": case_id,
        "type": 2,
        "opModTargetW": watts,
    }
    provenance = {
        "derc_mrid": mrid,
        "wire_multiplier": multiplier,
        "wire_value": value,
        "decoded_watts": watts,
        "case_id": case_id,
        "decode_expression": "value * 10**multiplier (map_l3_imm_controls.c:87)",
    }
    return json.dumps(rec) + "\n", provenance


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--url", required=True, help="DERControlList URL on the running bridge")
    ap.add_argument("--cert", required=True, help="device certificate, PEM")
    ap.add_argument("--key", required=True, help="device private key, PEM")
    ap.add_argument("--cafile", default=None, help="CA to verify the server with")
    ap.add_argument("--setpoint-file", required=True, help="controls_request_file to append to")
    ap.add_argument("--provenance-out", default=None)
    args = ap.parse_args(argv)

    body = fetch_derc(args.url, args.cert, args.key, args.cafile)
    mrid, multiplier, value = parse_derc(body)
    line, provenance = build_line(mrid, multiplier, value)

    # Append, matching the C client's "a+" open (map_l3_imm_controls.c:94
    # fflush after each line) so the drainer's byte-offset tail behaves
    # identically to the real thing.
    with open(args.setpoint_file, "a") as fh:
        fh.write(line)
        fh.flush()

    print(f"[derc] served mRID={mrid}", flush=True)
    print(
        f"[derc] wire multiplier={multiplier} value={value} "
        f"-> decoded {provenance['decoded_watts']} W",
        flush=True,
    )
    print(f"[derc] wrote to {args.setpoint_file}: {line.strip()}", flush=True)

    if args.provenance_out:
        with open(args.provenance_out, "w") as fh:
            json.dump(provenance, fh, indent=2)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
