"""The research service's HTTP API.

This service computes. It does not decide, persist, or execute.

Consequences of that, visible throughout this module:

*   It receives the bars it should evaluate rather than fetching them, so the
    control plane and the research plane provably reason over the same data and
    a strategy cannot widen its own lookback beyond what it declared.
*   It holds no credentials for the control plane and exposes no endpoint that
    could place an order. There is no code path from here to a broker.
*   Its database role, where one is configured, is read-only and cannot see
    users, accounts, orders, credentials or the ledger.
*   It returns results; the control plane validates them and decides what to
    store. Nothing this service returns is trusted on arrival.
"""

from __future__ import annotations

import hashlib
import os
import secrets
from datetime import datetime
from typing import Annotated, Any, Literal

import numpy as np
import pandas as pd
from fastapi import Depends, FastAPI, Header, HTTPException, Request, status
from fastapi.responses import JSONResponse
from pydantic import BaseModel, Field, field_validator

from vantage_quant import backtest as bt
from vantage_quant import ml, scanner
from vantage_quant import strategies as strat

SERVICE_TOKEN = os.getenv("VANTAGE_QUANT_SERVICE_TOKEN", "")

app = FastAPI(
    title="Vantage Research Service",
    version="0.1.0",
    description=(
        "Indicators, strategies, backtesting, machine learning and opportunity "
        "scanning. This service has no path to order execution."
    ),
    docs_url=None if os.getenv("VANTAGE_ENV") in ("production", "staging") else "/docs",
    redoc_url=None,
)


# ---------------------------------------------------------------------------
# Authentication
# ---------------------------------------------------------------------------


async def require_service_token(
    x_vantage_service_token: Annotated[str | None, Header()] = None,
) -> None:
    """Authenticate the calling service.

    A shared token, compared in constant time. It authenticates the control
    plane TO research and grants research nothing in return, which is the whole
    point of the asymmetry.

    When no token is configured the service refuses every request rather than
    running open: a research service that answers unauthenticated callers is a
    free compute endpoint, and in a deployed environment it would be reachable
    by anything on the network.
    """
    if not SERVICE_TOKEN:
        raise HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail=(
                "VANTAGE_QUANT_SERVICE_TOKEN is not configured; the service refuses "
                "to accept unauthenticated requests"
            ),
        )
    if not x_vantage_service_token or not secrets.compare_digest(
        x_vantage_service_token, SERVICE_TOKEN
    ):
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED, detail="invalid service token"
        )


Authenticated = Depends(require_service_token)


# ---------------------------------------------------------------------------
# Schemas
# ---------------------------------------------------------------------------

# Bounds on every list. An unbounded array on a compute endpoint is a
# denial-of-service vector: a caller could ask for a hundred million bars.
MAX_BARS = 50_000
MAX_INSTRUMENTS = 100


class BarInput(BaseModel):
    open_time: str
    open: str
    high: str
    low: str
    close: str
    volume: str = "0"


class SignalRequest(BaseModel):
    strategy_key: str = Field(max_length=64)
    version: int = 1
    instrument_id: str = Field(max_length=32)
    timeframe: str = Field(max_length=8)
    parameters: dict[str, Any] = Field(default_factory=dict)
    bars: list[BarInput] = Field(max_length=MAX_BARS)
    context: dict[str, Any] = Field(default_factory=dict)

    @field_validator("bars")
    @classmethod
    def _need_bars(cls, value: list[BarInput]) -> list[BarInput]:
        if len(value) < 2:
            raise ValueError("at least 2 bars are required")
        return value


class SignalResponse(BaseModel):
    action: Literal["buy", "sell", "hold", "close", "no_trade"]
    confidence: float
    suggested_stop: float | None = None
    suggested_target: float | None = None
    explanation: str
    indicators: dict[str, str] = Field(default_factory=dict)
    features: dict[str, float] = Field(default_factory=dict)
    bar_time: str
    code_hash: str
    # The market's shape at the instant of the signal, and the measures behind
    # it. UNKNOWN is a real answer and is returned whenever the evidence is
    # thin; it must never be coerced into a tradable label.
    regime: str = "UNKNOWN"
    regime_measures: dict[str, float] = Field(default_factory=dict)


class BacktestRequest(BaseModel):
    strategy_key: str = Field(max_length=64)
    version: int = 1
    instrument_id: str = Field(max_length=32)
    timeframe: str = Field(max_length=8)
    parameters: dict[str, Any] = Field(default_factory=dict)
    bars: list[BarInput] = Field(max_length=MAX_BARS)
    initial_capital: str = "500"
    currency: str = "ZAR"
    contract_size: str = "1"
    commission_per_lot: str = "0"
    spread_fraction: str = "0.00012"
    slippage_fraction: str = "0.0001"
    swap_long_per_lot: str = "0"
    swap_short_per_lot: str = "0"
    risk_per_trade: str = "0.01"
    min_quantity: str = "0.01"
    quantity_step: str = "0.01"
    sample_kind: str = "out_of_sample"
    seed: int = 0
    quote_to_account: str = "1"


class TrainRequest(BaseModel):
    model_key: str = Field(max_length=64)
    algorithm: str = "logistic_regression"
    instrument_id: str = Field(max_length=32)
    timeframe: str = Field(max_length=8)
    bars: list[BarInput] = Field(max_length=MAX_BARS)
    train_fraction: float = 0.6
    validation_fraction: float = 0.2
    embargo_bars: int = 2
    horizon: int = 1
    hyperparameters: dict[str, Any] = Field(default_factory=dict)
    seed: int = 0


class ScanInstrumentInput(BaseModel):
    instrument_id: str = Field(max_length=32)
    symbol: str = Field(max_length=32)
    bars: list[BarInput] = Field(max_length=5_000)
    spread_fraction: str = "0"
    session: str = "unknown"
    event_risk: str = "none"


class ScanRequest(BaseModel):
    timeframe: str = Field(max_length=8)
    instruments: list[ScanInstrumentInput] = Field(max_length=MAX_INSTRUMENTS)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def bars_to_frame(bars: list[BarInput]) -> pd.DataFrame:
    """Convert the wire format into a validated OHLC frame.

    Every value is parsed and checked here rather than trusted. The control
    plane is the only caller today, but a compute service that assumes its
    input is well-formed produces confident nonsense the moment that stops
    being true.
    """
    if not bars:
        raise HTTPException(status_code=422, detail="no bars supplied")

    try:
        index = pd.to_datetime([b.open_time for b in bars], utc=True, format="ISO8601")
        frame = pd.DataFrame(
            {
                "open": [float(b.open) for b in bars],
                "high": [float(b.high) for b in bars],
                "low": [float(b.low) for b in bars],
                "close": [float(b.close) for b in bars],
                "volume": [float(b.volume or 0) for b in bars],
            },
            index=index,
        )
    except (ValueError, TypeError) as exc:
        raise HTTPException(status_code=422, detail=f"bars could not be parsed: {exc}") from exc

    frame = frame.sort_index()
    if frame.index.has_duplicates:
        raise HTTPException(status_code=422, detail="bars contain duplicate timestamps")
    if frame[["open", "high", "low", "close"]].isna().to_numpy().any():
        raise HTTPException(status_code=422, detail="bars contain missing prices")
    if (frame[["open", "high", "low", "close"]] <= 0).to_numpy().any():
        raise HTTPException(status_code=422, detail="bars contain non-positive prices")

    incoherent = (
        (frame["high"] < frame["low"])
        | (frame["high"] < frame["close"])
        | (frame["low"] > frame["close"])
    )
    if incoherent.to_numpy().any():
        raise HTTPException(
            status_code=422,
            detail=f"{int(incoherent.sum())} bar(s) have incoherent OHLC values",
        )
    return frame


def _float(value: str, name: str, minimum: float | None = None) -> float:
    try:
        parsed = float(value)
    except (TypeError, ValueError) as exc:
        raise HTTPException(status_code=422, detail=f"{name} is not a number") from exc
    if not np.isfinite(parsed):
        raise HTTPException(status_code=422, detail=f"{name} is not finite")
    if minimum is not None and parsed < minimum:
        raise HTTPException(status_code=422, detail=f"{name} must be at least {minimum}")
    return parsed


# ---------------------------------------------------------------------------
# Endpoints
# ---------------------------------------------------------------------------


@app.get("/health/live")
async def liveness() -> dict[str, str]:
    """Liveness. Unauthenticated and dependency-free by design."""
    return {"status": "alive", "service": "vantage-quant"}


@app.get("/health/ready")
async def readiness() -> dict[str, Any]:
    return {
        "status": "ready",
        "strategies": len(strat.REGISTRY),
        "token_configured": bool(SERVICE_TOKEN),
    }


@app.get("/v1/strategies", dependencies=[Authenticated])
async def list_strategies() -> dict[str, Any]:
    """Describe every implemented strategy.

    The control plane reconciles this against its own registry at start-up, so
    a strategy present in one and missing from the other is visible rather than
    silently inert.
    """
    return {
        "strategies": [
            {
                "key": spec.key,
                "name": spec.name,
                "family": spec.family,
                "description": spec.description,
                "high_risk": spec.high_risk,
                "default_parameters": spec.default_parameters,
                "required_bars": spec.required_bars,
                "timeframes": list(spec.timeframes),
                "valid_regimes": list(spec.valid_regimes),
                "code_hash": spec.code_hash,
            }
            for spec in sorted(strat.REGISTRY.values(), key=lambda s: (s.family, s.key))
        ]
    }


@app.post("/v1/strategies/signal", dependencies=[Authenticated])
async def generate_signal(request: SignalRequest) -> SignalResponse:
    """Evaluate one strategy over completed bars."""
    frame = bars_to_frame(request.bars)
    try:
        signal, spec = strat.evaluate(
            request.strategy_key, frame, request.parameters, request.context
        )
    except KeyError as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc
    except ValueError as exc:
        raise HTTPException(status_code=422, detail=str(exc)) from exc

    # The market's shape at the instant of the signal, from the same
    # classifier the scanner uses.
    #
    # Reported rather than left to the caller because the caller does not have
    # the bars: the control plane sends them here, gets a signal back, and had
    # no way to know whether the market was trending or ranging when it acted.
    # `event_risk` is deliberately NOT passed -- the control plane holds the
    # authoritative economic calendar and infers EVENT_RISK itself, and two
    # sources for one label is how they disagree.
    regime, measures = scanner.classify_regime(frame)

    return SignalResponse(
        action=signal.action,
        confidence=round(signal.confidence, 6),
        suggested_stop=signal.suggested_stop,
        suggested_target=signal.suggested_target,
        explanation=signal.explanation,
        indicators=signal.indicators,
        features={k: round(v, 8) for k, v in signal.features.items() if np.isfinite(v)},
        bar_time=frame.index[-1].isoformat(),
        code_hash=spec.code_hash,
        regime=regime,
        # The measures are the account of the label. A regime with no numbers
        # behind it cannot be argued with after the fact.
        regime_measures={
            k: round(v, 6) for k, v in measures.items() if np.isfinite(v)
        },
    )


@app.post("/v1/backtests/run", dependencies=[Authenticated])
async def run_backtest(request: BacktestRequest) -> dict[str, Any]:
    """Run a historical evaluation with costs applied."""
    frame = bars_to_frame(request.bars)

    config = bt.BacktestConfig(
        strategy_key=request.strategy_key,
        parameters=request.parameters,
        initial_capital=_float(request.initial_capital, "initial_capital", 0.01),
        risk_per_trade=_float(request.risk_per_trade, "risk_per_trade", 0.0),
        costs=bt.CostModel(
            spread_fraction=_float(request.spread_fraction, "spread_fraction", 0.0),
            slippage_fraction=_float(request.slippage_fraction, "slippage_fraction", 0.0),
            commission_per_lot=_float(request.commission_per_lot, "commission_per_lot", 0.0),
            swap_long_per_lot=_float(request.swap_long_per_lot, "swap_long_per_lot"),
            swap_short_per_lot=_float(request.swap_short_per_lot, "swap_short_per_lot"),
        ),
        instrument=bt.InstrumentModel(
            contract_size=_float(request.contract_size, "contract_size", 0.0000001),
            min_quantity=_float(request.min_quantity, "min_quantity", 0.0000001),
            quantity_step=_float(request.quantity_step, "quantity_step", 0.0000001),
        ),
        sample_kind=request.sample_kind,  # type: ignore[arg-type]
        quote_to_account=_float(request.quote_to_account, "quote_to_account", 0.0000001),
        seed=request.seed,
    )

    try:
        result = bt.run(frame, config)
    except KeyError as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc
    except ValueError as exc:
        raise HTTPException(status_code=422, detail=str(exc)) from exc

    return {
        "metrics": result.metrics,
        "equity_curve": result.equity_curve,
        "trades": [t.to_dict() for t in result.trades],
        "warnings": result.warnings,
        "dataset_hash": result.dataset_hash,
        "code_hash": result.code_hash,
        "frictionless": result.frictionless,
        "skipped_unaffordable": result.skipped_unaffordable,
    }


@app.post("/v1/ml/train", dependencies=[Authenticated])
async def train_model(request: TrainRequest) -> dict[str, Any]:
    """Train a directional model with chronological, embargoed splits."""
    frame = bars_to_frame(request.bars)

    try:
        model = ml.train(
            frame,
            key=request.model_key,
            algorithm=request.algorithm,
            horizon=request.horizon,
            train_fraction=request.train_fraction,
            validation_fraction=request.validation_fraction,
            embargo_bars=request.embargo_bars,
            seed=request.seed,
            hyperparameters=request.hyperparameters,
        )
    except ml.FeatureUnavailableError as exc:
        raise HTTPException(status_code=422, detail=str(exc)) from exc
    except ValueError as exc:
        raise HTTPException(status_code=422, detail=str(exc)) from exc

    model = ml.save(model)

    # Deployment here means "available to the research process for PAPER
    # signals". The lifecycle state lives in the control plane, is capped at
    # PAPER, and a model cannot promote itself.
    ml.deploy(model)

    dataset = ml.build_dataset(frame, request.horizon)

    return {
        "algorithm": model.algorithm,
        "feature_definition": ml.FEATURE_DEFINITION,
        "label_definition": ml.label_definition(request.horizon),
        "hyperparameters": model.hyperparameters,
        "dataset_hash": model.dataset_hash,
        "code_hash": hashlib.sha256(
            f"{model.algorithm}|{sorted(ml.FEATURE_NAMES)}|{request.horizon}".encode()
        ).hexdigest()[:16],
        "dependency_versions": model.dependency_versions,
        "seed": model.seed,
        "row_count": dataset.row_count,
        "windows": model.windows,
        "evaluations": model.evaluations,
        "warnings": model.warnings,
        "artifact_path": model.artifact_path or "",
        "artifact_hash": model.artifact_hash or "",
    }


@app.post("/v1/scanner/scan", dependencies=[Authenticated])
async def scan(request: ScanRequest) -> dict[str, Any]:
    """Rank candidate instruments. Places nothing."""
    candidates: list[dict[str, Any]] = []

    for item in request.instruments:
        try:
            frame = bars_to_frame(item.bars)
        except HTTPException:
            # One malformed instrument must not fail the whole scan.
            continue
        candidate = scanner.scan_instrument(
            instrument_id=item.instrument_id,
            symbol=item.symbol,
            bars=frame,
            spread_fraction=_float(item.spread_fraction, "spread_fraction", 0.0),
            session=item.session,
            event_risk=item.event_risk,
        )
        if candidate is not None:
            candidates.append(candidate.to_dict())

    candidates.sort(key=lambda c: float(c["score"]), reverse=True)
    return {
        "candidates": candidates,
        "evaluated_at": datetime.utcnow().isoformat() + "Z",
        "note": "The scanner ranks opportunities. It does not place orders.",
    }


@app.exception_handler(Exception)
async def unhandled_exception(request: Request, exc: Exception) -> JSONResponse:
    """Return a generic error rather than a traceback.

    A stack trace tells a caller about file paths, library versions and internal
    structure. The detail belongs in the service's own logs.
    """
    return JSONResponse(
        status_code=500,
        content={"detail": "the research service could not complete this request"},
    )
