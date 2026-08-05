# DERControl sign loopback harness

A HELICS plus OpenDSS co-simulation that physically verifies the bridge's
DERControl active and reactive sign convention: it drives the bridge's
REAL control-path output into an OpenDSS DER and reads the resulting DER
power direction back out.

This harness is a test harness, not a shipped bridge component. It lives
in the worktree only and is NOT a `go.mod` dependency of the bridge. The
Python packages (HELICS, opendssdirect) are installed in a throwaway
virtualenv, not system wide.

## Why co-simulation and not the platform sim

The GridAPPS-D platform's own REQUEST_SIMULATION / GridLAB-D path is dead
on this stack (every start fails with "no substation source", tracked
separately). This harness therefore closes the loop WITHOUT
the platform's GridLAB-D simulation: it couples the bridge output to an
OpenDSS solve we drive directly.

## Unit under test vs harness

The bridge control path is the unit under test and is NOT reimplemented
here. The Go test `TestSignLoopbackCaptureBridgeSetpoints`
(`internal/sep2embed/control_sign_loopback_test.go`) runs the genuine
`ApplyControlDelta` for each commanded 2030.5 intent, reads the resulting
`DERControlBase` target values back out of the store, and writes them to
`bridge_capture.json`. The Python `control_shim` federate loads that file
and drives those exact values into the federation, so the co-sim measures
the bridge's real output, not a hand typed number.

## Topology

Single HELICS broker (zmq), two value federates:

```
control_shim  --(shim/p_set_w, shim/q_set_var, shim/case_id)-->  opendss_der
control_shim  <--(der/meas_p_kw, der/meas_q_kvar, der/echo_case)-- opendss_der
```

- `control_shim` (`control_shim_federate.py`): carries the bridge's real
  captured setpoint into the federation and collects the measured DER
  power keyed by the echoed case id.
- `opendss_der` (`opendss_der_federate.py`): applies the setpoint to the
  OpenDSS DER (`der_model.py`), solves, and publishes the measured DER
  active/reactive power, generator positive.

## Time model

- Both federates are periodic value federates: period 1.0 s, offset 0.
- Both are `UNINTERRUPTIBLE`: grants land only on the period, never on an
  asynchronous value arrival between periods, so the handshake is strict
  lockstep.
- The federation runs `t = 1 .. T` with `T = 2 * num_cases + 3`.
- One command per two steps: step `t = 2k+1` publishes command `k`. A
  value published at `t` is delivered at the peer's next grant, so the
  command reaches OpenDSS at `t+1` and its measurement returns to the
  shim by `t+2`.
- Clean exit: after the loop each federate requests `HELICS_TIME_MAXTIME`
  so the broker coordinates the final grant across both federates rather
  than dropping an in flight message at teardown.

## Convergence and sanity checks

A federation that merely runs clean is not a pass. `run_federation.py`
asserts coupled-solver agreement on three axes:

1. Correspondence: every case has a measurement whose echoed case id
   matched, so the shim reads the DER response to the command it sent,
   not a stale sample.
2. Magnitude agreement: the measured DER power tracks the commanded
   target within tolerance (generator model=1 holds its commanded
   terminal power), confirming the coupling actually moved the DER.
3. Direction: the measured sign is asserted against 2030.5 semantics.

## Sign conventions in play

- IEEE 2030.5 (section 10.10): `opModTargetW` positive = discharging /
  exporting; `opModTargetVar` positive = over excited / injecting VARs.
- OpenDSS `Generator` kW/kvar parameters are generator positive
  (kW>0 exports, kvar>0 injects), matching 2030.5 directly.
- OpenDSS `CktElement.Powers()` is load positive (power INTO the element
  from the grid). `der_model.apply_setpoint` returns the negative of that,
  so a discharging DER reads positive P and an injecting DER positive Q.

## Run

```
cd gridappsd-ieee-2030_5-go
python3 -m venv /tmp/sign-loopback-venv
/tmp/sign-loopback-venv/bin/pip install -r harness/requirements.txt

# Regenerate the bridge capture from the real control path:
SIGN_LOOPBACK_CAPTURE_OUT=$PWD/harness/bridge_capture.json \
  go test ./internal/sep2embed/ -run TestSignLoopbackCaptureBridgeSetpoints

# Run the federation and print the directional verdict:
cd harness
/tmp/sign-loopback-venv/bin/python run_federation.py
```

`helics_broker` must be on `PATH` (HELICS runtime 3.6.x).

## Result

For the two commanded cases the measured OpenDSS DER direction matched
the commanded 2030.5 intent with no sign flip:

| commanded 2030.5 | bridge delta | measured OpenDSS DER |
|---|---|---|
| opModTargetW +5000 W (discharge) | target_w +5000 | P = +5.000 kW (export) |
| opModTargetVar +3000 var (inject) | target_var +3000 | Q = +3.000 kvar (inject) |

Evidence supports keeping `activeSignFlip = false` and
`reactiveSignFlip = false`. The final electrical-correctness sign off is
Vance's, not this harness's.
