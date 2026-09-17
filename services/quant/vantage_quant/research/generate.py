"""Run every strategy over every permitted dataset and record what it said.

This is the observation half. It walks each fixture bar by bar, asks each
strategy for its opinion using exactly the code the control plane calls, and
records an observation for every actionable answer -- whether or not consensus,
risk or authority would have accepted it.

That last point is the whole design. Recording only signals that became orders
would measure the current policy, and the current policy executes almost
nothing: the sample would be both tiny and selected by the very thresholds
under examination.

Warm-up is recorded and marked, never analysed. Indicators are still filling
through it, and the platform suppresses trading there for the same reason.
"""

from __future__ import annotations

import subprocess
from dataclasses import dataclass
from pathlib import Path

import pandas as pd

from vantage_quant import strategies
from vantage_quant.scanner import classify_regime

from .observation import SignalObservation, observation_id
from .partition import DatasetAssignment

#: Bars a strategy must see before its opinion counts as evaluation evidence.
#:
#: The control plane's own warm-up is 60 market instants and its orchestrator
#: refuses below 50 complete bars. 60 is used here so research and production
#: agree about when a strategy has enough history to be taken seriously.
WARMUP_BARS = 60

#: Bars of history handed to a strategy. The orchestrator passes 300; the
#: fixtures are shorter than that, so every call simply receives everything
#: available up to and including the signal bar.
MAX_LOOKBACK = 300


@dataclass(frozen=True)
class StrategySignals:
    """Observations plus the levels needed to score them.

    The stop and target are kept ALONGSIDE the observation rather than inside
    it: they are the strategy's own declared levels, they are needed to resolve
    target-before-stop, and putting them on the observation would blur the line
    between "what was said" and "what is needed to score it".
    """

    observation: SignalObservation
    suggested_stop: float | None
    suggested_target: float | None


@dataclass(frozen=True)
class DatasetBars:
    dataset_id: str
    dataset_hash: str
    frame: pd.DataFrame


def code_sha() -> str:
    """The commit these observations were produced under."""
    try:
        out = subprocess.run(
            ["git", "rev-parse", "--short", "HEAD"],
            capture_output=True,
            text=True,
            check=True,
            timeout=10,
        )
        return out.stdout.strip()
    except (subprocess.SubprocessError, OSError):
        return "unknown"


def load_fixture(path: Path, assignment: DatasetAssignment) -> DatasetBars:
    """Read one fixture into the frame shape the strategies expect."""
    raw = pd.read_csv(path)
    frame = pd.DataFrame(
        {
            "open": raw["open"].astype(float),
            "high": raw["high"].astype(float),
            "low": raw["low"].astype(float),
            "close": raw["close"].astype(float),
            "volume": raw["volume"].astype(float),
        }
    )
    frame.index = pd.to_datetime(raw["timestamp"], utc=True, format="ISO8601")
    frame.attrs["session"] = raw["session"].tolist()
    frame.attrs["spread_fraction"] = raw["spread_fraction"].astype(float).tolist()
    return DatasetBars(
        dataset_id=assignment.dataset_id,
        dataset_hash=assignment.dataset_hash,
        frame=frame,
    )


def _trailing_volatility(frame: pd.DataFrame, upto: int, window: int = 20) -> float:
    """Realised volatility over the bars BEFORE `upto`, never including after.

    Deliberately exclusive of the signal bar's own forward move: a volatility
    that peeked at the next bar would be a future field wearing a context
    field's name.
    """
    start = max(0, upto - window)
    closes = frame["close"].iloc[start:upto]
    if len(closes) < 2:
        return 0.0
    returns = closes.pct_change().dropna()
    return float(returns.std()) if not returns.empty else 0.0


def observe_dataset(
    spec: strategies.StrategySpec,
    bars: DatasetBars,
    *,
    version: int = 1,
    sha: str = "",
) -> list[StrategySignals]:
    """Every observation one strategy produces over one dataset."""
    frame = bars.frame
    sessions: list[str] = frame.attrs.get("session", [])
    spreads: list[float] = frame.attrs.get("spread_fraction", [])
    out: list[StrategySignals] = []

    # `i` is the index of the signal bar. The strategy sees bars[0..i]
    # inclusive and nothing after; the slice is the information boundary and a
    # test asserts it never reaches further.
    for i in range(len(frame)):
        history = frame.iloc[max(0, i + 1 - MAX_LOOKBACK) : i + 1]
        if len(history) < spec.required_bars:
            continue

        session = sessions[i] if i < len(sessions) else "unknown"
        spread = spreads[i] if i < len(spreads) else 0.0
        ctx = strategies.StrategyContext(
            session=session, spread_fraction=spread, event_risk="none"
        )

        try:
            signal = spec.generate(history, dict(spec.default_parameters), ctx)
        except Exception:  # a strategy that raises is not evidence
            continue

        if signal.action not in ("buy", "sell"):
            continue

        # The fill is the NEXT bar's open. Without a next bar there is no
        # executable entry, so there is nothing to score.
        if i + 1 >= len(frame):
            continue

        from .outcome import entry_fill_price, research_cost_model

        next_open = float(frame.iloc[i + 1]["open"])
        entry = entry_fill_price(
            direction=signal.action,
            next_open=next_open,
            costs=research_cost_model(),
        )

        regime, _ = classify_regime(history)
        bar_time = frame.index[i].to_pydatetime()

        out.append(
            StrategySignals(
                suggested_stop=(
                    float(signal.suggested_stop)
                    if signal.suggested_stop is not None
                    else None
                ),
                suggested_target=(
                    float(signal.suggested_target)
                    if signal.suggested_target is not None
                    else None
                ),
                observation=SignalObservation(
                    observation_id=observation_id(
                        strategy_id=spec.key,
                        dataset_id=bars.dataset_id,
                        bar_time=bar_time,
                        direction=signal.action,
                    ),
                    strategy_id=spec.key,
                    strategy_version=version,
                    strategy_family=spec.family,
                    instrument="XAUUSD.m",
                    direction=signal.action,
                    bar_time=bar_time,
                    raw_score=float(signal.confidence),
                    confidence_kind="raw_score",
                    score_components={
                        k: float(v)
                        for k, v in signal.features.items()
                        if isinstance(v, (int, float)) and pd.notna(v)
                    },
                    regime=regime,
                    session=session,
                    spread_fraction=float(spread),
                    trailing_volatility=_trailing_volatility(frame, i),
                    event_risk="none",
                    dataset_id=bars.dataset_id,
                    dataset_hash=bars.dataset_hash,
                    code_sha=sha,
                    evaluation_phase="warmup" if i < WARMUP_BARS else "evaluation",
                    entry_reference=entry,
                ),
            )
        )
    return out

