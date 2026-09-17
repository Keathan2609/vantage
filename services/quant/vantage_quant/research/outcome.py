"""What happened next, computed only once "next" has elapsed.

A SignalOutcome is derived from bars STRICTLY AFTER the observation's own bar.
It is a separate type in a separate module because the separation is the
control: strategy code cannot reach it, ``observation.py`` does not import it,
and an analysis that wants a forward return has to say so by asking for this.

Every outcome is directional. A SELL that is followed by a fall is a good
outcome and its number is positive, so "higher is better" holds for both sides
and a mixed-direction analysis does not silently cancel itself out.

Costs use the platform's own execution assumptions rather than a research
model invented here. A signal that is directionally right and cannot pay the
spread is not a useful signal, and measuring it against cheaper costs than the
venue charges would hide exactly that.
"""

from __future__ import annotations

from dataclasses import dataclass

import pandas as pd

from vantage_quant.backtest import CostModel

from .observation import SignalObservation

#: Bumped when the cost assumptions change, so two analyses fitted against
#: different costs are not compared as though they agreed.
COST_POLICY_VERSION = 1


def research_cost_model() -> CostModel:
    """The costs an outcome is charged.

    The platform's OWN defaults, deliberately: 0.00012 spread fraction and
    0.0001 slippage, the same numbers the backtester applies. Inventing
    cheaper research costs would produce an edge that disappears the moment
    anything trades.
    """
    return CostModel()


def entry_fill_price(
    *, direction: str, next_open: float, costs: CostModel, price_precision: int = 2
) -> float:
    """The price a hypothetical entry would actually have received.

    NOT the signal bar's close. A signal produced at a bar's close can only be
    acted on at the next bar's open, and it crosses the spread and suffers
    slippage getting there. Using the close would credit the strategy with a
    price nobody could have traded at, which flatters every result uniformly
    and most flatters the fastest signals.

    This mirrors ``backtest._open_position`` exactly rather than
    approximating it, so a research outcome and a backtest fill agree.
    """
    half_spread = next_open * costs.spread_fraction / 2.0
    slip = next_open * costs.slippage_fraction
    fill = (
        next_open + half_spread + slip
        if direction == "buy"
        else next_open - half_spread - slip
    )
    return round(fill, price_precision)


def exit_fill_price(
    *, direction: str, close: float, costs: CostModel, price_precision: int = 2
) -> float:
    """The price a hypothetical exit would have received.

    The other half of the spread, on the other side: a long exits by selling
    into the bid, a short exits by buying the ask. Charging only the entry
    would halve the real cost of a round trip.
    """
    half_spread = close * costs.spread_fraction / 2.0
    slip = close * costs.slippage_fraction
    fill = (
        close - half_spread - slip
        if direction == "buy"
        else close + half_spread + slip
    )
    return round(fill, price_precision)


@dataclass(frozen=True)
class SignalOutcome:
    """What the market did after one observation, over one horizon."""

    observation_id: str
    horizon_id: str
    horizon_bars: int

    #: Directional, as a fraction of the entry price. Positive means the market
    #: moved the way the signal pointed.
    forward_raw_return: float
    #: The same move after a full round trip's spread and slippage.
    forward_net_return: float

    #: The best and worst the position ever stood, within the horizon. These
    #: separate "right direction, bad timing" from "simply wrong": a signal
    #: with a large MFE and a negative close was correct and mistimed.
    maximum_favorable_excursion: float
    maximum_adverse_excursion: float

    positive_raw_outcome: bool
    positive_net_outcome: bool

    #: Whether the strategy's OWN stop or target was reached first, where it
    #: declared them. None where the strategy declared neither -- inventing
    #: generic levels would measure a strategy nobody wrote.
    target_hit: bool | None = None
    stop_hit: bool | None = None
    bars_to_target: int | None = None
    bars_to_stop: int | None = None

    #: The number of forward bars actually available. Shorter than
    #: ``horizon_bars`` only at the very end of a dataset, and such an outcome
    #: is excluded from analysis rather than scored over a truncated window.
    bars_observed: int = 0

    cost_policy_version: int = COST_POLICY_VERSION

    @property
    def is_complete(self) -> bool:
        """Whether the full horizon actually elapsed."""
        return self.bars_observed >= self.horizon_bars


def compute_outcome(
    observation: SignalObservation,
    *,
    future_bars: pd.DataFrame,
    horizon_id: str,
    horizon_bars: int,
    suggested_stop: float | None,
    suggested_target: float | None,
    costs: CostModel | None = None,
    price_precision: int = 2,
) -> SignalOutcome | None:
    """Score one observation over one horizon.

    ``future_bars`` must contain ONLY bars strictly after the observation's
    bar; the caller slices them and a test proves the slice never includes the
    signal's own bar. Returns None when the horizon has not elapsed -- an
    incomplete window is missing data, not a zero.
    """
    costs = costs or research_cost_model()
    if future_bars.empty:
        return None

    window = future_bars.iloc[:horizon_bars]
    if len(window) < horizon_bars:
        # The horizon has not elapsed. Scoring what exists would quietly
        # shorten the window for every signal near a dataset's end, and those
        # are not a random sample.
        return None

    entry = observation.entry_reference
    if entry <= 0:
        return None

    direction = observation.direction
    sign = 1.0 if direction == "buy" else -1.0

    final_close = float(window.iloc[-1]["close"])
    raw_return = sign * (final_close - entry) / entry

    exit_price = exit_fill_price(
        direction=direction, close=final_close, costs=costs, price_precision=price_precision
    )
    net_return = sign * (exit_price - entry) / entry

    # Excursions, measured against the extremes the position actually saw.
    highs = window["high"].astype(float)
    lows = window["low"].astype(float)
    if direction == "buy":
        mfe = (float(highs.max()) - entry) / entry
        mae = (float(lows.min()) - entry) / entry
    else:
        mfe = (entry - float(lows.min())) / entry
        mae = (entry - float(highs.max())) / entry

    target_hit, stop_hit, bars_to_target, bars_to_stop = _resolve_levels(
        window=window,
        direction=direction,
        stop=suggested_stop,
        target=suggested_target,
    )

    return SignalOutcome(
        observation_id=observation.observation_id,
        horizon_id=horizon_id,
        horizon_bars=horizon_bars,
        forward_raw_return=raw_return,
        forward_net_return=net_return,
        maximum_favorable_excursion=mfe,
        maximum_adverse_excursion=mae,
        positive_raw_outcome=raw_return > 0,
        positive_net_outcome=net_return > 0,
        target_hit=target_hit,
        stop_hit=stop_hit,
        bars_to_target=bars_to_target,
        bars_to_stop=bars_to_stop,
        bars_observed=len(window),
    )


def _resolve_levels(
    *,
    window: pd.DataFrame,
    direction: str,
    stop: float | None,
    target: float | None,
) -> tuple[bool | None, bool | None, int | None, int | None]:
    """Which of the strategy's own levels was reached first.

    CONSERVATIVE when both sit inside one bar's range: the stop is taken as
    hit. Intrabar order is unknowable from OHLC, and assuming the favourable
    leg would let every ambiguous bar count as a win -- which is the single
    easiest way to manufacture an edge that does not exist. This matches the
    backtester's existing policy rather than inventing a second one.
    """
    if stop is None and target is None:
        return None, None, None, None

    for i, (_, bar) in enumerate(window.iterrows(), start=1):
        high = float(bar["high"])
        low = float(bar["low"])

        if direction == "buy":
            stop_touched = stop is not None and low <= stop
            target_touched = target is not None and high >= target
        else:
            stop_touched = stop is not None and high >= stop
            target_touched = target is not None and low <= target

        if stop_touched and target_touched:
            return False, True, None, i
        if stop_touched:
            return False, True, None, i
        if target_touched:
            return True, False, i, None

    return False, False, None, None
