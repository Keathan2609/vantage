"use client";

import { Suspense, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "next/navigation";

import { Empty, Panel } from "@/components/ui";
import { api, type Bar } from "@/lib/api";
import { decimal, percent, time } from "@/lib/format";
import { useAsync, useVantage } from "@/lib/store";

const TIMEFRAMES = ["15m", "1h", "4h"] as const;

export default function ChartPage() {
  return (
    <Suspense fallback={<Empty>Loading chart…</Empty>}>
      <ChartView />
    </Suspense>
  );
}

function ChartView() {
  const params = useSearchParams();
  const { quotes } = useVantage();
  const instruments = useAsync(() => api.instruments(true), []);

  const [instrumentId, setInstrumentId] = useState(
    params.get("instrument") ?? "XAUUSD.m",
  );
  const [timeframe, setTimeframe] = useState<(typeof TIMEFRAMES)[number]>("1h");

  const available = instruments.data?.instruments ?? [];
  const selected = available.find((i) => i.id === instrumentId) ?? available[0];

  const bars = useAsync(
    () =>
      selected
        ? api.bars(selected.id, timeframe, 400)
        : Promise.resolve({ instrument_id: "", timeframe, bars: [] as Bar[] }),
    [selected?.id, timeframe],
  );

  const quote = selected ? quotes[selected.id] : undefined;

  // Memoised so the empty-history fallback is a stable array rather than a new
  // one each render, which would make every derived memo below recompute on
  // every market tick.
  const series = useMemo(() => bars.data?.bars ?? [], [bars.data]);

  const summary = useMemo(() => {
    if (series.length < 2) return null;
    const first = series[0];
    const last = series[series.length - 1];
    if (!first || !last) return null;
    const open = Number.parseFloat(first.open);
    const close = Number.parseFloat(last.close);
    const highs = series.map((b) => Number.parseFloat(b.high));
    const lows = series.map((b) => Number.parseFloat(b.low));
    return {
      change: close - open,
      changePct: open > 0 ? (close - open) / open : 0,
      high: Math.max(...highs),
      low: Math.min(...lows),
      bars: series.length,
      from: first.open_time,
      to: last.open_time,
    };
  }, [series]);

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
        {bars.loading ? (
          <Empty>Loading bars…</Empty>
        ) : series.length === 0 ? (
          <Empty>
            No completed bars for {selected?.symbol} at {timeframe}.
          </Empty>
        ) : (
          <Candles bars={series} precision={selected?.price_precision ?? 2} />
        )}
      </Panel>

      <div className="grid cols-2">
        <Panel title="Range" flush>
          <div className="panel-body">
            {summary ? (
              <dl className="kv">
                <dt>Bars shown</dt>
                <dd>{summary.bars}</dd>
                <dt>From</dt>
                <dd>{new Date(summary.from).toLocaleString("en-GB")}</dd>
                <dt>To</dt>
                <dd>{new Date(summary.to).toLocaleString("en-GB")}</dd>
                <dt>High</dt>
                <dd>{summary.high.toFixed(selected?.price_precision ?? 2)}</dd>
                <dt>Low</dt>
                <dd>{summary.low.toFixed(selected?.price_precision ?? 2)}</dd>
                <dt>Change</dt>
                <dd className={summary.change >= 0 ? "profit" : "loss"}>
                  {summary.change >= 0 ? "+" : ""}
                  {summary.change.toFixed(selected?.price_precision ?? 2)} (
                  {(summary.changePct * 100).toFixed(2)}%)
                </dd>
              </dl>
            ) : (
              <Empty>Not enough history.</Empty>
            )}
            <p className="tiny muted" style={{ marginTop: 8, marginBottom: 0 }}>
              Only COMPLETED bars are shown and served. The in-progress bar is withheld
              because its close has not happened — a strategy reading it would be reading
              the future.
            </p>
          </div>
        </Panel>

        <Panel title="Recent bars" flush>
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>Time</th>
                  <th className="right">Open</th>
                  <th className="right">High</th>
                  <th className="right">Low</th>
                  <th className="right">Close</th>
                  <th className="right">Volume</th>
                </tr>
              </thead>
              <tbody>
                {[...series]
                  .reverse()
                  .slice(0, 60)
                  .map((bar) => {
                    const up =
                      Number.parseFloat(bar.close) >= Number.parseFloat(bar.open);
                    return (
                      <tr key={bar.open_time}>
                        <td className="mono muted">
                          {new Date(bar.open_time).toLocaleString("en-GB", {
                            day: "2-digit",
                            month: "short",
                            hour: "2-digit",
                            minute: "2-digit",
                          })}
                        </td>
                        <td className="mono right">
                          {decimal(bar.open, selected?.price_precision ?? 2)}
                        </td>
                        <td className="mono right">
                          {decimal(bar.high, selected?.price_precision ?? 2)}
                        </td>
                        <td className="mono right">
                          {decimal(bar.low, selected?.price_precision ?? 2)}
                        </td>
                        <td className={`mono right ${up ? "profit" : "loss"}`}>
                          {decimal(bar.close, selected?.price_precision ?? 2)}
                        </td>
                        <td className="mono right muted">{decimal(bar.volume, 0)}</td>
                      </tr>
                    );
                  })}
              </tbody>
            </table>
          </div>
        </Panel>
      </div>
    </>
  );
}

/**
 * Candlesticks drawn as inline SVG.
 *
 * Hand-drawn rather than pulled from a charting library: the whole renderer is
 * a loop over bars computing four coordinates each. It gives exact control over
 * the palette and the density, it adds no dependency to audit or update, and it
 * cannot pull in a licence that needs reviewing.
 */
function Candles({ bars, precision }: { bars: Bar[]; precision: number }) {
  const hostRef = useRef<HTMLDivElement | null>(null);
  const [width, setWidth] = useState(900);
  const [hover, setHover] = useState<number | null>(null);

  useEffect(() => {
    const element = hostRef.current;
    if (!element) return;
    const observer = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) setWidth(Math.max(320, entry.contentRect.width));
    });
    observer.observe(element);
    return () => observer.disconnect();
  }, []);

  const height = 400;
  const padding = { top: 12, right: 62, bottom: 22, left: 8 };
  const plotWidth = width - padding.left - padding.right;
  const plotHeight = height - padding.top - padding.bottom;

  const highs = bars.map((b) => Number.parseFloat(b.high));
  const lows = bars.map((b) => Number.parseFloat(b.low));
  const max = Math.max(...highs);
  const min = Math.min(...lows);
  const span = max - min || 1;
  // A little headroom so wicks do not touch the frame.
  const top = max + span * 0.04;
  const bottom = min - span * 0.04;
  const scale = (price: number) =>
    padding.top + ((top - price) / (top - bottom)) * plotHeight;

  const slot = plotWidth / bars.length;
  const bodyWidth = Math.max(1, Math.min(9, slot * 0.62));

  const gridLines = 5;
  const ticks = Array.from({ length: gridLines }, (_, index) => {
    const price = bottom + ((top - bottom) * index) / (gridLines - 1);
    return { price, y: scale(price) };
  });

  const active = hover !== null ? bars[hover] : undefined;

  return (
    <div ref={hostRef} style={{ position: "relative" }}>
      <svg
        width={width}
        height={height}
        role="img"
        aria-label="price chart"
        onMouseLeave={() => setHover(null)}
      >
        {ticks.map((tick) => (
          <g key={tick.price}>
            <line
              x1={padding.left}
              x2={padding.left + plotWidth}
              y1={tick.y}
              y2={tick.y}
              stroke="var(--border)"
              strokeWidth="1"
            />
            <text
              x={padding.left + plotWidth + 6}
              y={tick.y + 3.5}
              fill="var(--text-muted)"
              fontSize="10"
              fontFamily="var(--font-mono)"
            >
              {tick.price.toFixed(precision)}
            </text>
          </g>
        ))}

        {bars.map((bar, index) => {
          const open = Number.parseFloat(bar.open);
          const high = Number.parseFloat(bar.high);
          const low = Number.parseFloat(bar.low);
          const close = Number.parseFloat(bar.close);
          const x = padding.left + index * slot + slot / 2;
          const up = close >= open;
          const colour = up ? "var(--profit)" : "var(--loss)";
          const bodyTop = scale(Math.max(open, close));
          const bodyBottom = scale(Math.min(open, close));

          return (
            <g
              key={bar.open_time}
              onMouseEnter={() => setHover(index)}
              style={{ cursor: "crosshair" }}
            >
              {/* An invisible wide target makes hovering usable at this density. */}
              <rect
                x={x - slot / 2}
                y={padding.top}
                width={Math.max(slot, 2)}
                height={plotHeight}
                fill="transparent"
              />
              <line
                x1={x}
                x2={x}
                y1={scale(high)}
                y2={scale(low)}
                stroke={colour}
                strokeWidth="1"
              />
              <rect
                x={x - bodyWidth / 2}
                y={bodyTop}
                width={bodyWidth}
                height={Math.max(1, bodyBottom - bodyTop)}
                fill={colour}
              />
            </g>
          );
        })}

        {hover !== null ? (
          <line
            x1={padding.left + hover * slot + slot / 2}
            x2={padding.left + hover * slot + slot / 2}
            y1={padding.top}
            y2={padding.top + plotHeight}
            stroke="var(--border-focus)"
            strokeWidth="1"
            strokeDasharray="2 3"
          />
        ) : null}
      </svg>

      {active ? (
        <div
          style={{
            position: "absolute",
            top: 8,
            left: 12,
            background: "var(--bg-panel-raised)",
            border: "1px solid var(--border-strong)",
            borderRadius: 2,
            padding: "5px 9px",
            fontFamily: "var(--font-mono)",
            fontSize: 11,
            pointerEvents: "none",
          }}
        >
          {new Date(active.open_time).toLocaleString("en-GB")} · O{" "}
          {decimal(active.open, precision)} · H {decimal(active.high, precision)} · L{" "}
          {decimal(active.low, precision)} · C {decimal(active.close, precision)}
        </div>
      ) : null}

      <div
        className="tiny muted"
        style={{ padding: "0 12px 8px", display: "flex", justifyContent: "space-between" }}
      >
        <span>{new Date(bars[0]?.open_time ?? "").toLocaleString("en-GB")}</span>
        <span>
          {time(bars[bars.length - 1]?.open_time)} · completed bars only · simulated data
        </span>
      </div>
    </div>
  );
}
