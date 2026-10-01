# Market data

Two responsibilities live in `internal/marketdata`, deliberately separate:

- **ingestion** -- getting prices in, from whatever provider supplies them
- **quality** -- deciding whether what arrived can be acted on

The second is not a filter applied at the edge. It is a *verdict recorded
alongside the data*, consulted by the order pipeline and surfaced in the UI. A
trading system that cannot say "I do not trust this price right now" will
eventually act on a price it should not have.

## The provider interface

```go
type Provider interface {
    Name() string
    Quote(ctx, instrument, now) (domain.Quote, error)
    HistoricalBars(ctx, instrument, tf, from, to) ([]domain.Bar, error)
}
```

Implementations are swappable and nothing downstream knows which is in use. In
this build there is one: a deterministic development generator that produces a
plausible two-sided market with a realistic spread. It is labelled as `mock` in
every quote row, every health record and the Connections page -- a synthetic
price that looks authoritative is worse than an obviously synthetic one.

## Ingestion

The scheduler polls every **2 seconds** and, for each enabled instrument,
writes `market_quotes` (history), `market_quotes_latest` (current) and a
`market_data_health` verdict.

Polling rather than streaming, for now, is a considered choice: this build's
data updates every couple of seconds, a 2-second poll of a small set of
instruments is cheap, and it has no reconnect semantics to get wrong. A
streaming provider is the right answer at higher frequency, and swapping one in
changes the ingestor only -- not the store, the risk engine or any page.

Bars are aggregated into `market_bars` per timeframe: `1m`, `5m`, `15m`, `1h`,
`4h`, `1d`. Only **completed** bars are served to research. The current,
still-forming bar is excluded, because an indicator computed on a partial bar
changes as the bar develops and produces a signal that could not have been
acted on at the time -- the most common form of look-ahead bias in retail
systems.

## Quality: five states

`EvaluateQuoteHealth` is a pure function of a quote, the previous quote, a
policy and the current time, so every boundary that decides whether money may
move is exhaustively unit-testable.

| State | Meaning | Automation |
| --- | --- | --- |
| `ok` | Fresh, sane, two-sided | Permitted |
| `degraded` | Suspect but usable -- e.g. older than 3s | **Refused for automation; a manual order is allowed with the state shown** |
| `stale` | Older than the maximum age | Refused |
| `invalid` | Structurally wrong: crossed book, non-positive price, regressed or future timestamp | Refused |
| `no_data` | Nothing has arrived | Refused |

Default policy:

| Threshold | Value | Reasoning |
| --- | --- | --- |
| `MaxQuoteAge` | 10s | Beyond this the price is not the market |
| `DegradedQuoteAge` | 3s | A five-second-old quote is not tradable by a system that acts in milliseconds |
| `MaxSpreadFraction` | 0.5% of mid | Wider than this is an abnormal book, not a tradable one |
| `MaxClockSkewAhead` | 2s | Tolerates provider clock drift; more is a broken timestamp |

The degraded case is where a real judgement is encoded: automation is refused,
but a human is allowed through with the state displayed. An operator who can
see "degraded, 4s old" can decide; a scheduler cannot.

## Issues recorded

A health record carries specific issues rather than a single opaque state, so
an operator can tell a slow provider from a broken one:

`stale_quote`, `crossed_book`, `non_positive_price`, `spread_abnormal`,
`timestamp_regressed`, `duplicate_tick`, `missing_bars`, `provider_down`,
`excessive_latency`, `future_timestamp`.

`timestamp_regressed` and `future_timestamp` deserve a note: both indicate the
provider's clock cannot be trusted, and a feed whose timestamps move backwards
will corrupt any bar aggregation built on it. They are treated as `invalid`
rather than `degraded` for that reason.

## The market clock

`domain.MarketClock` answers whether an instrument's market is open, and it
resolves session boundaries in the venue's IANA time zone -- New York for the FX
and metals calendar -- rather than in UTC with a fixed offset. That is what
makes the weekly open and close move correctly across daylight-saving changes
instead of drifting by an hour twice a year.

Four statuses:

| Status | Meaning |
| --- | --- |
| `open` | Tradable |
| `closed_weekend` | Between the Friday close and the Sunday open, in New York local time |
| `closed_holiday` | A date in `market_holidays` for the instrument's calendar |
| `closed_daily_break` | The venue's daily maintenance window |

Sessions (`sydney`, `tokyo`, `london`, `new_york`, and the London/New York
overlap) are reported for context and used by session-based strategies. The overlap is
reported explicitly because it is where volume and spread behaviour change
most.

Holidays are data, in `market_holidays`, keyed by calendar id -- not hardcoded
dates in Go.

## FX conversion

An account denominated in ZAR holding a position quoted in USD needs a rate,
and `internal/fx` gets one from `fx_rates_latest`. The important behaviour is
what happens when there is no rate: **conversion fails and the platform
refuses to trade or value the position**, rather than defaulting to 1.0 or to a
stale rate of unknown age.

The portfolio surfaces this as `unvalued_positions` with a `valuation_note`, so
an unvalued position is visibly unvalued rather than silently valued wrong. A
position that cannot be valued cannot be risk-checked, and a risk check on a
guessed rate is worse than no check because it looks like one.

## What the browser sees

One polling loop in `lib/store.tsx` feeds the whole terminal -- quotes, health
and market status every 2 seconds; the portfolio every 5. A dozen panels each
on their own timer would produce a dozen times the requests and a UI where
different panels disagree about the current price.

When a poll fails, the last known values stay on screen and a warning appears
in the top bar. Blanking the display would be worse: an operator would not know
whether the market stopped or the connection did.

## Provider terms

No third-party site is scraped and no provider's terms are worked around. This
build uses deterministic mock providers for prices, the economic calendar and
news. A licensed feed is a configuration change plus one adapter, and licensing
is a commercial decision that belongs to the operator, not to the code.

Specifically, and by instruction: nothing here scrapes Forex Factory, and no
architecture in this repository depends on scraping it or any comparable site.
