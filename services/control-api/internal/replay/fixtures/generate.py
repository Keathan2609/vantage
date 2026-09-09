"""Generate the committed replay fixtures.

Run from the repository root:

    python services/control-api/internal/replay/fixtures/generate.py

Committed so the fixtures are reproducible: a diff to a CSV should be
explainable by a diff to this file.

Closed-form and deterministic: no RNG, so the committed CSV is reproducible by
anyone running this script, and a diff to a fixture is a deliberate change
rather than a reseed.

Prices are Decimal throughout. A fixture written from floats would carry
binary-rounding artefacts into a financial test.
"""

import csv
import io
import os
from datetime import datetime, timedelta, timezone
from decimal import Decimal, getcontext

getcontext().prec = 28

OUT = os.path.join("services", "control-api", "internal", "replay", "fixtures")
HEADER = [
    "instrument_id", "timeframe", "timestamp",
    "open", "high", "low", "close", "volume",
    "bid", "ask", "spread_fraction", "session", "source",
]

# A Tuesday, and deliberately AHEAD of any plausible seed date.
#
# 08:00 UTC is inside London; the London/New York overlap starts around 13:00
# UTC, so a series of 1h bars spends most of its length in liquid hours.
#
# The year matters for a reason that is not obvious. A replay puts the whole
# process on dataset time, and the data-quality policy compares each incoming
# quote against the newest one already stored. A dataset dated BEFORE the
# seeded market data therefore makes every replay quote look
# "timestamp_regressed" -- correctly, since it genuinely is older -- and the
# whole run is refused for a reason that reads like a broken clock. Dating
# fixtures ahead of the seed sidesteps that without weakening the check.
#
# US DST in 2027 begins on 14 March, so 2 March is EST (UTC-5) and the venue's
# 17:00-18:00 New York maintenance break is 22:00-23:00 UTC, which is what the
# tradable() filter below assumes.
START = datetime(2027, 3, 2, 4, 0, 0, tzinfo=timezone.utc)
TF = "1h"
STEP = timedelta(hours=1)
SPREAD = Decimal("0.00012")

# The venue's daily maintenance break is 17:00-18:00 New York. On 2027-03-02
# New York is UTC-5 (DST begins 2027-03-14), so the break is 22:00-23:00 UTC.
BREAK_START_UTC_HOUR = 22


def session_for(ts):
    """The session label, for the fixture's advisory column."""
    h = ts.hour
    if 13 <= h < 16:
        return "overlap"
    if 8 <= h < 16:
        return "london"
    if 13 <= h < 21:
        return "new_york"
    if 0 <= h < 8:
        return "tokyo"
    return "sydney"


def tradable(ts):
    """Skip weekend and the daily break, matching fx_metals_24x5."""
    if ts.weekday() >= 5:  # Saturday, Sunday
        return False
    if ts.hour == BREAK_START_UTC_HOUR:
        return False
    return True


def bar(instrument, ts, open_px, close_px, wick_fraction, spread=SPREAD, source="generated"):
    """Build one row with wicks OUTSIDE the body.

    The wick placement matters: an indicator that reads true range needs a real
    range, not a body height, and every bar must satisfy the dataset
    validator's consistency rules.
    """
    high = max(open_px, close_px)
    low = min(open_px, close_px)
    wick = ((high + low) / 2) * wick_fraction
    high = (high + wick).quantize(Decimal("0.01"))
    low = (low - wick).quantize(Decimal("0.01"))
    return [
        instrument, TF, ts.strftime("%Y-%m-%dT%H:%M:%SZ"),
        str(open_px.quantize(Decimal("0.01"))),
        str(high), str(low),
        str(close_px.quantize(Decimal("0.01"))),
        "1000",
        "", "",                      # bid/ask derived from close and the spread
        str(spread),
        session_for(ts), source,
    ]


def emit(name, rows):
    os.makedirs(OUT, exist_ok=True)
    path = os.path.join(OUT, name)
    buf = io.StringIO()
    w = csv.writer(buf, lineterminator="\n")
    w.writerow(HEADER)
    w.writerows(rows)
    io.open(path, "w", encoding="utf-8", newline="\n").write(buf.getvalue())
    print(f"{name}: {len(rows)} rows")


def walk(instrument, count, start_price, next_close, wick=Decimal("0.0008"),
         spread_at=None, start=START):
    """Advance a price series, skipping non-tradable hours."""
    rows = []
    ts = start
    price = start_price
    i = 0
    while len(rows) < count:
        if not tradable(ts):
            ts += STEP
            continue
        open_px = price
        close_px = next_close(i, price)
        spread = spread_at(i) if spread_at else SPREAD
        rows.append(bar(instrument, ts, open_px, close_px, wick, spread))
        price = close_px
        ts += STEP
        i += 1
    return rows


GOLD = "XAUUSD.m"
SILVER = "XAGUSD"
P0 = Decimal("2650.00")

# --- A. clean trend --------------------------------------------------------
# Two bars with the trend, one against by a third. Enough to keep ADX high
# without an unbroken line no indicator would ever see.
def trend_step(rate):
    def f(i, price):
        if i % 3 == 2:
            return price * (Decimal(1) - rate / 3)
        return price * (Decimal(1) + rate)
    return f


emit("trend_clean.csv", walk(GOLD, 140, P0, trend_step(Decimal("0.002"))))

# --- B. range --------------------------------------------------------------
CYCLE = [0, 1, 2, 1, 0, -1, -2, -1]


def range_step(base, amplitude):
    def f(i, _price):
        return base * (Decimal(1) + amplitude * Decimal(CYCLE[i % len(CYCLE)]))
    return f


emit("range_bound.csv", walk(GOLD, 140, P0, range_step(P0, Decimal("0.004")),
                             wick=Decimal("0.0006")))

# --- C. volatility shock ---------------------------------------------------
SHOCK_AT = 70


def shock_step(i, price):
    if i < SHOCK_AT:
        return price * (Decimal("1.0002") if i % 2 == 0 else Decimal("0.9998"))
    return price * (Decimal("0.98") if i % 2 == 0 else Decimal("1.01"))


emit("volatility_shock.csv",
     walk(GOLD, 140, P0, shock_step,
          spread_at=lambda i: SPREAD if i < SHOCK_AT else Decimal("0.0009")))

# --- E. spread spike -------------------------------------------------------
# The price path is flat: only the cost of crossing it moves, so the scenario
# tests one thing.
def flat_step(i, price):
    return price * (Decimal("1.00005") if i % 2 == 0 else Decimal("0.99995"))


emit("spread_spike.csv",
     walk(GOLD, 100, P0, flat_step, wick=Decimal("0.0002"),
          spread_at=lambda i: SPREAD if i < 60 else Decimal("0.02")))

# --- H. drawdown -----------------------------------------------------------
emit("drawdown.csv",
     walk(GOLD, 120, P0, lambda i, price: price * Decimal("0.996"),
          wick=Decimal("0.0010")))

# --- K. trend reversal -----------------------------------------------------
REVERSE_AT = 70


def reversal_step(i, price):
    rate = Decimal("0.002") if i < REVERSE_AT else Decimal("-0.0025")
    if i % 3 == 2:
        return price * (Decimal(1) - rate / 3)
    return price * (Decimal(1) + rate)


emit("trend_reversal.csv", walk(GOLD, 140, P0, reversal_step))

# --- L. false breakout -----------------------------------------------------
# A long quiet range establishes the channel, one bar breaks well above it,
# and the next few bars retrace straight back inside. A breakout strategy that
# entered on the break must be stopped out rather than carried.
def false_breakout_step(i, price):
    if i < 80:
        return P0 * (Decimal(1) + Decimal("0.0015") * Decimal(CYCLE[i % len(CYCLE)]))
    if i == 80:
        return P0 * Decimal("1.012")     # the break
    if 81 <= i <= 86:
        return price * Decimal("0.9965")  # straight back down through the range
    return P0 * (Decimal(1) + Decimal("0.0015") * Decimal(CYCLE[i % len(CYCLE)]))


emit("false_breakout.csv", walk(GOLD, 120, P0, false_breakout_step,
                                wick=Decimal("0.0006")))

# --- M. correlated pair ----------------------------------------------------
# Both instruments trend up together. Interleaved by timestamp, which is what
# the dataset's sort guarantees, so the pipeline sees them as one market state.
gold = walk(GOLD, 120, P0, trend_step(Decimal("0.002")))
silver = walk(SILVER, 120, Decimal("31.50"), trend_step(Decimal("0.0022")))
emit("correlated_pair.csv", sorted(gold + silver, key=lambda r: (r[2], r[0])))

# --- T. day boundary -------------------------------------------------------
# Starts late in the UTC day so the series crosses midnight UTC and the venue's
# daily break, which the walk skips.
emit("day_boundary.csv",
     walk(GOLD, 90, P0, trend_step(Decimal("0.0012")),
          start=datetime(2027, 3, 2, 18, 0, 0, tzinfo=timezone.utc)))
