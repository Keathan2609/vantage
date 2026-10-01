# Gold research profile (XAUUSD)

XAUUSD is the primary research instrument. This document records what is
specific about trading it, so those characteristics inform strategy design and
parameter choice without being compiled into the platform.

**Nothing here is a prediction, and no figure here is a claim about
profitability.** Where a number appears it is either a specification of the
instrument as this build models it, or a threshold configured in this
repository, and it is labelled as which.

## The rule this document exists to protect

**Gold assumptions do not belong in generic infrastructure.**

The risk engine, the OMS, the market clock and the data-quality policy must
work for an instrument nobody has thought about yet. Every gold-specific
observation below therefore belongs in one of three places:

| Where | What goes there | Why |
| --- | --- | --- |
| Instrument specification (`instruments` table) | contract size, tick size, margin rate, commission, swap, min/max/step quantity | Data. The platform reads it; no code branches on the symbol. |
| Strategy parameters (`strategy_versions.parameters`) | lookbacks, ATR multiples, session filters, thresholds | Per-strategy and per-version, so a change is attributable and backtestable. |
| Risk limits (`risk_limits`) | spread ceiling, exposure caps, blackout windows | Per-account, so a different account may trade the same instrument differently. |

If something cannot be expressed in one of those, it is a sign the abstraction
is wrong rather than that gold is special.

Checked, as of this revision: `grep -rn XAUUSD internal/ --include=*.go`
excluding tests returns twelve lines, and every one is a comment, a calendar
name, a mock-fixture row or an entry in the mock provider's starting-price map.
There is no branch on the symbol in any decision path. That is the property to
re-check when adding an instrument, because it is the one that decays quietly.

## What this build actually models

From the seeded instrument specification. These are the numbers a strategy and
the risk engine see, not observations about the real market:

| | `XAUUSD` | `XAUUSD.m` (micro) |
| --- | --- | --- |
| Contract size | 100 oz | 1 oz |
| Quote currency | USD | USD |
| Base | XAU | XAU |
| Session calendar | `fx_metals_24x5` | `fx_metals_24x5` |

The micro contract exists for a reason that matters more than it looks: on a
R500 account, one standard 100 oz contract is roughly 48,000 ZAR of notional at
seeded prices, which is about 96 times the account. The account cannot trade
standard gold at all, and the honest result is NO TRADE. `XAUUSD.m` is what
makes the small-account case a real test rather than a permanent refusal.

## Spread sensitivity

Gold's spread is wider than a major FX pair's in absolute terms and moves more
with liquidity. Two consequences the platform already encodes:

- **Spread is a cost, not a detail.** The backtester subtracts it (`CostModel`),
  the scanner subtracts it from an instrument's score rather than ranking on
  the setup alone, and the risk engine refuses an order when the spread exceeds
  the account's ceiling (`max_spread_fraction`, default 0.5% of mid).
- **A strategy whose edge is smaller than the spread has no edge.** The
  backtester's `Frictionless` flag and its methodology warnings exist so a
  zero-cost run cannot be mistaken for a result.

The mock venue widens the spread outside liquid hours deliberately, so a
strategy that only works when the book is tight fails visibly rather than
quietly. `TestTheSpreadWidensOutsideLiquidHours` asserts it stays that way.

## Sessions

The venue calendar is `fx_metals_24x5`: Sunday 17:00 New York to Friday 17:00
New York, with a daily maintenance break 17:00 -- 18:00 New York, Monday to
Thursday.

| Session | Local window | What it means for gold |
| --- | --- | --- |
| Sydney | 07:00 -- 16:00 Australia/Sydney | Thin. Wide spreads, little follow-through. |
| Tokyo | 09:00 -- 18:00 Asia/Tokyo | Physical-demand flow; ranges more often than trends. |
| London | 08:00 -- 16:30 Europe/London | The main liquidity session for metals. |
| New York | 08:00 -- 17:00 America/New_York | US data and rates flow. |
| **Overlap** | London afternoon / New York morning | The deepest book of the day, and where most directional moves resolve. |

Two practical points:

- **The overlap is where a breakout strategy is most likely to be real.** A
  break of prior structure in the Tokyo session is more often noise; the same
  break in the overlap has volume behind it. `session_london_breakout` encodes
  this as a parameter, not as a hard-coded hour.
- **The daily break is not a gap to trade.** Ingestion stores nothing during
  it and the market clock reports `MarketClosedBreak`. A strategy that treated
  the 18:00 reopen as a breakout would be trading the venue's maintenance
  window. This has already cost a test run: any suite that waits for a
  tradable feed simply cannot run between 17:00 and 18:00 New York, and the
  failure reads as broken market data. The replay provider exists partly so
  that is no longer a constraint on testing.

## Volatility regime

Gold alternates between long compressions and violent expansions, more sharply
than the FX majors. The regime classifier reads this from ATR relative to its
own 50-bar baseline rather than from an absolute threshold, which is what makes
the same rule meaningful for a $2,650 gold price and a 1.08 EURUSD price:

| Ratio to baseline | Regime |
| --- | --- |
| > 1.5 | `HIGH_VOLATILITY` |
| < 0.6 | `LOW_VOLATILITY` |
| otherwise, ADX > 25 | `TRENDING` |
| otherwise, ADX < 20 | `RANGING` |
| otherwise | `UNKNOWN` |

`UNKNOWN` is a real answer and is never coerced. A strategy that declares
itself valid only in trends does not run in a market nobody can characterise --
see `domain.Regime` and the consensus policy.

**A stop sized for a compressed market is not a stop in an expanded one.** This
is why sizing derives from the distance to the stop and the account's risk
budget, and why every strategy that offers a stop derives it from ATR rather
than from a fixed number of dollars.

## Event risk

Gold is a rates-and-inflation instrument as much as a metal. The USD releases
that matter most:

| Event | Why gold cares |
| --- | --- |
| FOMC rate decision and statement | The dominant driver. Real yields are gold's opportunity cost. |
| FOMC minutes / speeches | Second-order, but repricing happens on tone. |
| CPI (headline and core) | Inflation surprises move real yields immediately. |
| PCE deflator | The Fed's preferred measure; less traded, still repriced. |
| Non-farm payrolls | Employment feeds the rate path. |
| Average hourly earnings | Read as an inflation input, released alongside NFP. |
| ISM manufacturing / services | Growth expectations. |
| Retail sales | Growth expectations. |
| US 10-year auctions and yield moves | The transmission mechanism for all of the above. |
| Geopolitical risk-off episodes | Gold's safe-haven behaviour; not on a calendar. |

Handling, as configured:

- High-impact events inside the account's blackout window
  (`event_blackout_before_minutes` / `event_blackout_after_minutes`) make the
  regime `EVENT_RISK` and **veto** an automated trade outright. There is no
  confidence level that reaches past it.
- Medium impact raises the net-confidence requirement rather than vetoing. A
  release that is close but not imminent should make the platform choosier, not
  blind.
- The blackout **after** an event exists because the first move is frequently
  reversed. Trading the spike is not trading the news.
- Manual orders are not blacked out. The operator has been told and is
  deciding.
- **A control that limits loss must never prevent closing.** Any order that
  strictly reduces exposure passes the blackout, the notional cap and the
  loss ceilings. Three separate checks were once found refusing a flatten; the
  rule is now written into `internal/risk/engine.go`.

The last point is the one most specific to gold: an event-driven gap is exactly
when an operator most needs to be able to get out, and exactly when a naive
blackout would stop them.

The calendar is currently **development fixtures**. No provider is contacted,
and `docs/NEWS_AND_CALENDAR.md` records the boundaries for a future
integration. Nothing in this repository scrapes a news site.

## Abnormal volatility

Distinct from `HIGH_VOLATILITY`, which is a regime the platform trades
differently. Abnormal means the data itself is suspect:

- a spread beyond the account ceiling → refuse
- a crossed or non-positive book → the quote is not stored at all
- a quote older than the policy allows, measured from **both** its arrival and
  the venue's own timestamp → automation stops

That second timestamp was added in this milestone. A provider that reconnects
and replays a ten-minute-old gold price previously read as fresh, because
freshness was measured from when the platform received it.

## Small-account constraints (the R500 case)

The seeded paper account is 500 ZAR. With USD/ZAR at 18.25 that is about $27.
This is not a toy: it is the constraint that makes every refusal path real.

At seeded prices, on `XAUUSD.m` (1 oz, 0.5% margin):

- 0.01 lots is roughly $0.13 of margin -- affordable
- but the **minimum quantity** and **step** decide whether a risk-derived size
  can be expressed at all
- and a risk budget of 1% of 500 ZAR is 5 ZAR, which at a typical ATR stop
  distance often sizes below the instrument minimum

**When a risk-derived size rounds below the instrument minimum, the answer is
NO TRADE.** Not the minimum size -- that would take more risk than the account
permits, which is the one thing sizing may never do. `risk.Size` returns
infeasible with the reason, and for this account that is the common outcome.

Standard `XAUUSD` is simply unaffordable here, and the platform says so:
`exposure_limit_breached`, notional 48,223 ZAR against a 2,500 ZAR limit. That
refusal is asserted in the smoke suite.

**Broker assumptions must not be adjusted to manufacture a trade.** If the
contract size, minimum quantity and margin rate make gold untradable on R500,
the correct output is a refusal with its arithmetic shown.

## What is deliberately not modelled

- **Correlation with silver, the dollar index or real yields.** Exposure and
  concentration are per-instrument, so gold and silver currently count as
  diversification when they are frequently one trade. Named in
  `docs/RISK_ENGINE.md` and in the engineering report as a real gap.
- **Physical-market structure** -- lease rates, ETF flows, central-bank
  purchases. Not available to this build and not simulated.
- **The futures basis and roll.** This is a spot CFD model; there is no
  contract to roll.
- **Options-implied volatility.** No options data, so the volatility regime is
  realised-only and therefore backward-looking.

## Where these characteristics are exercised

| Concern | Where |
| --- | --- |
| Session behaviour | `domain.ForexMetalsCalendar`, `session_london_breakout` |
| Volatility regime | `vantage_quant.scanner.classify_regime`, `atr_volatility_regime` |
| Spread cost | `vantage_quant.backtest.CostModel`, `risk` spread check, scanner scoring |
| Event risk | `econdata`, `pre_event_blackout`, the consensus event veto |
| Small-account sizing | `risk.Size`, and the R500 refusals in `tests/smoke` |
| Deterministic scenarios | `internal/orchestrator/scenario_test.go` |
