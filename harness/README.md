# GAGO-044 DERControl sign loopback harness

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
separately as GAGO-042). This harness therefore closes the loop WITHOUT
the platform's GridLAB-D simulation: it couples the bridge output to an
OpenDSS solve we drive directly.

## Unit under test vs harness

The bridge control path is the unit under test and is NOT reimplemented
here. The Go test `TestGAGO044CaptureBridgeSetpoints`
(`internal/sep2embed/control_gago044_test.go`) runs the genuine
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
cd /home/debian/repos/gridappsd-ieee-2030_5-go-gago044
python3 -m venv /tmp/gago044-venv
/tmp/gago044-venv/bin/pip install -r harness/requirements.txt

# Regenerate the bridge capture from the real control path:
GAGO044_CAPTURE_OUT=$PWD/harness/bridge_capture.json \
  go test ./internal/sep2embed/ -run TestGAGO044CaptureBridgeSetpoints

# Run the federation and print the directional verdict:
cd harness
/tmp/gago044-venv/bin/python run_federation.py
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

---

# LIVE mode (GAGO-094 phase 2)

Everything above describes the OFFLINE REPLAY path, which is unchanged and
still runnable. This section documents the LIVE path added alongside it.

## Why a live mode exists

The offline path has a replay seam: `control_shim_federate.py` loads
`bridge_capture.json`, a file produced ahead of time by a Go test. That
makes the co-simulation a replay of recorded values rather than a closed
loop, and it means no running bridge is exercised at all. The live mode
removes that seam: the setpoint that reaches OpenDSS originates from a
control delta published on the real ActiveMQ bus and applied by a real
running bridge.

## Live topology

```
bus_inject.py
  -> /topic/goss.gridappsd.simulation.input.<sim_id>   (LIVE ActiveMQ)
  -> the running bridge's runControlSubscriber
  -> ApplyControlDelta writes a DERControl into the embedded 2030.5 store
  -> read back over REAL mTLS as an <opModTargetW>
  -> a C-client-format setpoint line
  -> helics_setpoint_bridge.py (EPRI worktree) drains it
  -> shim/p_set_w, shim/q_set_var, shim/case_id
  -> opendss_der_federate.py applies it and publishes der/meas_p_kw
```

## Files

| File | Role |
|---|---|
| `bus_inject.py` | builds and publishes the `diff.Message` envelope; encodes watts to the sep2 int8-multiplier / int16-mantissa pair, refusing rather than truncating |
| `derc_to_setpoint_file.py` | real mTLS GET of the served DERControlList, decodes `opModTargetW` with the C client's own expression, writes the C client's line shape |
| `run_live_federation.py` | the one bounded command: inject, bring up broker plus both federates, tear everything down, print the verdict |
| `helics_setpoint_bridge.py` (EPRI worktree) | drains the setpoint file and publishes onto the HELICS keys; owns the unit conversion and the between-grant policy |

## Units at every hop

| Hop | Quantity | Unit | Factor into the next hop |
|---|---|---|---|
| bus delta `value`/`multiplier` | sep2 ActivePower | mantissa, int16 | `value * 10**multiplier` -> W |
| served `<opModTargetW>` | sep2 ActivePower | same wire pair | `value * 10**multiplier` -> W |
| setpoint file `opModTargetW` | decoded physical | **W** | 1.0 |
| `shim/p_set_w` | HELICS double | **W** | 1/1000 |
| `der/meas_p_kw` | HELICS double | **kW** | terminal |

`shim/q_set_var` is **var** and `der/meas_q_kvar` is **kvar**. No two
adjacent hops share a unit by accident: each factor above is asserted in a
unit test, because a silent 1000x here would make a PASS meaningless.

## Time model, live

Same period 1.0 s, offset 0, `UNINTERRUPTIBLE` lockstep as the offline
mode, plus one addition that the offline mode does not need:

**Real-time pacing is ON by default in live mode.** HELICS time is
logical, so an unpaced federation runs 22 steps of 1.0 s in milliseconds
of wall time and exits before an asynchronous real source has written
anything. The EPRI shim federate therefore sleeps at each grant until wall
elapsed catches up to logical elapsed; because the two federates are
mutually subscribed, that paces the whole federation. `--no-realtime-pace`
disables it, and is safe ONLY when the setpoint file is fully populated
before startup. The ledger records `realtime_paced` and
`wall_clock_elapsed_s`, so pacing is evidence rather than an assumption.

**Between-grant policy:** `coalesce-last` (default) applies the LAST
setpoint drained in a grant interval and still ledgers every superseded
one with `applied=false`, so a coalesced setpoint is accounted for rather
than silently dropped. `strict-one` refuses outright when more than one
setpoint lands in a single interval.

## Correlation

The numeric `case_id` HELICS carries is derived from the DERControl mRID
the bridge actually served (`mrid_to_case_id`, a truncated sha256 that
stays exactly representable in a float64). The full mRID string is kept in
the ledger alongside it, so the numeric id is always resolvable back to
the exact DERControl. Nothing is synthesized.

## Run

```
# The bridge must already be running against the live broker with
# SEP2_SIMULATION_ID set to your synthetic sim id.

cd harness

# 1. Inject a control delta on the live bus.
SEP2_STOMP_USER=... SEP2_STOMP_PASSWORD=... \
  python run_live_federation.py --sim-id <synthetic-id> \
    --device-mrid <feeder device mRID> --watts 5000 \
    --setpoint-file /var/tmp/controls.inp --inject-only

# 2. Read the served DERControl back over mTLS into a setpoint line.
python derc_to_setpoint_file.py \
  --url "https://127.0.0.1:8443/edev/<LFDI>/fsa/1/derp/1/derc" \
  --cert <device>.pem --key <device-key>.pem --cafile <ca>.pem \
  --setpoint-file /var/tmp/controls.inp

# 3. Run the federation and print the verdict.
SEP2_STOMP_USER=... SEP2_STOMP_PASSWORD=... \
  python run_live_federation.py --sim-id <synthetic-id> \
    --device-mrid <feeder device mRID> \
    --setpoint-file /var/tmp/controls.inp \
    --ledger-out /tmp/live-ledger.json --steps 22 --federation-only
```

Step 2 can be run WHILE step 3's federation is in flight; real-time pacing
is what makes that work.

## Known walls in the live path

Two walls sit between the served DERControl and the EPRI C client, and
they are why step 2 exists as a separate live-derived read rather than the
C client writing the line itself:

1. **No `RegistrationLink`.** Every `oeg_client` mode that sets
   `GET_DERC` also sets `REGISTER_TEST` (`oeg_client.c:169, :182, :192,
   :200`), and `REGISTER_TEST` aborts with "EndDevice does not contain
   RegistrationLink" when the EndDevice carries none. The bridge serves
   none, so the client never reaches the DERControl.
2. **Empty FSA list.** `GET /edev/{lfdi}/fsa` returns
   `all="0" results="0"` because `internal/sep2embed/stores.go:34`
   constructs the `FSAs` store and nothing ever writes to it. Even after
   registering, a link-traversing client cannot discover the DERProgram
   (the `/fsa/1/derp/1/derc` path is reachable only if guessed).

Both are bridge-server gaps against capabilities the 2030.5 core already
provides, and both are reported as findings rather than fixed here.

## Live result

A 5000 W target injected on the live bus reached the OpenDSS DER as
+5.000 kW under a matching correlation key:

| hop | observed |
|---|---|
| bus frame captured | 1 frame on `...input.gago094p2e` (baseline was 0) |
| bridge applied | `applied:1 skipped:0`, `value {multiplier:0, value:5000}` |
| DERControl served over mTLS | `<opModTargetW><multiplier>0</multiplier><value>5000</value>` |
| setpoint line | `{"txid": 848235397954, "opModTargetW": 5000.0}` |
| HELICS published | `t=7.0 p_set_w=5000.000 W case_id=848235397954` |
| OpenDSS measured | `+5.000 kW` (expected +5.000 kW) |

The reactive leg is NOT proven and is structurally unprovable through this
client: the C client advertises no `opModTargetVar`
(`map20305_der_sunspec_main.c:218`), and its `opModFixedVar` is a percent
type that cannot be converted to vars without a rated capability the
bridge does not seed.
