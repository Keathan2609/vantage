# News and economic calendar

Two provider interfaces in `internal/econdata`, both with deterministic mock
implementations in this build:

```go
type CalendarProvider interface {
    Name() string
    Events(ctx context.Context, from, to time.Time) ([]Event, error)
}

type NewsProvider interface {
    Name() string
    Items(ctx context.Context, since time.Time, limit int) ([]Headline, error)
}
```

An `Ingestor` pulls from both and upserts by `(source, external_id)`, so a
refresh is idempotent: running it a hundred times produces the same rows rather
than a hundred copies of the calendar. The scheduler refreshes every 15
minutes, and the development seed uses the same ingestion path rather than
holding a second copy of the fixtures -- a duplicate set in the seed would be a
second source of truth that could disagree with the provider.

A failed refresh is logged and retried on the next tick. It leaves the last
known calendar in force rather than silently removing every blackout, which is
the safer failure: a stale blackout costs a trade, a missing one costs a
position.

The Calendar page states that the data is deterministic development fixture
data rather than a licensed feed. That is not modesty -- a calendar that looks
authoritative and is not is *worse* than no calendar, because an operator would
plan around it.

## Provider terms

**Nothing here scrapes any third-party site.** No provider's terms of use are
worked around, and by explicit instruction the architecture does not depend on
scraping Forex Factory or any comparable source.

A licensed calendar or news feed is an adapter plus configuration. Licensing is
a commercial decision for the operator; the code is shaped so that the decision
does not require a redesign.

## Calendar data

`economic_events` holds:

| Field | Notes |
| --- | --- |
| `scheduled_at` | UTC |
| `country`, `currency` | For instrument mapping |
| `impact` | `low`, `medium`, `high` |
| `event_name` | e.g. Non-Farm Payrolls |
| `actual`, `forecast`, `previous`, `revised` | Strings, because releases are published in mixed units (`4.50%`, `185K`, `-73.8B`) |
| `status` | Scheduled, released, revised |
| `source` | Which provider supplied it |

`economic_event_instrument_map` links an event to the instruments it affects,
so a USD release blacks out gold (quoted in USD) rather than only currency
pairs whose symbol contains "USD".

Actual, forecast and previous are deliberately **strings**. Coercing them to
numbers loses the unit, and a system that thinks payrolls of "185K" is the
number 185 will eventually compare it to something.

## Blackout windows

Configured per account, on the Risk page:

| Setting | Default | Meaning |
| --- | --- | --- |
| `event_blackout_before_minutes` | 15 | Before a high-impact release |
| `event_blackout_after_minutes` | 10 | After |
| `block_on_high_impact_events` | true | Whether it is enforced at all |

**Only high-impact events halt trading.** Blacking out on every scheduled
release would leave almost no tradable window on a gold instrument, which is a
different way of not having a risk control.

**The blackout applies to automated orders.** A manual order is allowed through
with the event surfaced in the interface. From the risk engine's own comment:
the operator has been told, and blocking a person from closing a position
before a release would be worse than the risk it prevents. Automation gets no
such judgement, so it gets the hard block.

The refusal is a normal risk check, `event_risk`, producing
`event_risk_blackout`, recorded like any other with its window and the event
that caused it.

## Why event risk is treated as a first-class control

Around a high-impact release three things change at once, and all three break
assumptions a backtest made:

1. **The spread widens**, often by a multiple, so entry cost is not what was
   modelled.
2. **Slippage stops resembling anything measured.** A stop placed before the
   release is not the stop that gets filled.
3. **Direction becomes close to a coin flip** on the release itself, while
   position size was chosen on the assumption of an ordinary distribution.

Refusing to *open* exposure into that window costs the occasional good trade
and removes a class of loss no model in this platform can price.

## The `pre_event_blackout` strategy

There is also a strategy in the registry that closes or avoids positions ahead
of events, distinct from the risk control. The difference matters: the risk
check is a *refusal* imposed on everything automated, while the strategy is an
*opinion* that can be backtested, evaluated and compared. Keeping them separate
means the control cannot be tuned away by strategy parameters.

## News

`news_items` holds a headline, an optional summary, publication time, related
instruments and currencies, an optional sentiment score and topics.

Sentiment is displayed as a number with no interpretation attached, and nothing
in the trading path consumes it. A sentiment score from a mock provider driving
a real order would be theatre. When a licensed feed with a defensible sentiment
model is attached, that becomes a design decision to make deliberately -- and
it should be backtested like any other feature before it influences anything.

## What is missing

- **No live feed.** Both providers are mocks.
- **No revision handling.** A revised release updates the row; the platform
  does not currently model the trading implications of a revision separately
  from the original.
- **No news-driven halt.** Only scheduled calendar events produce a blackout.
  An unscheduled headline -- a central-bank intervention, a geopolitical event --
  is not detected, and the honest mitigation for that is the spread check and
  the data-health check, both of which react to the market rather than to the
  news.
