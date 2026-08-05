"""Sign loopback HELICS federate: the OpenDSS DER physics.

This value federate is the grid physics end of the co-simulation. It
subscribes to a commanded p/q setpoint published by the control shim
federate, applies it to the OpenDSS DER (see der_model.py), solves, and
publishes the MEASURED DER active/reactive power back, generator
positive. It echoes the case id it acted on so the shim can confirm each
measurement corresponds to the command it sent (the correspondence half
of the convergence check).

Time model: periodic value federate, period = 1.0 s, offset = 0,
uninterruptible (it only computes at integer grant boundaries, never
between). It marches in strict lockstep with the shim from t=1 to t=T.
A value published at time t is delivered at the peer's next grant, so a
command sent at t is applied here at t+1 and its measurement is visible
to the shim at t+2. See run_federation.py for the shared T.

Lane note: this federate owns the OpenDSS solve and the generator
positive measurement convention only. Whether that convention is the
electrically correct reading of 2030.5 is Vance's sign off; this harness
produces the evidence, it does not render the physics verdict.
"""
import sys

import helics as h

import der_model

PERIOD = 1.0


def main(total_steps):
    fi = h.helicsCreateFederateInfo()
    h.helicsFederateInfoSetCoreName(fi, "opendss_core")
    h.helicsFederateInfoSetCoreTypeFromString(fi, "zmq")
    h.helicsFederateInfoSetCoreInitString(fi, "--federates=1")
    h.helicsFederateInfoSetTimeProperty(fi, h.HELICS_PROPERTY_TIME_PERIOD, PERIOD)
    # Uninterruptible: grants land only on the period, never on an
    # asynchronous value arrival between periods.
    h.helicsFederateInfoSetFlagOption(fi, h.HELICS_FLAG_UNINTERRUPTIBLE, True)

    fed = h.helicsCreateValueFederate("opendss_der", fi)

    p_set = h.helicsFederateRegisterSubscription(fed, "shim/p_set_w", "W")
    q_set = h.helicsFederateRegisterSubscription(fed, "shim/q_set_var", "var")
    case_in = h.helicsFederateRegisterSubscription(fed, "shim/case_id", "")
    h.helicsInputSetDefaultDouble(p_set, 0.0)
    h.helicsInputSetDefaultDouble(q_set, 0.0)
    h.helicsInputSetDefaultDouble(case_in, -1.0)

    meas_p = h.helicsFederateRegisterGlobalPublication(fed, "der/meas_p_kw", h.HELICS_DATA_TYPE_DOUBLE, "kW")
    meas_q = h.helicsFederateRegisterGlobalPublication(fed, "der/meas_q_kvar", h.HELICS_DATA_TYPE_DOUBLE, "kvar")
    echo = h.helicsFederateRegisterGlobalPublication(fed, "der/echo_case", h.HELICS_DATA_TYPE_DOUBLE, "")

    der_model.build()

    h.helicsFederateEnterExecutingMode(fed)

    last_p_kw, last_q_kvar = 0.0, 0.0
    t = 0.0
    for _ in range(total_steps):
        t = h.helicsFederateRequestTime(fed, t + PERIOD)
        case_id = h.helicsInputGetDouble(case_in)
        if case_id >= 0.0:
            p_w = h.helicsInputGetDouble(p_set)
            q_v = h.helicsInputGetDouble(q_set)
            last_p_kw, last_q_kvar = der_model.apply_setpoint(p_w, q_v, label=f"case {int(case_id)}")
            h.helicsPublicationPublishDouble(echo, case_id)
        else:
            h.helicsPublicationPublishDouble(echo, -1.0)
        h.helicsPublicationPublishDouble(meas_p, last_p_kw)
        h.helicsPublicationPublishDouble(meas_q, last_q_kvar)

    # Clean exit handshake: announce completion so the broker coordinates
    # the final grant across both federates instead of dropping an
    # in flight message at teardown.
    h.helicsFederateRequestTime(fed, h.HELICS_TIME_MAXTIME)
    h.helicsFederateDisconnect(fed)
    h.helicsFederateDestroy(fed)


if __name__ == "__main__":
    main(int(sys.argv[1]))
