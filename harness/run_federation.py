"""GAGO-044 co-simulation runner: broker plus two federates, then verdict.

Topology: a single HELICS broker (zmq) with two value federates.

    control_shim  --(shim/p_set_w, shim/q_set_var, shim/case_id)-->  opendss_der
    control_shim  <--(der/meas_p_kw, der/meas_q_kvar, der/echo_case)-- opendss_der

The control_shim carries the bridge's REAL ApplyControlDelta output
(bridge_capture.json); the opendss_der applies it to a generator positive
OpenDSS DER and measures what the DER physically does.

Time model: both federates periodic (period 1.0 s, offset 0),
uninterruptible, lockstep t=1..T with T = 2*num_cases + 3. One command
per two steps: step t=2k+1 publishes command k, its measurement returns
to the shim by t=2k+3.

Convergence / sanity checks (a co-sim that merely runs clean is not a
pass):
  1. Correspondence: every case has a measurement whose echoed case id
     matched, so we read the DER response to the command we sent, not a
     stale sample.
  2. Magnitude agreement: the measured DER power tracks the commanded
     target within tolerance (generator model=1 injects its commanded
     terminal power), confirming the coupling actually moved the DER.
  3. Direction: the measured sign is asserted against 2030.5 semantics.
"""
import json
import os
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
CAPTURE = os.path.join(HERE, "bridge_capture.json")
RESULT = os.path.join(HERE, "loopback_result.json")

TOL_KW = 0.05   # 50 W: generator model=1 holds its commanded terminal power
TOL_KVAR = 0.05


def run():
    with open(CAPTURE) as f:
        num_cases = len(json.load(f)["cases"])
    total_steps = 2 * num_cases + 3

    python = sys.executable
    broker = subprocess.Popen(
        ["helics_broker", "-f", "2", "--name=gago044broker", "--loglevel=warning"],
        cwd=HERE,
    )
    time.sleep(1.0)
    opendss = subprocess.Popen([python, "opendss_der_federate.py", str(total_steps)], cwd=HERE)
    shim = subprocess.Popen(
        [python, "control_shim_federate.py", CAPTURE, RESULT, str(total_steps)],
        cwd=HERE,
    )
    procs = [broker, opendss, shim]

    try:
        rc_shim = shim.wait(timeout=90)
        rc_opendss = opendss.wait(timeout=90)
        broker.wait(timeout=30)
    except subprocess.TimeoutExpired as exc:
        # A hang must not leak the federate and broker processes. Tear all
        # three down (graceful, then forced) and re raise a clear message.
        _terminate_all(procs)
        raise SystemExit(
            f"federation hung waiting on a process ({exc.cmd}); terminated broker, "
            f"opendss, and shim"
        ) from exc

    if rc_shim != 0 or rc_opendss != 0:
        raise SystemExit(f"federate failed: shim={rc_shim} opendss={rc_opendss}")


def _terminate_all(procs):
    """Best effort teardown of every still running process: graceful
    terminate first, then kill anything that ignores it. Runs to
    completion for all procs; a failure tearing one down never prevents
    tearing down the rest, so this cannot mask the original hang."""
    for p in procs:
        if p.poll() is None:
            p.terminate()
    for p in procs:
        try:
            p.wait(timeout=10)
        except subprocess.TimeoutExpired:
            p.kill()
            try:
                p.wait(timeout=10)
            except subprocess.TimeoutExpired:
                pass


def verdict():
    with open(RESULT) as f:
        data = json.load(f)

    print()
    print("GAGO-044 DERControl sign loopback: directional evidence")
    print("activeSignFlip =", data["activeSignFlip"], "  reactiveSignFlip =", data["reactiveSignFlip"])
    print("-" * 96)
    print(f"{'case':13s} {'2030.5 command':26s} {'bridge delta':16s} {'measured OpenDSS DER':22s} {'dir':4s}")
    print("-" * 96)

    ok = True
    # Expected direction per 2030.5: opModTargetW +ve = discharge (P>0),
    # opModTargetVar +ve = inject (Q>0).
    for r in data["results"]:
        p_kw = r["measured_p_kw"]
        q_kvar = r["measured_q_kvar"]
        if p_kw is None or q_kvar is None:
            print(f"{r['name']:13s} MISSING MEASUREMENT (no matched echo) -> FAIL")
            ok = False
            continue

        if r["attribute"] == "opModTargetW":
            cmd = f"opModTargetW {r['commanded_value']:+.0f} W"
            delta = f"target_w {r['bridge_target_w']:+.0f}"
            commanded = r["bridge_target_w"] / 1000.0
            measured = p_kw
            expect_positive = r["commanded_value"] > 0
            mag_ok = abs(measured - commanded) <= TOL_KW
        else:
            cmd = f"opModTargetVar {r['commanded_value']:+.0f} var"
            delta = f"target_var {r['bridge_target_var']:+.0f}"
            commanded = r["bridge_target_var"] / 1000.0
            measured = q_kvar
            expect_positive = r["commanded_value"] > 0
            mag_ok = abs(measured - commanded) <= TOL_KVAR

        sign_ok = (measured > 0) == expect_positive and measured != 0
        direction = "OK" if (sign_ok and mag_ok) else "FAIL"
        ok = ok and sign_ok and mag_ok
        meas_str = f"P={p_kw:+.3f}kW Q={q_kvar:+.3f}kvar"
        print(f"{r['name']:13s} {cmd:26s} {delta:16s} {meas_str:22s} {direction}")

    print("-" * 96)
    if not ok:
        print("VERDICT: FAIL, measured direction or magnitude disagrees with the commanded 2030.5 intent.")
        raise SystemExit(1)
    print("VERDICT: coupled agreement confirmed. Commanded 2030.5 direction reproduced at the OpenDSS DER")
    print("for both cases, with no sign flip. Evidence supports keeping activeSignFlip=false and")
    print("reactiveSignFlip=false. Final physics sign off is Vance's.")


if __name__ == "__main__":
    run()
    verdict()
