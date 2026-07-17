"""GAGO-044 HELICS federate: the bridge control shim.

This value federate stands in for the bridge's shipped control publish
path. It does NOT reimplement the sign logic: it loads bridge_capture.json,
the genuine output of the real ApplyControlDelta control path (written by
the Go test TestGAGO044CaptureBridgeSetpoints), and drives those exact
target values into the federation. The OpenDSS federate then reports what
the DER physically does with them.

It publishes one case per two time steps (a command needs one step to be
delivered to OpenDSS and another for the measurement to return), collects
each measurement keyed by the echoed case id, and writes the paired
result table to the path named by the first CLI argument.

Time model: periodic value federate, period = 1.0 s, offset = 0,
uninterruptible, lockstep with the OpenDSS federate from t=1 to t=T.
"""
import json
import sys

import helics as h

PERIOD = 1.0


def main(capture_path, result_path, total_steps):
    with open(capture_path) as f:
        capture = json.load(f)
    cases = capture["cases"]

    fi = h.helicsCreateFederateInfo()
    h.helicsFederateInfoSetCoreName(fi, "shim_core")
    h.helicsFederateInfoSetCoreTypeFromString(fi, "zmq")
    h.helicsFederateInfoSetCoreInitString(fi, "--federates=1")
    h.helicsFederateInfoSetTimeProperty(fi, h.HELICS_PROPERTY_TIME_PERIOD, PERIOD)
    h.helicsFederateInfoSetFlagOption(fi, h.HELICS_FLAG_UNINTERRUPTIBLE, True)

    fed = h.helicsCreateValueFederate("control_shim", fi)

    p_pub = h.helicsFederateRegisterGlobalPublication(fed, "shim/p_set_w", h.HELICS_DATA_TYPE_DOUBLE, "W")
    q_pub = h.helicsFederateRegisterGlobalPublication(fed, "shim/q_set_var", h.HELICS_DATA_TYPE_DOUBLE, "var")
    case_pub = h.helicsFederateRegisterGlobalPublication(fed, "shim/case_id", h.HELICS_DATA_TYPE_DOUBLE, "")

    meas_p = h.helicsFederateRegisterSubscription(fed, "der/meas_p_kw", "kW")
    meas_q = h.helicsFederateRegisterSubscription(fed, "der/meas_q_kvar", "kvar")
    echo_in = h.helicsFederateRegisterSubscription(fed, "der/echo_case", "")
    h.helicsInputSetDefaultDouble(echo_in, -1.0)

    h.helicsFederateEnterExecutingMode(fed)

    # Command k is published at t = 2k + 1.
    publish_time = {2 * k + 1: k for k in range(len(cases))}
    measured = {}  # echoed case id -> (p_kw, q_kvar)

    t = 0.0
    for _ in range(total_steps):
        t = h.helicsFederateRequestTime(fed, t + PERIOD)
        step = int(round(t))

        # Collect any measurement that has arrived, keyed by its echo.
        echoed = int(round(h.helicsInputGetDouble(echo_in)))
        if echoed >= 0 and echoed not in measured:
            measured[echoed] = (
                h.helicsInputGetDouble(meas_p),
                h.helicsInputGetDouble(meas_q),
            )

        # Publish this step's command, or idle.
        if step in publish_time:
            k = publish_time[step]
            c = cases[k]
            h.helicsPublicationPublishDouble(p_pub, float(c["bridge_target_w"]))
            h.helicsPublicationPublishDouble(q_pub, float(c["bridge_target_var"]))
            h.helicsPublicationPublishDouble(case_pub, float(c["id"]))
        else:
            h.helicsPublicationPublishDouble(case_pub, -1.0)

    # Clean exit handshake: announce completion so the broker coordinates
    # the final grant across both federates instead of dropping an
    # in flight message at teardown.
    h.helicsFederateRequestTime(fed, h.HELICS_TIME_MAXTIME)
    h.helicsFederateDisconnect(fed)
    h.helicsFederateDestroy(fed)

    results = []
    for c in cases:
        p_kw, q_kvar = measured.get(c["id"], (None, None))
        results.append({
            "id": c["id"],
            "name": c["name"],
            "attribute": c["attribute"],
            "commanded_value": c["commanded_value"],
            "bridge_target_w": c["bridge_target_w"],
            "bridge_target_var": c["bridge_target_var"],
            "measured_p_kw": p_kw,
            "measured_q_kvar": q_kvar,
        })

    with open(result_path, "w") as f:
        json.dump({
            "activeSignFlip": capture["activeSignFlip"],
            "reactiveSignFlip": capture["reactiveSignFlip"],
            "results": results,
        }, f, indent=2)


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2], int(sys.argv[3]))
