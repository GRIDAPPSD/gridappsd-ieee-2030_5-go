"""GAGO-094 LIVE co-simulation runner: bus -> bridge -> 2030.5 client -> HELICS.

This is the live-mode sibling of run_federation.py. run_federation.py is
the OFFLINE replay path: it loads bridge_capture.json (produced by the Go
test TestGAGO044CaptureBridgeSetpoints) and drives canned values into the
federation. That path is deliberately left intact and runnable; this file
adds the live path alongside it rather than replacing it.

WHAT IS LIVE HERE

    [1] this runner publishes a real DifferenceBuilder envelope onto the
        LIVE ActiveMQ destination
        /topic/goss.gridappsd.simulation.input.<sim_id>
        (internal/cim/sim/topics.go:29-31)
    [2] the RUNNING bridge's runControlSubscriber consumes it
        (cmd/bridge/main.go:1019-1041) and ApplyControlDelta writes a real
        DERControl into the embedded 2030.5 server's store
        (internal/sep2embed/control.go:131-205)
    [3] the EPRI C client GETs that DERControl over real mTLS and actuates
        it, appending a JSON setpoint line to its controls_request_file
        (map_l3_imm_controls.c:81-95)
    [4] helics_setpoint_bridge.py (in the EPRI worktree) drains that file
        and publishes onto shim/p_set_w / shim/q_set_var / shim/case_id
    [5] opendss_der_federate.py applies the setpoint and publishes the
        measured DER power back

Nothing in this chain is a canned file.

WHAT IS NOT LIVE, stated plainly

The simulation_id is SYNTHETIC. No GridAPPS-D platform simulation is
started: standing one up is the one move that risks perturbing the loaded
Southern feeder placement, which must not be restarted. Real ActiveMQ,
real STOMP, real envelope, real bridge subscriber, real Southern feeder
mRIDs; a synthesized sim_id string. That is the whole of the caveat.

The down-leg (control delta) and the up-leg (DERStatus telemetry relay)
share the same destination in the shipped wiring
(cmd/bridge/main.go:404 plus the LOAD-BEARING INVARIANT comment near
:985). This runner does not split the topic. The monitor disambiguates
the two legs by FRAME CONTENT: a control delta carries a
"DERControl.DERControlBase."-prefixed attribute, telemetry carries a
"DERStatus."-prefixed one.

TIME MODEL

Both HELICS federates are periodic value federates, period 1.0 s,
offset 0, HELICS_FLAG_UNINTERRUPTIBLE, so grants land only on the period
and the handshake is strict lockstep. That is unchanged from the offline
runner. What IS new is that the control source is now ASYNCHRONOUS with
respect to the period: the C client writes its setpoint whenever it
happens to poll and actuate, not on a grant boundary.

That asynchrony forces a REAL-TIME constraint the offline runner does not
need. HELICS time is logical, so an unpaced federation completes 22 steps
of 1.0 s period in milliseconds of wall time and exits before a live
source has written anything: the first live run of this federation did
exactly that and reported "0 setpoints drained, VERDICT FAIL" with no
error naming the cause (2026-07-30). The EPRI shim federate therefore
pins logical time to wall time by default (sleeping at each grant until
wall elapsed catches up to logical elapsed), and because the two
federates are mutually subscribed the lockstep handshake paces the whole
federation. Pass --no-realtime-pace ONLY when the setpoint file is fully
populated before startup. The ledger records `realtime_paced` and
`wall_clock_elapsed_s` so a run's pacing is evidence, not an assumption.

Between-grant policy: coalesce-last (the default) or strict-one, enforced
in helics_setpoint_bridge._publish_federate. The EPRI shim federate drains
the setpoint file once per granted step and applies the LAST setpoint seen
in that interval, publishing its txid as the case id. Every superseded
setpoint from the same interval is still written to the correlation ledger
with applied=false, so a coalesced setpoint is accounted for, never
silently dropped. strict-one refuses outright when more than one setpoint
lands inside a single grant interval, for runs that need a strict
one-to-one setpoint-to-measurement mapping.
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time

import bus_inject

HERE = os.path.dirname(os.path.abspath(__file__))

# Default location of the EPRI worktree's HELICS glue. Overridable so the
# runner does not hardcode one host layout.
DEFAULT_EPRI_SHIM = (
    "/home/debian/repos/EPRI_Client-gago094/agents_com_ieee2030/"
    "helics_setpoint_bridge.py"
)

PERIOD = 1.0


def publish_delta(
    host: str,
    port: int,
    user: str,
    password: str,
    sim_id: str,
    device_mrid: str,
    watts: float,
    attribute: str = bus_inject.DELTA_ATTR_TARGET_W,
) -> dict[str, object]:
    """Publish one control delta onto the LIVE bus and return the envelope
    that was sent, so the caller can assert on the exact bytes.

    Only the synthetic sim_id's own destination is ever written. No
    platform request queue and no real simulation id is touched.
    """
    import stomp

    wire = bus_inject.encode_power(watts)
    envelope = bus_inject.build_envelope(
        sim_id=sim_id,
        device_mrid=device_mrid,
        attribute=attribute,
        forward=wire,
    )
    dest = bus_inject.input_topic(sim_id)
    body = json.dumps(envelope)

    conn = stomp.Connection([(host, port)])
    conn.connect(user, password, wait=True)
    try:
        conn.send(destination=dest, body=body, content_type="application/json")
        # A STOMP send is fire-and-forget; give the broker a beat to fan out
        # to the bridge's subscriber before the caller checks for effect.
        time.sleep(0.5)
    finally:
        conn.disconnect()

    print(
        f"[inject] {dest}\n"
        f"[inject] attribute={attribute} object={device_mrid}\n"
        f"[inject] requested={watts:.1f} W -> wire multiplier={wire.multiplier} "
        f"value={wire.value} (physical {wire.physical:.1f} W)",
        flush=True,
    )
    return envelope


def run_federation(
    steps: int,
    epri_shim: str,
    setpoint_file: str,
    ledger_out: str,
    between_grant_policy: str,
    broker_name: str,
    timeout_s: int,
    realtime_pace: bool = True,
) -> int:
    """Bring up broker + OpenDSS DER federate + the EPRI setpoint shim,
    wait bounded, and tear everything down.

    Every process gets a bounded wait and is force-killed on timeout. A
    leaked helics_broker would silently poison a later run, so teardown is
    unconditional.
    """
    python = sys.executable
    procs: list[tuple[str, subprocess.Popen[bytes]]] = []

    broker = subprocess.Popen(
        ["helics_broker", "-f", "2", f"--name={broker_name}", "--loglevel=warning"],
        cwd=HERE,
    )
    procs.append(("broker", broker))
    time.sleep(1.0)

    opendss = subprocess.Popen([python, "opendss_der_federate.py", str(steps)], cwd=HERE)
    procs.append(("opendss", opendss))

    shim_argv = [
        python,
        epri_shim,
        "--setpoint-file",
        setpoint_file,
        "--steps",
        str(steps),
        "--period",
        str(PERIOD),
        "--between-grant-policy",
        between_grant_policy,
        "--ledger-out",
        ledger_out,
    ]
    if not realtime_pace:
        shim_argv.append("--no-realtime-pace")
    shim = subprocess.Popen(shim_argv, cwd=os.path.dirname(epri_shim))
    procs.append(("epri_shim", shim))

    rc_shim = rc_opendss = None
    try:
        rc_shim = shim.wait(timeout=timeout_s)
        rc_opendss = opendss.wait(timeout=timeout_s)
        broker.wait(timeout=30)
    except subprocess.TimeoutExpired as exc:
        _terminate_all(procs)
        print(
            f"[live] federation hung waiting on a process ({exc.cmd}); "
            f"terminated broker, opendss, and epri_shim",
            file=sys.stderr,
            flush=True,
        )
        return 2
    finally:
        # Belt and braces: nothing survives this function, hang or not.
        _terminate_all(procs)

    print(f"[live] rc: epri_shim={rc_shim} opendss={rc_opendss}", flush=True)
    return 0 if (rc_shim == 0 and rc_opendss == 0) else 1


def _terminate_all(procs: list[tuple[str, subprocess.Popen[bytes]]]) -> None:
    """Graceful terminate then force kill for every still-running process.
    Runs to completion for all of them: a failure tearing one down never
    prevents tearing down the rest."""
    for name, p in procs:
        if p.poll() is None:
            print(f"[live] terminating {name} (pid {p.pid})", flush=True)
            p.terminate()
    for name, p in procs:
        try:
            p.wait(timeout=10)
        except subprocess.TimeoutExpired:
            print(f"[live] killing {name} (pid {p.pid})", flush=True)
            p.kill()
            try:
                p.wait(timeout=10)
            except subprocess.TimeoutExpired:
                print(f"[live] {name} (pid {p.pid}) would not die", flush=True)


def verdict(ledger_out: str) -> int:
    """Report the coupled result, hop by hop with units.

    A federation that merely ran clean is not a pass: this asserts that a
    published watt setpoint arrived at the OpenDSS DER as the SAME power
    (modulo the documented W -> kW factor of 1/1000) under a matching
    correlation key.
    """
    if not os.path.exists(ledger_out):
        print(f"VERDICT: FAIL, no ledger at {ledger_out}", flush=True)
        return 1

    with open(ledger_out) as fh:
        data = json.load(fh)

    ledger = data["ledger"]
    print()
    print("GAGO-094 live coupling: setpoint traverse")
    print(f"between-grant policy: {data['between_grant_policy']}   period: {data['period_s']}s")
    # Pacing is reported because an unpaced run against a live source can
    # drain nothing for time-model reasons alone; the reader must be able
    # to tell the two failure causes apart.
    print(
        f"realtime paced: {data.get('realtime_paced')}   "
        f"wall-clock elapsed: {data.get('wall_clock_elapsed_s')}s"
    )
    print(f"active-power factor (file W -> shim/p_set_w W): {data['active_power_factor_w_per_file_unit']}")
    print("-" * 100)
    print(f"{'txid':>6} {'file value':>14} {'p_set_w (W)':>14} {'meas P (kW)':>13} {'expect (kW)':>12} {'ok':>5}")
    print("-" * 100)

    applied = [e for e in ledger if e.get("applied")]
    if not applied:
        print("(no setpoint was published)")
        print("-" * 100)
        print("VERDICT: FAIL, nothing traversed the HELICS leg.")
        return 1

    ok = True
    for e in applied:
        p_w = e["p_set_w"]
        meas_kw = e["measured_p_kw"]
        # The documented W -> kW hop: opendss_der_federate publishes kW,
        # der_model divides the commanded watts by 1000
        # (der_model.py:40). Factor 1/1000, asserted here numerically.
        expect_kw = None if p_w is None else p_w / 1000.0
        if meas_kw is None or expect_kw is None:
            row_ok = False
        else:
            row_ok = abs(meas_kw - expect_kw) <= 0.05
        ok = ok and row_ok
        meas_s = "None" if meas_kw is None else f"{meas_kw:+.3f}"
        exp_s = "None" if expect_kw is None else f"{expect_kw:+.3f}"
        print(
            f"{e['txid']:>6} {e['raw_value']:>14.3f} {p_w:>14.3f} "
            f"{meas_s:>13} {exp_s:>12} {'OK' if row_ok else 'FAIL':>5}"
        )

    print("-" * 100)
    if data["parse_errors"]:
        print("parse errors surfaced (not swallowed):")
        for err in data["parse_errors"]:
            print(f"  - {err}")
    if data["truncations_observed"]:
        print(f"setpoint-file rewrites observed: {data['truncations_observed']}")

    if not ok:
        print("VERDICT: FAIL, the measured DER power does not match the published setpoint.")
        return 1
    print("VERDICT: the setpoint traversed bus -> bridge -> 2030.5 client -> HELICS -> OpenDSS")
    print("with the documented unit factors and a matching correlation key.")
    return 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--sim-id", required=True, help="SYNTHETIC simulation id")
    ap.add_argument("--device-mrid", required=True, help="Southern feeder device mRID")
    ap.add_argument("--watts", type=float, default=5000.0)
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=61613)
    ap.add_argument("--user", default=os.environ.get("SEP2_STOMP_USER", ""))
    ap.add_argument("--password", default=os.environ.get("SEP2_STOMP_PASSWORD", ""))
    ap.add_argument("--setpoint-file", required=True)
    ap.add_argument("--epri-shim", default=DEFAULT_EPRI_SHIM)
    ap.add_argument("--ledger-out", default="/tmp/gago094/logs/live-ledger.json")
    ap.add_argument("--steps", type=int, default=40)
    ap.add_argument("--timeout", type=int, default=180)
    ap.add_argument(
        "--between-grant-policy", choices=["coalesce-last", "strict-one"],
        default="coalesce-last",
    )
    ap.add_argument(
        "--inject-only", action="store_true",
        help="publish the bus delta and exit without starting the federation",
    )
    ap.add_argument(
        "--federation-only", action="store_true",
        help="run the federation without injecting (the delta is injected "
        "out of band, e.g. while the EPRI client is already polling)",
    )
    ap.add_argument(
        "--no-realtime-pace", action="store_true",
        help="let logical time fast-forward past wall time. Safe ONLY when "
        "the setpoint file is fully populated before startup; against a live "
        "source the federation finishes before the source writes and drains "
        "nothing (see the TIME MODEL section above)",
    )
    args = ap.parse_args(argv)

    if not args.user or not args.password:
        # Fail closed: credentials come from the environment, never from a
        # default baked into source.
        print(
            "error: broker credentials required via --user/--password or "
            "SEP2_STOMP_USER/SEP2_STOMP_PASSWORD",
            file=sys.stderr,
        )
        return 2

    if not args.federation_only:
        publish_delta(
            args.host, args.port, args.user, args.password,
            args.sim_id, args.device_mrid, args.watts,
        )
    if args.inject_only:
        return 0

    rc = run_federation(
        args.steps, args.epri_shim, args.setpoint_file, args.ledger_out,
        args.between_grant_policy, f"gago094live{int(time.time())}", args.timeout,
        realtime_pace=not args.no_realtime_pace,
    )
    vrc = verdict(args.ledger_out)
    return rc or vrc


if __name__ == "__main__":
    raise SystemExit(main())
