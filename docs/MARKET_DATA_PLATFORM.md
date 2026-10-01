# Market data: Vantage owns it

How prices get in, what happens to them, and who is allowed to touch them.

`MARKET_DATA.md` covers the live quote path and data-quality verdicts.
This covers historical acquisition, storage, provenance and the chart.

## The shape of it

```
Twelve Data
    ↓            (server side only -- the browser never gets here)
TwelveDataProvider
    ↓            marketdata.Provider, the same seam as mock and replay
Syncer           chunk → fetch → validate → store → record provenance
    ↓
market_bars      PostgreSQL. Vantage's data now, not the provider's.
    ↓
research · replay · indicators · strategies · scanner · paper-forward
    ↓
Vantage API      /market-data/*, /market/bars
    ↓
Lightweight Charts → terminal
```

The provider is replaceable. The stored series is the asset.

## The browser never talks to a provider

The API key lives in the control plane, the outbound request is made there,
and what reaches the terminal is Vantage's own stored data. A front end that
could call a provider directly would need the key in a bundle, and a bundle is
public.

A Playwright test watches the network and fails if the browser requests a
provider host.

## The key is optional

No `TWELVE_DATA_API_KEY` means the provider reports `MISCONFIGURED` and refuses
every acquisition. It does **not** mean the platform fails to start: synthetic
data, replay and everything already stored keep working, and the terminal says
why acquisition is unavailable. Taking a whole trading platform down over an
optional feature would be a worse failure than the missing feature.

The key never reaches a response, a log, a decision snapshot or the
configuration digest. Tests pin all four.

## The provider URL is configuration, never a parameter

Fixed at construction, validated as an https origin with no path or query.
Redirects are refused -- following one would carry the API key to wherever the
provider pointed. Nothing in the request path is built from client input: a
caller names an instrument, a timeframe and a range, and never a URL.

## Symbols are translated once

Vantage says `XAUUSD`. Twelve Data says `XAU/USD`. A broker will say something
else. The translation happens at the provider boundary in both directions, and
everything upstream speaks canonical ids only -- otherwise changing provider
stops being configuration and becomes a search-and-replace.

The mapping is an explicit table, not a transformation. "Insert a slash before
the last three characters" works for XAUUSD and fails *silently* for the next
instrument, by succeeding against a symbol that is not the one asked for.

**`XAUUSD.m` is deliberately not mapped.** That is a broker contract with its
own specification and session; Twelve Data quotes spot gold. Treating one as
the other's history would produce research about one instrument presented as
research about another.

## Three operations, one path

| | Chooses its ranges by |
|---|---|
| `BACKFILL` | what the operator asked for |
| `SYNC` | everything after the newest stored bar |
| `REPAIR` | the gaps, and nothing else |

Everything after that -- chunking, fetching, validating, storing, recording -- is
shared. Three copies of "fetch and store" would drift and only one would get
the fix.

Chunks are deterministic and bounded below the provider's own 5000 ceiling: a
request at exactly the limit cannot distinguish "this is all there is" from
"this is all that fits", and that difference decides whether to ask again.

Rate limiting is a fixed window, deliberately under the plan's allowance.
Reacting to 429s instead would make being throttled the normal operating mode,
and a throttled provider reads downstream as an outage. Retries are bounded --
never infinite, because a provider that is down stays down and an unbounded
retry turns one outage into an outage plus a request flood.

## Nothing is ever fabricated

A provider returning less than was asked for produces a `PARTIAL` segment and
the shortfall stays visible. No bar is interpolated, forward-filled or
synthesised to close a gap. A fabricated candle is a fabricated trade.

The requested and received ranges are separate columns for the same reason: a
provider whose plan stops short returns a shorter window, and recording the
returned range as though it were the requested one would erase the fact that
four years were asked for and are not there.

## Idempotent by construction

`market_bars` has `PRIMARY KEY (instrument_id, timeframe, open_time)` -- that IS
the canonical bar identity -- and ingestion upserts on it. Fetching the same
period twice cannot produce a duplicate.

Stored and duplicate counts are tracked separately, because a backfill
reporting thousands of bars ingested when it re-fetched an existing range would
hide that it did no work.

## Closed markets are not gaps

The venue shuts every weekend and for an hour each weekday. Reporting those as
missing data makes a complete dataset look riddled with holes and buries the
one real gap among fifty weekends -- the store's gap query is deliberately
structural, and the classification happens where a market clock exists.

On the current development data: 0 real gaps, 27 closed-market absences.

## Closed bars only

The current partial candle is excluded from sync and never stored as history.
A strategy that treats an incomplete bar as an observation is reading a value
that will change, and on the next tick its own history rewrites itself.

## UTC, stated rather than assumed

Requests ask for UTC explicitly rather than accepting the provider's default,
which is the exchange's zone. Nothing consults the host's timezone: a
developer's locale is not a property of the market.

## Research datasets are immutable identities

A research run that says "XAUUSD 1h from 2020 to 2024" references a *mutable*
thing -- the next sync appends bars and the run stops being reproducible while
looking identical. A snapshot fixes an identity: instrument, timeframe,
provider, range, normalization version and a deterministic hash of the bars.

The bars are not copied into the record. The hash detects drift; a snapshot
whose recomputed hash no longer matches must not be used.

`INVALID` data cannot become a dataset. The code refuses it and a CHECK
constraint refuses it, because a research dataset built on refused bars is the
one mistake this whole layer exists to prevent and one enforcement point is not
enough for it.

### Source type is derived, never assumed

The first working version hard-coded `HISTORICAL_MARKET` and cheerfully
labelled bars from the `mock` generator as real market evidence. Two milestones
of work exist to keep generated and observed data apart, and one constant
walked past all of it.

It is now derived from which providers supplied the bars. `mock` and `replay`
are synthetic; anything else is real, which is the safe default for a new
provider. **A mixed range is refused** rather than labelled: calling it
historical smuggles generated bars into real evidence, calling it synthetic
discards real observations, and a source type that is a compromise would be
inherited silently by every conclusion drawn from it.

### The manifest carries provenance across the process boundary

The research plane is Python and does not touch the control plane's database
(ENGINEERING_GUIDE.md rule 5), so a snapshot is materialised as a CSV in the allowlisted
research directory. A CSV cannot say what it is -- and the Python importer
previously assumed everything in that directory was `HISTORICAL_MARKET`, which
is right for a file an operator placed by hand and wrong for a snapshot Vantage
exported from generated bars. The same defect, one process boundary away.

Each export now carries a `.manifest.json` with source type, provider, both
hashes, range, versions, quality and timezone. The importer reads it; an
unrecognised source type is refused rather than defaulted, and a malformed
manifest is ignored entirely rather than half-applied.

## The chart

TradingView Lightweight Charts is the **renderer** and nothing else. Every bar
it draws came from Vantage's store through Vantage's API. It is never asked
where the market is.

The chart instance is created once per mount and fed imperatively. Re-creating
it on every render would discard the user's zoom and pan on each market tick.
Panning past the oldest loaded bar requests one page backwards, de-duplicated
at the seam because the library throws on a repeated timestamp.

Prices become floats exactly once, for canvas coordinates. That is allowed
because a pixel is presentational and never returns to the server.

Markers show PAPER decisions and simulated orders, labelled as such. A NO TRADE
decision is drawn as a neutral dot rather than an arrow: a refusal is a
first-class outcome and drawing it like an entry would make refusals look like
trades.

## RBAC

Reading is broadly permitted -- an operator who cannot see what data the
platform holds cannot interpret anything built on it. Acquiring is ADMIN:
each call spends a metered external quota and writes to the bar series every
strategy reads. The role is enforced by the router group, not by a check inside
a handler, and a test reads the route table to prove it.

## Provider differences are a future research problem

A decentralised FX/metals price is not one number. Twelve Data's XAUUSD will
not equal a broker's XAUUSD bar for bar, and nothing here assumes it does.
Market-price research and broker-specific execution validation are separate
questions and are kept separate.

## Costs

No provider in this layer supplies bid/ask, so transaction costs are an
**assumption**. Every dataset built from these bars carries
`cost_basis = ESTIMATED_COSTS`. A backtest reporting costs it never paid is
worse than one that admits it is estimating.

## Current status

**`TWELVE_DATA_CONFIGURATION_REQUIRED`.** No API key is configured on this
machine, so no real bars have been acquired. The provider, chunking, rate
limiting, storage, provenance, quality gate, coverage, gap repair, snapshots
and chart are built and tested against a mocked provider; what is missing is
the key.
