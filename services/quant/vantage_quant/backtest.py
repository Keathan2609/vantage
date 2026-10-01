"""Event-driven backtester.

A backtest is a claim about what would have happened. Most backtests are wrong
in the same few ways, and this one is built specifically to avoid them:

**Execution timing.** A strategy that sees bar *i*'s close cannot trade at that
close -- the close is the last price of a bar that has just finished. Entries
therefore fill at the NEXT bar's open, crossing the spread, plus slippage.
Filling at the signal bar's close is the single most flattering error available
and it silently inflates every result.

**Same-bar stop and target.** When a bar's range covers both the stop and the
target, bar data cannot say which came first. This engine always assumes the
STOP was hit. That is pessimistic by construction, which is the only safe
direction for an assumption about your own losses.

**Costs.** Spread, slippage, commission and overnight financing are applied to
every trade. A frictionless run is available but is flagged ``frictionless``,
and the flag travels with the stored result so it can never be read as a
realistic expectation.

**Position sizing.** Size comes from the account's risk fraction and the stop
distance, floored onto the instrument's quantity step. When the risk budget
cannot buy the minimum tradable size, the trade is skipped and counted -- for a
small account that is the honest outcome, and hiding it would misrepresent what
the strategy could actually have done.

The engine shares its financial semantics with the paper execution path:
crossing the spread, flooring quantities onto the step, and refusing
unaffordable trades. Where the two differ, that difference is a bug in one of
them.
"""

from __future__ import annotations

import hashlib
import math
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any, Literal

import numpy as np
import pandas as pd

from vantage_quant import strategies as strat

SampleKind = Literal["in_sample", "validation", "out_of_sample", "walk_forward", "paper_forward"]


@dataclass
class CostModel:
    """Trading frictions.

    ``spread_fraction`` is the full bid-ask spread as a fraction of price; half
    is paid on entry and half on exit. ``slippage_fraction`` is additional
    adverse movement on market orders.
    """

    spread_fraction: float = 0.00012
    slippage_fraction: float = 0.0001
    commission_per_lot: float = 0.0
    swap_long_per_lot: float = 0.0
    swap_short_per_lot: float = 0.0

    @property
    def frictionless(self) -> bool:
        return (
            self.spread_fraction == 0.0
            and self.slippage_fraction == 0.0
            and self.commission_per_lot == 0.0
        )


@dataclass
class InstrumentModel:
    """The venue arithmetic a backtest must respect."""

    contract_size: float = 1.0
    min_quantity: float = 0.01
    quantity_step: float = 0.01
    price_precision: int = 2

    def normalise_quantity(self, quantity: float) -> float:
        """Floor onto the step, never round up.

        Rounding up would take more risk than the sizing policy authorised,
        which is the same rule the live sizing path follows.
        """
        if self.quantity_step <= 0:
            return round(quantity, 8)
        steps = math.floor(quantity / self.quantity_step + 1e-9)
        return round(steps * self.quantity_step, 8)


@dataclass
class Trade:
    """One completed or open round trip."""

    side: str
    quantity: float
    entry_time: datetime
    entry_price: float
    exit_time: datetime | None = None
    exit_price: float | None = None
    gross_pnl: float = 0.0
    commission: float = 0.0
    slippage: float = 0.0
    swap: float = 0.0
    net_pnl: float = 0.0
    mae: float = 0.0  # maximum adverse excursion, in price units
    mfe: float = 0.0  # maximum favourable excursion
    exit_reason: str = ""
    bars_held: int = 0

    def to_dict(self) -> dict[str, Any]:
        return {
            "side": self.side,
            "quantity": f"{self.quantity}",
            "entry_time": self.entry_time.isoformat(),
            "entry_price": f"{self.entry_price}",
            "exit_time": self.exit_time.isoformat() if self.exit_time else None,
            "exit_price": f"{self.exit_price}" if self.exit_price is not None else None,
            "gross_pnl": f"{self.gross_pnl:.6f}",
            "commission": f"{self.commission:.6f}",
            "slippage": f"{self.slippage:.6f}",
            "swap": f"{self.swap:.6f}",
            "net_pnl": f"{self.net_pnl:.6f}",
            "mae": f"{self.mae:.6f}",
            "mfe": f"{self.mfe:.6f}",
            "exit_reason": self.exit_reason,
        }


@dataclass
class BacktestConfig:
    strategy_key: str
    parameters: dict[str, Any] = field(default_factory=dict)
    initial_capital: float = 500.0
    risk_per_trade: float = 0.01
    costs: CostModel = field(default_factory=CostModel)
    instrument: InstrumentModel = field(default_factory=InstrumentModel)
    sample_kind: SampleKind = "out_of_sample"
    # quote_to_account converts one unit of quote currency into the account's
    # currency. Held constant across a run: a backtest that also modelled FX
    # drift would be measuring two strategies at once.
    quote_to_account: float = 1.0
    session: str = "unknown"
    seed: int = 0
    max_bars_held: int = 0  # 0 means no time-based exit


@dataclass
class BacktestResult:
    metrics: dict[str, Any]
    equity_curve: list[dict[str, Any]]
    trades: list[Trade]
    warnings: list[str]
    dataset_hash: str
    code_hash: str
    frictionless: bool
    skipped_unaffordable: int


def run(bars: pd.DataFrame, config: BacktestConfig) -> BacktestResult:
    """Run a backtest over completed bars."""
    _validate_bars(bars)

    spec = strat.get(config.strategy_key)
    params = strat.merged_params(spec, config.parameters)
    ctx = strat.StrategyContext(session=config.session)

    warm_up = max(spec.required_bars, 30)
    if len(bars) <= warm_up + 5:
        raise ValueError(
            f"backtest needs more than {warm_up + 5} bars for "
            f"{config.strategy_key!r}; received {len(bars)}"
        )

    equity = config.initial_capital
    peak_equity = equity
    open_trade: Trade | None = None
    open_stop: float | None = None
    open_target: float | None = None

    trades: list[Trade] = []
    curve: list[dict[str, Any]] = []
    skipped_unaffordable = 0
    signal_count = 0

    costs = config.costs

    # The loop stops one bar early because an entry signalled on the final bar
    # would fill on a bar that does not exist. Pretending otherwise would add a
    # free trade at the end of every run.
    for i in range(warm_up, len(bars) - 1):
        history = bars.iloc[: i + 1]
        next_bar = bars.iloc[i + 1]
        bar_time = bars.index[i + 1]

        # --- Manage an open position first -------------------------------
        if open_trade is not None:
            open_trade.bars_held += 1
            high = float(next_bar["high"])
            low = float(next_bar["low"])

            if open_trade.side == "buy":
                open_trade.mae = max(open_trade.mae, open_trade.entry_price - low)
                open_trade.mfe = max(open_trade.mfe, high - open_trade.entry_price)
                stop_hit = open_stop is not None and low <= open_stop
                target_hit = open_target is not None and high >= open_target
            else:
                open_trade.mae = max(open_trade.mae, high - open_trade.entry_price)
                open_trade.mfe = max(open_trade.mfe, open_trade.entry_price - low)
                stop_hit = open_stop is not None and high >= open_stop
                target_hit = open_target is not None and low <= open_target

            # The level that was hit is carried alongside the fact that it was
            # hit, so "a stop was touched" and "this is the stop price" cannot
            # drift apart -- and the None checks below narrow the type for free.
            hit_stop = open_stop if stop_hit else None
            hit_target = open_target if target_hit else None

            exit_price: float | None = None
            reason = ""
            if hit_stop is not None and hit_target is not None:
                # Bar data cannot resolve the order of two intrabar events.
                # Assume the stop: the pessimistic reading is the only safe one.
                exit_price, reason = float(hit_stop), "stop_loss (stop assumed first)"
            elif hit_stop is not None:
                exit_price, reason = float(hit_stop), "stop_loss"
            elif hit_target is not None:
                exit_price, reason = float(hit_target), "take_profit"
            elif config.max_bars_held and open_trade.bars_held >= config.max_bars_held:
                exit_price, reason = float(next_bar["open"]), "max_bars_held"

            if exit_price is None:
                # Still open: check whether the strategy wants out.
                signal, _ = _evaluate(spec, history, params, ctx)
                wants_out = signal.action == "close" or (
                    signal.action in ("buy", "sell") and signal.action != open_trade.side
                )
                if wants_out:
                    exit_price, reason = float(next_bar["open"]), "strategy_exit"

            if exit_price is not None:
                equity = _close_trade(
                    open_trade, exit_price, bar_time, reason, equity, config
                )
                trades.append(open_trade)
                open_trade, open_stop, open_target = None, None, None

        # --- Consider a new entry ----------------------------------------
        if open_trade is None:
            signal, _ = _evaluate(spec, history, params, ctx)
            if signal.action in ("buy", "sell"):
                signal_count += 1
                trade, stop, target, affordable = _open_trade(
                    signal, next_bar, bar_time, equity, config
                )
                if not affordable:
                    skipped_unaffordable += 1
                elif trade is not None:
                    open_trade, open_stop, open_target = trade, stop, target

        # --- Mark to market ----------------------------------------------
        mark = _mark_to_market(open_trade, float(next_bar["close"]), config)
        current_equity = equity + mark
        peak_equity = max(peak_equity, current_equity)
        curve.append(
            {
                "time": bar_time.isoformat(),
                "equity": round(current_equity, 6),
                "realised": round(equity, 6),
                "open_pnl": round(mark, 6),
                "drawdown": round(
                    0.0 if peak_equity <= 0 else (peak_equity - current_equity) / peak_equity, 6
                ),
            }
        )

    # An open position at the end of the series is closed at the last close so
    # the result reflects a settled account rather than an unrealised hope.
    if open_trade is not None:
        equity = _close_trade(
            open_trade,
            float(bars.iloc[-1]["close"]),
            bars.index[-1],
            "end_of_data",
            equity,
            config,
        )
        trades.append(open_trade)

    metrics = compute_metrics(trades, curve, config)
    warnings = _warnings(trades, metrics, config, signal_count, skipped_unaffordable, len(bars))

    return BacktestResult(
        metrics=metrics,
        equity_curve=curve,
        trades=trades,
        warnings=warnings,
        dataset_hash=dataset_hash(bars),
        code_hash=spec.code_hash,
        frictionless=costs.frictionless,
        skipped_unaffordable=skipped_unaffordable,
    )


def _evaluate(
    spec: strat.StrategySpec,
    history: pd.DataFrame,
    params: dict[str, Any],
    ctx: strat.StrategyContext,
) -> tuple[strat.Signal, strat.StrategySpec]:
    try:
        return spec.generate(history, params, ctx), spec
    except Exception as exc:  # a strategy fault must not silently become a trade
        return strat.no_trade(f"strategy raised {type(exc).__name__}: {exc}"), spec


def _open_trade(
    signal: strat.Signal,
    next_bar: pd.Series,
    bar_time: datetime,
    equity: float,
    config: BacktestConfig,
) -> tuple[Trade | None, float | None, float | None, bool]:
    """Open a position at the next bar's open, with costs and sizing applied."""
    side = signal.action
    costs = config.costs
    inst = config.instrument

    reference = float(next_bar["open"])
    half_spread = reference * costs.spread_fraction / 2.0
    slip = reference * costs.slippage_fraction

    # Crossing the spread and suffering slippage, both adverse.
    fill = reference + half_spread + slip if side == "buy" else reference - half_spread - slip
    fill = round(fill, inst.price_precision)
    if fill <= 0:
        return None, None, None, True

    stop = signal.suggested_stop
    if stop is None:
        # Without a stop there is no risk-based size. Skipping is deliberate:
        # inventing a stop would measure a strategy nobody wrote.
        return None, None, None, True

    stop_distance = abs(fill - stop)
    if stop_distance <= 0:
        return None, None, None, True

    risk_budget = equity * config.risk_per_trade
    loss_per_lot = stop_distance * inst.contract_size * config.quote_to_account
    if loss_per_lot <= 0:
        return None, None, None, True

    raw_quantity = risk_budget / loss_per_lot
    quantity = inst.normalise_quantity(raw_quantity)

    if quantity < inst.min_quantity:
        # The risk budget cannot buy the smallest tradable size. This is the
        # normal case for a very small account, and it is counted rather than
        # quietly rounded up.
        return None, None, None, False

    commission = costs.commission_per_lot * quantity * config.quote_to_account
    slippage_cost = slip * quantity * inst.contract_size * config.quote_to_account

    trade = Trade(
        side=side,
        quantity=quantity,
        entry_time=bar_time,
        entry_price=fill,
        commission=commission,
        slippage=slippage_cost,
    )
    return trade, stop, signal.suggested_target, True


def _close_trade(
    trade: Trade,
    exit_price: float,
    exit_time: datetime,
    reason: str,
    equity: float,
    config: BacktestConfig,
) -> float:
    """Close a trade, apply exit costs and financing, and return new equity."""
    costs = config.costs
    inst = config.instrument

    half_spread = exit_price * costs.spread_fraction / 2.0
    slip = exit_price * costs.slippage_fraction

    # The exit crosses the spread in the opposite direction to the entry.
    if trade.side == "buy":
        fill = exit_price - half_spread - slip
    else:
        fill = exit_price + half_spread + slip
    fill = round(fill, inst.price_precision)

    direction = 1.0 if trade.side == "buy" else -1.0
    gross = (fill - trade.entry_price) * direction * trade.quantity * inst.contract_size
    gross *= config.quote_to_account

    exit_commission = costs.commission_per_lot * trade.quantity * config.quote_to_account
    exit_slippage = slip * trade.quantity * inst.contract_size * config.quote_to_account

    # Overnight financing. Approximated from calendar days held, which is the
    # right order of magnitude and is charged rather than ignored -- a strategy
    # that holds for weeks pays for the privilege.
    days_held = max(0, (exit_time - trade.entry_time).days)
    rate = costs.swap_long_per_lot if trade.side == "buy" else costs.swap_short_per_lot
    swap = rate * trade.quantity * days_held * config.quote_to_account

    trade.exit_price = fill
    trade.exit_time = exit_time
    trade.exit_reason = reason
    trade.gross_pnl = gross
    trade.commission += exit_commission
    trade.slippage += exit_slippage
    trade.swap = swap
    trade.net_pnl = gross - trade.commission - trade.slippage + swap

    return equity + trade.net_pnl


def _mark_to_market(trade: Trade | None, price: float, config: BacktestConfig) -> float:
    if trade is None:
        return 0.0
    direction = 1.0 if trade.side == "buy" else -1.0
    return (
        (price - trade.entry_price)
        * direction
        * trade.quantity
        * config.instrument.contract_size
        * config.quote_to_account
    )


def compute_metrics(
    trades: list[Trade], curve: list[dict[str, Any]], config: BacktestConfig
) -> dict[str, Any]:
    """Compute performance statistics.

    Win rate is reported but never presented as the headline. A strategy can
    win 90% of the time and lose money, and optimising for win rate reliably
    produces exactly that. Expectancy, profit factor and maximum drawdown are
    the numbers that decide whether an approach is viable.
    """
    initial = config.initial_capital
    if not curve:
        return {"trades": 0, "note": "no bars were evaluated"}

    equities = np.array([point["equity"] for point in curve], dtype="float64")
    final_equity = float(equities[-1])
    total_return = (final_equity - initial) / initial if initial > 0 else 0.0

    returns = np.diff(equities) / np.maximum(equities[:-1], 1e-9)
    returns = returns[np.isfinite(returns)]

    running_peak = np.maximum.accumulate(equities)
    drawdowns = np.where(running_peak > 0, (running_peak - equities) / running_peak, 0.0)
    max_drawdown = float(drawdowns.max()) if len(drawdowns) else 0.0

    # Longest run of consecutive bars spent below a previous peak.
    dd_duration = 0
    current = 0
    for value in drawdowns:
        if value > 1e-9:
            current += 1
            dd_duration = max(dd_duration, current)
        else:
            current = 0

    closed = [t for t in trades if t.exit_price is not None]
    wins = [t for t in closed if t.net_pnl > 0]
    losses = [t for t in closed if t.net_pnl <= 0]

    gross_profit = sum(t.net_pnl for t in wins)
    gross_loss = abs(sum(t.net_pnl for t in losses))

    avg_win = gross_profit / len(wins) if wins else 0.0
    avg_loss = gross_loss / len(losses) if losses else 0.0
    win_rate = len(wins) / len(closed) if closed else 0.0

    expectancy = (
        (win_rate * avg_win) - ((1 - win_rate) * avg_loss) if closed else 0.0
    )
    payoff = avg_win / avg_loss if avg_loss > 0 else 0.0
    profit_factor = gross_profit / gross_loss if gross_loss > 0 else 0.0

    # Per-bar Sharpe and Sortino, not annualised. Annualising a handful of
    # trades produces an impressive number with no statistical content, and the
    # annualisation factor depends on a timeframe the engine does not assume.
    volatility = float(returns.std(ddof=1)) if len(returns) > 1 else 0.0
    mean_return = float(returns.mean()) if len(returns) else 0.0
    sharpe = mean_return / volatility if volatility > 0 else 0.0

    downside = returns[returns < 0]
    downside_dev = float(downside.std(ddof=1)) if len(downside) > 1 else 0.0
    sortino = mean_return / downside_dev if downside_dev > 0 else 0.0
    calmar = total_return / max_drawdown if max_drawdown > 0 else 0.0

    consecutive_losses = 0
    worst_streak = 0
    for trade in closed:
        if trade.net_pnl <= 0:
            consecutive_losses += 1
            worst_streak = max(worst_streak, consecutive_losses)
        else:
            consecutive_losses = 0

    bars_in_market = sum(t.bars_held for t in closed)
    exposure = bars_in_market / len(curve) if curve else 0.0

    net_pnls = np.array([t.net_pnl for t in closed], dtype="float64") if closed else np.array([])
    tail_loss = float(np.percentile(net_pnls, 5)) if len(net_pnls) >= 20 else 0.0

    return {
        "initial_capital": round(initial, 2),
        "final_equity": round(final_equity, 2),
        "total_return": round(total_return, 6),
        "total_return_pct": round(total_return * 100, 3),
        "max_drawdown": round(max_drawdown, 6),
        "max_drawdown_pct": round(max_drawdown * 100, 3),
        "max_drawdown_duration_bars": dd_duration,
        "volatility_per_bar": round(volatility, 8),
        "sharpe_per_bar": round(sharpe, 4),
        "sortino_per_bar": round(sortino, 4),
        "calmar": round(calmar, 4),
        "trades": len(closed),
        "wins": len(wins),
        "losses": len(losses),
        "win_rate": round(win_rate, 4),
        "loss_rate": round(1 - win_rate, 4) if closed else 0.0,
        "average_win": round(avg_win, 4),
        "average_loss": round(avg_loss, 4),
        "payoff_ratio": round(payoff, 4),
        "expectancy": round(expectancy, 6),
        "profit_factor": round(profit_factor, 4),
        "gross_profit": round(gross_profit, 4),
        "gross_loss": round(gross_loss, 4),
        "total_commission": round(sum(t.commission for t in closed), 4),
        "total_slippage": round(sum(t.slippage for t in closed), 4),
        "total_swap": round(sum(t.swap for t in closed), 4),
        "average_mae": round(float(np.mean([t.mae for t in closed])), 6) if closed else 0.0,
        "average_mfe": round(float(np.mean([t.mfe for t in closed])), 6) if closed else 0.0,
        "max_consecutive_losses": worst_streak,
        "exposure_fraction": round(exposure, 4),
        "average_bars_held": round(bars_in_market / len(closed), 2) if closed else 0.0,
        "tail_loss_5pct": round(tail_loss, 4),
        "bars_evaluated": len(curve),
        "sample_kind": config.sample_kind,
    }


def _warnings(
    trades: list[Trade],
    metrics: dict[str, Any],
    config: BacktestConfig,
    signal_count: int,
    skipped: int,
    bar_count: int,
) -> list[str]:
    """Methodology warnings, surfaced next to the results rather than buried.

    A backtest without its caveats is a marketing document. These are the
    caveats that most often decide whether a result means anything.
    """
    out: list[str] = []
    closed = metrics.get("trades", 0)

    if config.costs.frictionless:
        out.append(
            "FRICTIONLESS: no spread, slippage or commission was applied. These results "
            "are not achievable and must not be compared with a costed run."
        )
    if closed == 0:
        out.append(
            "No trades were completed, so every performance figure is vacuous. "
            f"{signal_count} signal(s) were generated."
        )
    elif closed < 30:
        out.append(
            f"Only {closed} trades. Below roughly 30 trades the metrics are dominated by "
            f"chance: a Sharpe ratio or win rate computed from this sample says almost "
            f"nothing about the strategy."
        )
    if skipped > 0:
        out.append(
            f"{skipped} signal(s) were skipped because the risk budget could not buy the "
            f"instrument's minimum tradable size. A larger account would have taken these "
            f"trades, so this result understates the strategy's activity at this capital."
        )
    if closed >= 10 and metrics.get("win_rate", 0) > 0.75:
        out.append(
            f"Win rate of {metrics['win_rate']:.0%} is unusually high. Check whether the "
            f"exits are cutting winners short and letting losers run, which produces a "
            f"flattering win rate alongside a poor expectancy."
        )
    if metrics.get("profit_factor", 0) > 3 and closed < 100:
        out.append(
            f"Profit factor of {metrics['profit_factor']:.1f} on {closed} trades is high "
            f"enough to suspect overfitting or a data artefact rather than an edge."
        )
    if metrics.get("max_drawdown", 0) > 0.3:
        out.append(
            f"Maximum drawdown of {metrics['max_drawdown']:.0%} would be difficult to sit "
            f"through and may exceed the account's drawdown limit, which would halt trading "
            f"before the recovery."
        )
    if metrics.get("exposure_fraction", 0) > 0.9:
        out.append(
            "The strategy was in the market for over 90% of bars, so its results are close "
            "to a buy-and-hold of the underlying rather than evidence of timing skill."
        )
    if config.sample_kind == "in_sample":
        out.append(
            "IN-SAMPLE result. This measures fit to data the strategy was developed on and "
            "is not evidence of a forward edge."
        )
    total_costs = (
        metrics.get("total_commission", 0)
        + metrics.get("total_slippage", 0)
        + abs(metrics.get("total_swap", 0))
    )
    gross = abs(metrics.get("gross_profit", 0)) + abs(metrics.get("gross_loss", 0))
    if gross > 0 and total_costs / gross > 0.3:
        out.append(
            f"Costs consumed {total_costs / gross:.0%} of gross trading result. The approach "
            f"is highly sensitive to execution quality and would degrade sharply on a wider "
            f"spread."
        )
    if bar_count < 500:
        out.append(
            f"Only {bar_count} bars of history. This is too short to cover more than one "
            f"market regime."
        )
    return out


def _validate_bars(bars: pd.DataFrame) -> None:
    """Reject data a backtest cannot honestly run on."""
    required = {"open", "high", "low", "close"}
    missing = required - set(bars.columns)
    if missing:
        raise ValueError(f"bars are missing required column(s): {sorted(missing)}")
    if not isinstance(bars.index, pd.DatetimeIndex):
        raise ValueError("bars must be indexed by timestamp")
    if not bars.index.is_monotonic_increasing:
        raise ValueError(
            "bars are not in ascending time order; a backtest over shuffled data is meaningless"
        )
    if bars.index.has_duplicates:
        raise ValueError("bars contain duplicate timestamps")
    if bars[list(required)].isna().to_numpy().any():
        raise ValueError("bars contain missing prices")
    if (bars[list(required)] <= 0).to_numpy().any():
        raise ValueError("bars contain non-positive prices")

    incoherent = (
        (bars["high"] < bars["low"])
        | (bars["high"] < bars["open"])
        | (bars["high"] < bars["close"])
        | (bars["low"] > bars["open"])
        | (bars["low"] > bars["close"])
    )
    if incoherent.to_numpy().any():
        count = int(incoherent.sum())
        raise ValueError(
            f"{count} bar(s) have incoherent OHLC values (e.g. a high below the close); "
            f"indicators computed over them would produce confident nonsense"
        )


def dataset_hash(bars: pd.DataFrame) -> str:
    """Fingerprint the exact data a result was produced from.

    Stored with every backtest so two runs claiming the same result can be
    checked for having used the same data.
    """
    digest = hashlib.sha256()
    digest.update(f"{len(bars)}".encode())
    if len(bars):
        digest.update(str(bars.index[0]).encode())
        digest.update(str(bars.index[-1]).encode())
    for column in ("open", "high", "low", "close"):
        if column in bars.columns:
            digest.update(
                np.ascontiguousarray(bars[column].to_numpy(dtype="float64")).tobytes()
            )
    return digest.hexdigest()


def walk_forward(
    bars: pd.DataFrame,
    config: BacktestConfig,
    train_bars: int,
    test_bars: int,
    step: int | None = None,
) -> list[tuple[pd.Timestamp, pd.Timestamp, BacktestResult]]:
    """Run a rolling out-of-sample evaluation.

    Each fold tests on data that follows its training window. This does not by
    itself prevent overfitting -- choosing parameters by looking at the combined
    walk-forward result overfits just as effectively -- but it does prevent the
    cruder error of testing on the data used to choose the parameters.
    """
    if train_bars < 100 or test_bars < 20:
        raise ValueError("walk-forward needs at least 100 training and 20 test bars per fold")
    stride = step or test_bars

    folds: list[tuple[pd.Timestamp, pd.Timestamp, BacktestResult]] = []
    start = 0
    while start + train_bars + test_bars <= len(bars):
        test_slice = bars.iloc[start + train_bars - 50 : start + train_bars + test_bars]
        fold_config = BacktestConfig(**{**config.__dict__, "sample_kind": "walk_forward"})
        try:
            result = run(test_slice, fold_config)
        except ValueError:
            start += stride
            continue
        folds.append((test_slice.index[0], test_slice.index[-1], result))
        start += stride
    return folds


def sensitivity(
    bars: pd.DataFrame, config: BacktestConfig, parameter: str, values: list[Any]
) -> list[dict[str, Any]]:
    """Measure how a result changes as one parameter varies.

    A strategy whose performance collapses when a period moves from 20 to 21 is
    fitted to noise. A broad plateau is worth more than a sharp peak, and this
    is how you tell them apart.
    """
    out: list[dict[str, Any]] = []
    for value in values:
        variant = BacktestConfig(
            **{
                **config.__dict__,
                "parameters": {**config.parameters, parameter: value},
            }
        )
        try:
            result = run(bars, variant)
        except (ValueError, KeyError) as exc:
            out.append({parameter: value, "error": str(exc)})
            continue
        out.append(
            {
                parameter: value,
                "total_return": result.metrics.get("total_return"),
                "max_drawdown": result.metrics.get("max_drawdown"),
                "trades": result.metrics.get("trades"),
                "expectancy": result.metrics.get("expectancy"),
                "profit_factor": result.metrics.get("profit_factor"),
            }
        )
    return out


def cost_sensitivity(
    bars: pd.DataFrame, config: BacktestConfig, spread_multipliers: list[float]
) -> list[dict[str, Any]]:
    """Re-run at wider spreads.

    A strategy that is profitable only at the tightest spread is not a
    strategy: real spreads widen exactly when its signals fire.
    """
    out: list[dict[str, Any]] = []
    base = config.costs
    for multiplier in spread_multipliers:
        variant = BacktestConfig(
            **{
                **config.__dict__,
                "costs": CostModel(
                    spread_fraction=base.spread_fraction * multiplier,
                    slippage_fraction=base.slippage_fraction * multiplier,
                    commission_per_lot=base.commission_per_lot,
                    swap_long_per_lot=base.swap_long_per_lot,
                    swap_short_per_lot=base.swap_short_per_lot,
                ),
            }
        )
        try:
            result = run(bars, variant)
        except ValueError as exc:
            out.append({"spread_multiplier": multiplier, "error": str(exc)})
            continue
        out.append(
            {
                "spread_multiplier": multiplier,
                "total_return": result.metrics.get("total_return"),
                "trades": result.metrics.get("trades"),
                "expectancy": result.metrics.get("expectancy"),
                "profit_factor": result.metrics.get("profit_factor"),
            }
        )
    return out


def monte_carlo_trade_order(
    trades: list[Trade], initial_capital: float, iterations: int = 1000, seed: int = 0
) -> dict[str, Any]:
    """Reshuffle trade order to estimate the range of plausible outcomes.

    The realised equity curve is one ordering of the trades that happened. A
    different order produces a different drawdown from the same trades, and the
    realised path is not special. This gives a sense of how much of the
    headline drawdown was luck of sequence.
    """
    closed = [t.net_pnl for t in trades if t.exit_price is not None]
    if len(closed) < 10:
        return {"note": f"only {len(closed)} closed trades; too few to resample meaningfully"}

    rng = np.random.default_rng(seed)
    pnls = np.array(closed, dtype="float64")
    finals: list[float] = []
    drawdowns: list[float] = []

    for _ in range(iterations):
        shuffled = rng.permutation(pnls)
        equity = initial_capital + np.cumsum(shuffled)
        peak = np.maximum.accumulate(np.concatenate([[initial_capital], equity]))[1:]
        finals.append(float(equity[-1]))
        drawdowns.append(float(np.max((peak - equity) / np.maximum(peak, 1e-9))))

    return {
        "iterations": iterations,
        "final_equity_p05": round(float(np.percentile(finals, 5)), 2),
        "final_equity_p50": round(float(np.percentile(finals, 50)), 2),
        "final_equity_p95": round(float(np.percentile(finals, 95)), 2),
        "max_drawdown_p50": round(float(np.percentile(drawdowns, 50)), 4),
        "max_drawdown_p95": round(float(np.percentile(drawdowns, 95)), 4),
        "probability_of_loss": round(
            float(np.mean(np.array(finals) < initial_capital)), 4
        ),
        "note": (
            "Resampling the ORDER of realised trades only. It does not model regime "
            "change, and it assumes the trades themselves remain representative."
        ),
    }
