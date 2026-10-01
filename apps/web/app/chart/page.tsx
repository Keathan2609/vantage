"use client";

import { Suspense, useCallback, useMemo, useState } from "react";
import { useSearchParams } from "next/navigation";

import { PriceChart, type ChartMarker } from "@/components/PriceChart";
import { Empty, Panel } from "@/components/ui";
import { api, type Bar } from "@/lib/api";
import { decimal, percent, time } from "@/lib/format";
import { useAsync, useVantage } from "@/lib/store";

const TIMEFRAMES = ["15m", "1h", "4h"] as const;

/** Bars fetched per page. Bounded on the server too; this is what one pan
 *  backwards asks for, not a ceiling. */
const PAGE = 500;

export default function ChartPage() {
  return (
    <Suspense fallback={<Empty>Loading chart…</Empty>}>
      <ChartView />
    </Suspense>
  );
}

function ChartView() {
  const params = useSearchParams();
  const { quotes, account } = useVantage();
  const instruments = useAsync(() => api.instruments(true), []);

  const [instrumentId, setInstrumentId] = useState(
    params.get("instrument") ?? "XAUUSD.m",
  );
  const [timeframe, setTimeframe] = useState<(typeof TIMEFRAMES)[number]>("1h");

  const available = instruments.data?.instruments ?? [];
  const selected = available.find((i) => i.id === instrumentId) ?? available[0];

  // The most recent page. Older pages are appended to `history` as the user
  // pans, so the chart grows backwards without ever re-fetching what it holds.
  const recent = useAsync(
    () =>
      selected
        ? api.bars(selected.id, timeframe, PAGE)
        : Promise.resolve({ instrument_id: "", timeframe, bars: [] as Bar[] }),
    [selected?.id, timeframe],
  );

  const [loadingOlder, setLoadingOlder] = useState(false);

  // Older pages are keyed by the series they belong to. Switching instrument
  // or timeframe therefore invalidates them by definition rather than by an
  // effect that clears state after the fact -- which would render one frame
  // showing the previous instrument's history under the new instrument's name.
  const [pages, setPages] = useState<{ key: string; bars: Bar[]; exhausted: boolean }>({
    key: "",
    bars: [],
    exhausted: false,
  });
  const seriesKey = `${selected?.id ?? ""}:${timeframe}`;
  // Memoised so the empty-history fallback is a stable array rather than a new
  // one each render, which would make every derived memo below recompute on
  // every market tick.
  const older = useMemo(
    () => (pages.key === seriesKey ? pages.bars : []),
    [pages, seriesKey],
  );
  const exhausted = pages.key === seriesKey && pages.exhausted;

  const loadOlder = useCallback(
    async (oldest: string) => {
      if (!selected || loadingOlder || exhausted) return;
      const key = `${selected.id}:${timeframe}`;
      setLoadingOlder(true);
      try {
        const page = await api.bars(selected.id, timeframe, PAGE, oldest);
        const bars = page.bars ?? [];
        setPages((current) => {
          const base = current.key === key ? current : { key, bars: [], exhausted: false };
          // An empty page means the beginning of the stored series. Recording
          // it stops the chart asking again on every further pan.
          if (bars.length === 0) {
            return { ...base, exhausted: true };
          }
          return { ...base, bars: [...bars, ...base.bars] };
        });
      } finally {
        setLoadingOlder(false);
      }
    },
    [selected, timeframe, loadingOlder, exhausted],
  );

  const series = useMemo(() => {
    const head = recent.data?.bars ?? [];
    if (older.length === 0) return head;
    // De-duplicated at the seam. Two pages can overlap by a bar, and the
    // chart library throws on a repeated timestamp rather than ignoring it.
    const seen = new Set(older.map((b) => b.open_time));
    return [...older, ...head.filter((b) => !seen.has(b.open_time))];
  }, [recent.data, older]);

  const quote = selected ? quotes[selected.id] : undefined;

  // --- provider and coverage ------------------------------------------------
  const providerStatus = useAsync(() => api.marketDataStatus(), []);
  const coverage = useAsync(
    () =>
      selected
        ? api.marketDataCoverage(selected.id, timeframe)
        : Promise.resolve(null),
    [selected?.id, timeframe],
  );

  // --- markers --------------------------------------------------------------
  const decisions = useAsync(
    () => (account ? api.decisions(account.id, 200) : Promise.resolve(null)),
    [account?.id],
  );

  const markers = useMemo<ChartMarker[]>(() => {
    const rows = decisions.data?.decisions ?? [];
    if (!selected) return [];
    return rows
      .filter((d) => d.InstrumentID === selected.id)
      .map((d) => {
        const action = (d.SignalAction ?? "").toLowerCase();
        const executed = d.Outcome === "EXECUTED";
        const side: ChartMarker["side"] =
          !executed || action === "flat" || action === "no_trade"
            ? "flat"
            : action === "buy"
              ? "buy"
              : "sell";
        return {
          time: d.CreatedAt,
          side,
          kind: executed ? "order" : "decision",
          // NO TRADE is labelled as such rather than omitted. A refusal is a
          // first-class outcome here and an operator needs to see that the
          // platform looked and declined, not an empty stretch of chart.
          text: executed
            ? `${action.toUpperCase()}${d.ApprovedQty ? ` ${d.ApprovedQty}` : ""}`
            : "NO TRADE",
        } satisfies ChartMarker;
      });
  }, [decisions.data, selected]);

  const summary = useMemo(() => {
    if (series.length < 2) return null;
    const first = series[0];
    const last = series[series.length - 1];
    if (!first || !last) return null;
    const open = Number.parseFloat(first.open);
    const close = Number.parseFloat(last.close);
    return {
      change: close - open,
      changePct: open > 0 ? (close - open) / open : 0,
      high: Math.max(...series.map((b) => Number.parseFloat(b.high))),
      low: Math.min(...series.map((b) => Number.parseFloat(b.low))),
      bars: series.length,
      from: first.open_time,
      to: last.open_time,
    };
  }, [series]);

  const provider = providerStatus.data?.provider;
  const held = coverage.data?.coverage;
  const gaps = coverage.data?.gaps ?? [];

  return (
    <>
      <Panel
        title={selected ? `${selected.symbol} · ${timeframe}` : "Chart"}
        note={
          quote
            ? `bid ${decimal(quote.bid, selected?.price_precision ?? 2)} · ask ${decimal(
                quote.ask,
                selected?.price_precision ?? 2,
              )} · spread ${percent(quote.spread_fraction, 4)}`
            : undefined
        }
        actions={
          <>
            <select
              value={selected?.id ?? ""}
              onChange={(event) => setInstrumentId(event.target.value)}
              style={{ width: 150 }}
              aria-label="Instrument"
            >
              {available.map((instrument) => (
                <option key={instrument.id} value={instrument.id}>
                  {instrument.symbol}
                </option>
              ))}
            </select>
            <div className="segmented">
              {TIMEFRAMES.map((tf) => (
                <button
                  key={tf}
                  data-selected={tf === timeframe}
                  onClick={() => setTimeframe(tf)}
                >
                  {tf}
                </button>
              ))}
            </div>
          </>
        }
        flush
      >
        {recent.loading && series.length === 0 ? (
          <Empty>Loading bars…</Empty>
        ) : (
          <div data-testid="price-chart">
            <PriceChart
              bars={series}
              markers={markers}
              onNeedOlderBars={loadOlder}
              emptyMessage={
                provider && !provider.configured
                  ? "No stored bars. The market-data provider is not configured."
                  : `No completed bars for ${selected?.symbol ?? "this instrument"} at ${timeframe}.`
              }
            />
          </div>
        )}
      </Panel>

      {/* Where these bars came from. Provenance visible to the operator rather
          than buried in a database: a chart that cannot say which provider
          produced it, how fresh it is and whether it has holes is a chart that
          invites trust it has not earned. */}
      <Panel
        title="Data source"
        note={
          loadingOlder
            ? "loading older history…"
            : exhausted
              ? "start of stored history reached"
              : undefined
        }
      >
        <div className="kv-grid" data-testid="market-data-provenance">
          <div>
            <dt>Provider</dt>
            <dd>{provider?.provider ?? "-"}</dd>
          </div>
          <div>
            <dt>Feed status</dt>
            <dd data-testid="provider-state">{provider?.state ?? "UNKNOWN"}</dd>
          </div>
          <div>
            <dt>Last provider success</dt>
            <dd>
              {provider?.last_success_at ? time(provider.last_success_at) : "-"}
            </dd>
          </div>
          <div>
            <dt>Newest market timestamp</dt>
            <dd>
              {provider?.last_market_timestamp
                ? time(provider.last_market_timestamp)
                : "-"}
            </dd>
          </div>
          <div>
            <dt>Local history</dt>
            <dd>
              {held?.earliest_bar && held?.latest_bar
                ? `${time(held.earliest_bar)} → ${time(held.latest_bar)}`
                : "none stored"}
            </dd>
          </div>
          <div>
            <dt>Bars held</dt>
            <dd>{held ? held.bars.toLocaleString() : "-"}</dd>
          </div>
          <div>
            <dt>Gaps</dt>
            <dd data-testid="coverage-gaps">
              {gaps.length === 0
                ? "none detected"
                : `${gaps.length} (${gaps.reduce((n, g) => n + g.missing_bars, 0)} bars)`}
            </dd>
          </div>
          <div>
            <dt>Last sync</dt>
            <dd>
              {held?.last_sync_at
                ? `${held.last_sync_kind ?? ""} ${held.last_sync_status ?? ""} · ${time(held.last_sync_at)}`
                : "never"}
            </dd>
          </div>
        </div>
        {provider && !provider.configured && (
          <p className="note" data-testid="provider-unconfigured">
            TWELVE_DATA_CONFIGURATION_REQUIRED: no API key is configured, so no
            new history can be acquired. Everything already stored still works.
          </p>
        )}
      </Panel>

      {summary && (
        <Panel title="Window">
          <div className="kv-grid">
            <div>
              <dt>Change</dt>
              <dd className={summary.change >= 0 ? "positive" : "negative"}>
                {summary.change.toFixed(selected?.price_precision ?? 2)} (
                {(summary.changePct * 100).toFixed(2)}%)
              </dd>
            </div>
            <div>
              <dt>High</dt>
              <dd>{summary.high.toFixed(selected?.price_precision ?? 2)}</dd>
            </div>
            <div>
              <dt>Low</dt>
              <dd>{summary.low.toFixed(selected?.price_precision ?? 2)}</dd>
            </div>
            <div>
              <dt>Bars loaded</dt>
              <dd>{summary.bars.toLocaleString()}</dd>
            </div>
            <div>
              <dt>From</dt>
              <dd>{time(summary.from)}</dd>
            </div>
            <div>
              <dt>To</dt>
              <dd>{time(summary.to)}</dd>
            </div>
          </div>
          {/* Simulated trades must never look like live execution. */}
          <p className="note">
            Markers show PAPER decisions and simulated orders only. No live
            execution exists in this build.
          </p>
        </Panel>
      )}
    </>
  );
}
