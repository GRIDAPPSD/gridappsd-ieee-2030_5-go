"""OpenDSS controllable DER model for the GAGO-044 sign loopback.

A minimal three phase feeder: a source, a short line, and one Generator
that stands in for the 2030.5 controlled DER inverter. OpenDSS Generator
kW and kvar parameters are generator positive: kW>0 exports real power,
kvar>0 injects reactive power (over excited). This matches IEEE 2030.5
opModTargetW positive = discharge and opModTargetVar positive = inject.

apply_setpoint drives a commanded p (watts) and q (vars) onto the DER and
returns the MEASURED DER active/reactive power read back from the solved
circuit, reported generator positive (power delivered BY the DER to the
grid), so a discharging DER reads positive P and an injecting DER reads
positive Q.
"""
import opendssdirect as dss


def build():
    dss.Command("clear")
    dss.Command("new circuit.gago044 basekv=0.416 pu=1.0 phases=3 bus1=sourcebus")
    dss.Command("new line.feeder bus1=sourcebus bus2=derbus phases=3 "
                "length=0.05 r1=0.3 x1=0.6 units=km")
    # DER inverter as a Generator, model=1 (constant P,Q). kVA rating
    # generous so neither target saturates. Start at zero output.
    dss.Command("new generator.der bus1=derbus phases=3 kv=0.416 "
                "kw=0 kvar=0 model=1 kva=100 conn=wye")
    dss.Command("set mode=snapshot")


def apply_setpoint(p_watts, q_vars, label=""):
    """Command the DER to p_watts / q_vars (generator positive) and return
    the measured (p_kw, q_kvar) the DER actually delivers, generator
    positive.

    Raises RuntimeError if the OpenDSS solve did not converge. Returning
    the last iteration values from a non converged solve as if they were
    fact would silently produce a FALSE directional verdict, exactly the
    silent-wrong-data failure the data-invariants rule exists to prevent,
    so the guard fails loud instead of returning stale power."""
    dss.Command(f"edit generator.der kw={p_watts / 1000.0} kvar={q_vars / 1000.0}")
    dss.Command("solve")
    if not dss.Solution.Converged():
        raise RuntimeError(
            f"OpenDSS solve did not converge for case {label!r} "
            f"(commanded p={p_watts}W q={q_vars}var); refusing to report "
            f"stale power as a measurement"
        )
    dss.Circuit.SetActiveElement("generator.der")
    powers = dss.CktElement.Powers()  # power INTO the element from the grid
    p_into = sum(powers[0::2])
    q_into = sum(powers[1::2])
    # Generator positive: power delivered BY the DER is the negative of
    # power flowing into the element.
    return -p_into, -q_into


if __name__ == "__main__":
    build()
    for name, pw, qv in [("discharge", 5000.0, 0.0), ("var_inject", 0.0, 3000.0),
                          ("charge", -5000.0, 0.0), ("absorb", 0.0, -3000.0)]:
        p_kw, q_kvar = apply_setpoint(pw, qv, label=name)
        print(f"{name:10s} commanded p={pw:+8.0f}W q={qv:+8.0f}var "
              f"-> measured P={p_kw:+7.3f}kW  Q={q_kvar:+7.3f}kvar")
