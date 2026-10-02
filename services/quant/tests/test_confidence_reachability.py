"""What a confidence score can and cannot reach, measured rather than argued.

Three strategies average a real component with a hard-coded constant, and the
control plane's consensus policy applies a 0.55 floor to the result. The
project's own record of why nothing trades said that `donchian_breakout`
"cannot clear a 0.55 floor whatever the market does -- not because the breakout
was weak, but because half its score is a constant."

That is not what the arithmetic does. `_confidence` is the MEAN of its
components, so blending a component with the constant 0.5 gives

    confidence = (min(1, penetration) + 0.5) / 2

which clears 0.55 at a penetration of 0.6 ATR and saturates at 0.75. The
measurements below cross that line. The observed 0.31-0.44 on the seeded data
was a weak-breakout result, not a ceiling.

The distinction decides what to fix. "Structurally impossible" invites removing
the strategy or lowering the floor. "Needs a 0.6 ATR break, which the test data
never produced" points at the data and at the fact that the constant throws
away half the dynamic range -- which is a calibration problem, and calibration
is research against realised outcomes, not a number moved until trades appear.

What the constant DOES cost is the top of the range, and that is real: no
amount of market can take a 0.5-blended score above 0.75. These tests pin both
the reachable floor and the unreachable ceiling, so the next person arguing
about the threshold is arguing with a measurement.
"""

from __future__ import annotations

import re

import numpy as np
import pandas as pd

from vantage_quant import strategies as strat

# The control plane's DefaultConsensusPolicy, duplicated deliberately.
#
# The research plane does not import the control plane and must not start. A
# copy that drifts is caught by the value appearing in a failure message next
# to the confidence it was compared against, which is the thing a reader needs
# in order to notice.
CONSENSUS_MIN_CONFIDENCE = 0.55


def breakout_series(bar_range: float, break_multiple: float, n: int = 200) -> pd.DataFrame:
    """A flat channel of constant-range bars, then one bar that breaks out.

    Constant range so the ATR is a known quantity rather than an artefact of a
    random walk: every true range is `bar_range`, so the Wilder average is too
    until the breakout bar arrives. That makes the penetration the test varies
    the ONLY thing that moves the confidence.
    """
    close = np.full(n, 2650.0)
    high = close + bar_range / 2
    low = close - bar_range / 2
    open_ = close.copy()

    channel_high = 2650.0 + bar_range / 2
    close[-1] = channel_high + bar_range * break_multiple
    high[-1] = close[-1]
    low[-1] = 2650.0
    open_[-1] = 2650.0

    index = pd.date_range("2025-01-06T00:00:00Z", periods=n, freq="1h")
    return pd.DataFrame(
        {"open": open_, "high": high, "low": low, "close": close,
         "volume": np.full(n, 1000.0)},
        index=index,
    )


def penetration_of(explanation: str) -> float:
    """The strategy states its own penetration; read it rather than recompute."""
    found = re.search(r"by ([0-9.]+)x ATR", explanation)
    assert found, f"explanation did not state a penetration: {explanation!r}"
    return float(found.group(1))


def test_a_constant_blend_halves_the_distance_to_the_top() -> None:
    """The mechanism, as a pure fact about the helper.

    This is what the constant actually costs: not the ability to clear a floor,
    but the top of the range. Every threshold above these values is unreachable
    by the strategies that use them, whatever the market does.
    """
    assert strat._confidence(1.0, 0.5) == 0.75
    assert strat._confidence(1.0, 0.45) == 0.725
    assert strat._confidence(1.0, 0.6) == 0.8

    # And the floor the constant imposes on the real component: to average 0.55
    # with a 0.5 constant, the other half has to reach 0.6.
    assert strat._confidence(0.6, 0.5) == 0.55
    assert strat._confidence(0.59, 0.5) < CONSENSUS_MIN_CONFIDENCE


def test_donchian_breakout_can_clear_the_consensus_floor() -> None:
    """The falsification. A strong break produces a signal above 0.55.

    If this ever fails, the strategy really has become incapable of clearing
    the floor and the original claim has become true -- which would be worth
    knowing, and is exactly why it is asserted rather than described.
    """
    signal, _ = strat.evaluate("donchian_breakout", breakout_series(4.0, 0.75), None, None)

    assert signal.action == "buy"
    assert signal.confidence >= CONSENSUS_MIN_CONFIDENCE, (
        f"a {penetration_of(signal.explanation)}x ATR break produced "
        f"{signal.confidence:.4f}, below the {CONSENSUS_MIN_CONFIDENCE} consensus floor"
    )


def test_the_confidence_tracks_the_break_and_saturates_at_the_blend_ceiling() -> None:
    """The whole curve, so the shape is recorded and not just one point.

    Weak breaks sit under the floor, which is correct and is what the seeded
    data produced. Strong ones clear it. Extreme ones stop at 0.75 because the
    real component is clamped at 1.0 and is averaged with 0.5.
    """
    measured = []
    for multiple in (0.2, 0.4, 0.6, 0.75, 1.0, 5.0):
        signal, _ = strat.evaluate(
            "donchian_breakout", breakout_series(4.0, multiple), None, None)
        measured.append((penetration_of(signal.explanation), signal.confidence))

    # Monotonic in the break size, up to the ceiling.
    confidences = [c for _, c in measured]
    assert confidences == sorted(confidences), f"not monotonic: {measured}"

    # The band around 0.6 ATR is deliberately excluded.
    #
    # The penetration read back here comes from the explanation, which states
    # it to two decimal places. A displayed "0.60" is anything in [0.595,
    # 0.605), and the floor sits exactly at 0.600 -- so a sample at the
    # boundary cannot be classified from the displayed value, and the first
    # version of this test failed on precisely that: a true 0.5957 printed as
    # 0.60 and correctly produced 0.5479. The arithmetic was right and the
    # assertion was wrong. Asserting either side of the band tests the claim
    # without pretending to a precision the explanation does not carry.
    weak = [c for pen, c in measured if pen < 0.55]
    strong = [c for pen, c in measured if pen > 0.65]
    assert weak and strong, f"the sweep did not straddle the floor: {measured}"
    assert max(weak) < CONSENSUS_MIN_CONFIDENCE, (
        f"a clearly weak break cleared the floor: {measured}")
    assert min(strong) >= CONSENSUS_MIN_CONFIDENCE, (
        f"a clearly strong break failed the floor: {measured}")

    # The ceiling is the constant's real cost, and no market gets past it.
    assert max(confidences) == 0.75, f"ceiling moved: {measured}"

    # And the formula the comment in consensus.go now states.
    for pen, conf in measured:
        assert abs(conf - (min(1.0, pen) + 0.5) / 2) < 0.01, (
            f"penetration {pen} gave {conf}, not (min(1,pen)+0.5)/2")
