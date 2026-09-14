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
from zoneinfo import ZoneInfo
from decimal import Decimal, getcontext

getcontext().prec = 28

# Relative to THIS FILE, not to the working directory.
#
# It was relative to the repository root, so running the generator from its own
# directory silently created
# `internal/replay/fixtures/services/control-api/internal/replay/fixtures/` and
# wrote there. The committed fixtures were untouched and the script reported
# success, which is the worst combination available.
OUT = os.path.dirname(os.path.abspath(__file__))
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
# The venue calendar is defined in NEW YORK time, and tradable() below now
# evaluates it there rather than in fixed UTC hours.
#
# It used to assume EST, which was true for 2027-03-02 and false eleven days
# later: US DST begins on 2027-03-14. That made the whole date range
# unusable -- datasets could not be spaced forward without silently placing
# bars inside the maintenance break -- and forced them backwards instead,
# where a guard test correctly refused them for being behind a plausible seed.
# Converting to New York time removes the constraint entirely.
START = datetime(2027, 3, 2, 4, 0, 0, tzinfo=timezone.utc)
NEW_YORK = ZoneInfo("America/New_York")
TF = "1h"
STEP = timedelta(hours=1)
SPREAD = Decimal("0.00012")

# The venue's daily maintenance break and weekly window, in NEW YORK hours --
# the terms the calendar is actually defined in. DST then takes care of itself.
BREAK_START_NY_HOUR = 17   # 17:00-18:00 New York, Monday to Thursday
WEEK_OPEN_NY_HOUR = 17     # Sunday 17:00 New York
WEEK_CLOSE_NY_HOUR = 17    # Friday 17:00 New York


def session_for(ts):
    """The session label, for the fixture's advisory column.

    Still expressed in UTC hours, which is what the London and Tokyo sessions
    are anchored to closely enough for an advisory column. The AUTHORITATIVE
    session on a decision comes from the market clock in the control plane, not
    from here -- this is a convenience for someone reading the CSV.
    """
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
    """Match fx_metals_24x5 exactly: weekly window plus the daily break.

    Getting this wrong is not cosmetic. A bar at a time the venue is shut is a
    bar that could not have happened, and a strategy acting on it would be
    trading the maintenance window. An earlier version of this function only
    excluded Saturday, Sunday and the break hour, which left bars on Friday
    evening -- after the 17:00 New York weekly close -- that the market clock
    reports as closed_weekend. Nine fixtures carried them until a test
    checking every bar against the real clock found it.

    The venue's week runs Sunday 17:00 New York to Friday 17:00 New York, and
    this evaluates it IN NEW YORK, so a dataset either side of a DST boundary
    is filtered correctly. The previous version hard-coded the EST equivalents
    in UTC and was wrong for any range after 2027-03-14.
    """
    local = ts.astimezone(NEW_YORK)
    wd = local.weekday()       # Monday = 0
    if wd == 5:                # Saturday: shut all day
        return False
    if wd == 6:                # Sunday: shut until the weekly open
        return local.hour >= WEEK_OPEN_NY_HOUR
    if wd == 4 and local.hour >= WEEK_CLOSE_NY_HOUR:  # Friday, after the close
        return False
    if local.hour == BREAK_START_NY_HOUR:             # the daily break, Mon-Thu
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


def week(n):
    """Shift the start FORWARD by n weeks, keeping the weekday.

    Every dataset needs its OWN date range. The per-bar guard evaluates a
    strategy once per completed bar and records that in strategy_runs, so two
    datasets covering the same hours cannot both be replayed into one account:
    the second finds every bar already evaluated and produces nothing at all.
    Measured before this existed: eight of nine scenarios recorded zero
    strategy runs and the failures read as a broken pipeline.

    Datasets are spaced TWO weeks apart, not one: a 140-bar hourly series spans
    about eight days once non-tradable hours are skipped, so consecutive weeks
    would still overlap by a couple of days and those bars would be evaluated
    only once.

    FORWARD, so every dataset stays ahead of any plausible seed date. A dataset
    behind the seeded market data makes every replay quote look
    `timestamp_regressed` -- correctly, since it is older -- and the run is
    refused for a reason that reads like a broken clock. A guard test enforces
    the margin.

    Whole weeks preserve the weekday, which `tradable` depends on. DST is no
    longer a constraint: `tradable` evaluates the venue calendar in New York.
    """
    return START + timedelta(weeks=n)


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


emit("trend_clean.csv", walk(GOLD, 140, P0, trend_step(Decimal("0.002")),
                             start=week(0)))

# --- B. range --------------------------------------------------------------
CYCLE = [0, 1, 2, 1, 0, -1, -2, -1]


def range_step(base, amplitude):
    def f(i, _price):
        return base * (Decimal(1) + amplitude * Decimal(CYCLE[i % len(CYCLE)]))
    return f


emit("range_bound.csv", walk(GOLD, 140, P0, range_step(P0, Decimal("0.004")),
                             wick=Decimal("0.0006"), start=week(2)))

# --- C. volatility shock ---------------------------------------------------
SHOCK_AT = 70


def shock_step(i, price):
    if i < SHOCK_AT:
        return price * (Decimal("1.0002") if i % 2 == 0 else Decimal("0.9998"))
    return price * (Decimal("0.98") if i % 2 == 0 else Decimal("1.01"))


emit("volatility_shock.csv",
     walk(GOLD, 140, P0, shock_step, start=week(4),
          spread_at=lambda i: SPREAD if i < SHOCK_AT else Decimal("0.0009")))

# --- E. spread spike -------------------------------------------------------
# The price path is flat: only the cost of crossing it moves, so the scenario
# tests one thing.
def flat_step(i, price):
    return price * (Decimal("1.00005") if i % 2 == 0 else Decimal("0.99995"))


emit("spread_spike.csv",
     walk(GOLD, 100, P0, flat_step, wick=Decimal("0.0002"), start=week(6),
          spread_at=lambda i: SPREAD if i < 60 else Decimal("0.02")))

# --- H. drawdown -----------------------------------------------------------
emit("drawdown.csv",
     walk(GOLD, 120, P0, lambda i, price: price * Decimal("0.996"),
          wick=Decimal("0.0010"), start=week(8)))

# --- K. trend reversal -----------------------------------------------------
REVERSE_AT = 70


def reversal_step(i, price):
    rate = Decimal("0.002") if i < REVERSE_AT else Decimal("-0.0025")
    if i % 3 == 2:
        return price * (Decimal(1) - rate / 3)
    return price * (Decimal(1) + rate)


emit("trend_reversal.csv", walk(GOLD, 140, P0, reversal_step, start=week(10)))

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
                                wick=Decimal("0.0006"), start=week(12)))

# --- M. correlated pair ----------------------------------------------------
# Both instruments trend up together. Interleaved by timestamp, which is what
# the dataset's sort guarantees, so the pipeline sees them as one market state.
gold = walk(GOLD, 120, P0, trend_step(Decimal("0.002")), start=week(14))
silver = walk(SILVER, 120, Decimal("31.50"), trend_step(Decimal("0.0022")),
              start=week(14))
emit("correlated_pair.csv", sorted(gold + silver, key=lambda r: (r[2], r[0])))

# --- T. day boundary -------------------------------------------------------
# Starts late in the UTC day so the series crosses midnight UTC and the venue's
# daily break, which the walk skips.
emit("day_boundary.csv",
     walk(GOLD, 90, P0, trend_step(Decimal("0.0012")),
          # Late in the UTC day, so the series crosses midnight UTC and the
          # venue's daily break, on its own week.
          start=week(16) + timedelta(hours=14)))

# ---------------------------------------------------------------------------
# The condition scenarios: D, G, I and N.
#
# # Why these need their own fixtures at all
#
# Three of the four are not new MARKET shapes. An economic release, a news
# item and an exhausted risk budget are conditions applied to an ordinary
# market, and the behaviour under test is what the platform does when a
# tradable signal meets one of them.
#
# They still need their own datasets, for the reason `week` exists: a strategy
# is evaluated once per completed bar and that is recorded permanently in
# strategy_runs, which a replay's purge deliberately does not touch. Two
# scenarios sharing one dataset would mean the second found every bar already
# evaluated and produced nothing -- which reads as a broken pipeline rather
# than as a spent fixture. So each gets its own two-week slot.
#
# D, I and N are therefore deliberately the same trend shape as A. The market
# is the control; the condition is the variable.

# --- G. conflicting strategy signals ---------------------------------------
# Five bars up hard, two down hard. The net drift is strongly positive, so a
# trend or momentum strategy should read BUY -- while each impulse leaves the
# price stretched well away from its own mean, which is what an oscillator or
# a mean-reversion strategy reads as SELL.
#
# Whether the seeded strategy set actually splits on this is an empirical
# question the fixture cannot settle, so the scenario VERIFIES that
# disagreement occurred and skips rather than passing if it did not. A fixture
# that merely hoped for conflict would otherwise assert nothing.
def conflicting_step(i, price):
    if i % 7 in (5, 6):
        return price * Decimal("0.9965")
    return price * Decimal("1.0030")


# 180 instants, not 120, and the reason is measured rather than chosen.
#
# The first 60 are warm-up and produce no executable intent. Of the remainder,
# the seeded strategy set produces NOTHING actionable until roughly 120 bars of
# history exist: a 120-bar version of this exact trend yielded 288 no_trade
# signals and ONE sell, while the 140-bar trend-clean fixture yields 46
# decisions. The signal region begins where the shorter dataset ends.
#
# So a condition scenario on a 120-bar market tests the condition against a
# market that never traded -- which passes, or skips, for entirely the wrong
# reason. 180 leaves about 60 instants inside the signal region.
CONDITION_INSTANTS = 180

emit("conflicting_signals.csv",
     walk(GOLD, CONDITION_INSTANTS, P0, conflicting_step, wick=Decimal("0.0009"),
          start=week(18)))

# --- D. high-impact economic event -----------------------------------------
emit("event_window.csv",
     walk(GOLD, CONDITION_INSTANTS, P0, trend_step(Decimal("0.002")),
          start=week(20)))

# --- I. strong signal, no risk capacity ------------------------------------
emit("capacity_exhausted.csv",
     walk(GOLD, CONDITION_INSTANTS, P0, trend_step(Decimal("0.002")),
          start=week(22)))

# --- N. news alongside strategy agreement ----------------------------------
emit("news_agreement.csv",
     walk(GOLD, CONDITION_INSTANTS, P0, trend_step(Decimal("0.002")),
          start=week(24)))

# --- Partial-fill testability ----------------------------------------------
# A dataset on the SYNTHETIC instrument, which exists only so a partial fill is
# a representable quantity at all. See the TEST_XAU comment in internal/seed.
#
# A replay serves quotes only for the instruments its dataset carries, so
# without this fixture the synthetic instrument has no price during a replay
# and every order against it is correctly refused for a stale feed.
#
# Gentle drift rather than a trend: the point of this run is the venue's fill
# behaviour, not the market's. A strong move would add price movement to a
# reconciliation the test wants to attribute entirely to the split fill.
TEST_GOLD = "TEST_XAU"


def gentle_step(i, price):
    return price * (Decimal("1.0004") if i % 2 == 0 else Decimal("0.9997"))


emit("test_partial_fill.csv",
     walk(TEST_GOLD, 80, P0, gentle_step, wick=Decimal("0.0004"),
          start=week(26)))
