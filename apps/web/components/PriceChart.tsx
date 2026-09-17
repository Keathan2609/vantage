"use client";

import {
  CandlestickSeries,
  ColorType,
  createChart,
  createSeriesMarkers,
  CrosshairMode,
  type CandlestickData,
  type IChartApi,
  type ISeriesApi,
  type ISeriesMarkersPluginApi,
  type SeriesMarker,
  type Time,
  type UTCTimestamp,
} from "lightweight-charts";
import { useEffect, useMemo, useRef } from "react";

import type { Bar } from "@/lib/api";

/**
 * Candles, rendered by TradingView Lightweight Charts over VANTAGE's data.
 *
 * TradingView is the renderer and nothing else. Every bar here came from
 * Vantage's own store through Vantage's own API; the library is never asked
 * where the market is, and the browser never talks to a data provider. That
 * separation is the point of the whole market-data layer behind this
 * component — if the chart fetched its own prices, the platform would be
 * showing one thing and trading on another.
 *
 * # Why the chart is built once and fed imperatively
 *
 * Lightweight Charts owns a canvas and its own internal state. Re-creating it
 * whenever React re-renders would throw away the user's zoom and pan on every
 * market tick, which is the single most irritating thing a trading chart can
 * do. So the chart instance lives in a ref for the lifetime of the mount, and
 * data arrives through `setData`/`update` — the same way the library expects.
 */

export interface ChartMarker {
  time: string;
  side: "buy" | "sell" | "flat";
  kind: "decision" | "order" | "fill";
  text: string;
}

export interface PriceChartProps {
  bars: Bar[];
  markers?: ChartMarker[];
  /** Called when the user pans past the oldest loaded bar. */
  onNeedOlderBars?: (oldest: string) => void;
  height?: number;
  /** Rendered over the canvas when there is nothing to draw. */
  emptyMessage?: string;
}

/** Terminal palette. Read from CSS variables so the chart cannot drift from
 *  the rest of the application's theme. */
function palette() {
  if (typeof window === "undefined") {
    return {
      text: "#c9d1d9",
      grid: "#21262d",
      up: "#3fb950",
      down: "#f85149",
      border: "#30363d",
    };
  }
  const style = getComputedStyle(document.documentElement);
  const read = (name: string, fallback: string) =>
    style.getPropertyValue(name).trim() || fallback;
  return {
    text: read("--text-muted", "#c9d1d9"),
    grid: read("--border-subtle", "#21262d"),
    up: read("--positive", "#3fb950"),
    down: read("--negative", "#f85149"),
    border: read("--border", "#30363d"),
  };
}

/**
 * Bars arrive as decimal STRINGS and must become numbers for the canvas.
 *
 * This is the one place a price is allowed to become a float, and it is
 * allowed because a pixel coordinate is presentational and never returns to
 * the server. Nothing derived from these numbers is submitted, stored or
 * compared — see the money conventions in CLAUDE.md.
 */
function toCandles(bars: Bar[]): CandlestickData<Time>[] {
  const out: CandlestickData<Time>[] = [];
  let previous = -Infinity;
  for (const bar of bars) {
    const seconds = Math.floor(Date.parse(bar.open_time) / 1000);
    if (!Number.isFinite(seconds)) continue;
    // Lightweight Charts requires strictly ascending, de-duplicated times and
    // throws otherwise. The API returns ascending bars, but a merge of a
    // backward page with the existing series can overlap at the seam, and a
    // thrown exception inside the canvas takes the whole page down.
    if (seconds <= previous) continue;
    previous = seconds;
    out.push({
      time: seconds as UTCTimestamp,
      open: Number(bar.open),
      high: Number(bar.high),
      low: Number(bar.low),
      close: Number(bar.close),
    });
  }
  return out;
}

export function PriceChart({
  bars,
  markers = [],
  onNeedOlderBars,
  height = 460,
  emptyMessage = "No bars for this instrument and timeframe yet.",
}: PriceChartProps) {
  const container = useRef<HTMLDivElement | null>(null);
  const chart = useRef<IChartApi | null>(null);
  const series = useRef<ISeriesApi<"Candlestick"> | null>(null);
  const markerApi = useRef<ISeriesMarkersPluginApi<Time> | null>(null);
  // The pagination callback and the current bars are held in refs so the
  // chart's scroll subscription always sees the latest values without being
  // torn down and rebuilt -- which would reset the user's zoom on every tick.
  // Written in an effect rather than during render: a ref mutation during
  // render is not guaranteed to have happened by the time anything reads it.
  const needOlder = useRef(onNeedOlderBars);
  const latestBars = useRef<Bar[]>(bars);
  const requestedBefore = useRef<string | null>(null);

  useEffect(() => {
    needOlder.current = onNeedOlderBars;
    latestBars.current = bars;
  }, [onNeedOlderBars, bars]);

  const candles = useMemo(() => toCandles(bars), [bars]);

  // --- create once ----------------------------------------------------------
  useEffect(() => {
    if (!container.current) return;
    const colors = palette();

    const instance = createChart(container.current, {
      layout: {
        background: { type: ColorType.Solid, color: "transparent" },
        textColor: colors.text,
        fontFamily:
          "var(--font-mono, ui-monospace, SFMono-Regular, Menlo, monospace)",
      },
      grid: {
        vertLines: { color: colors.grid },
        horzLines: { color: colors.grid },
      },
      crosshair: { mode: CrosshairMode.Normal },
      rightPriceScale: { borderColor: colors.border },
      timeScale: {
        borderColor: colors.border,
        timeVisible: true,
        secondsVisible: false,
      },
      autoSize: true,
    });

    const candleSeries = instance.addSeries(CandlestickSeries, {
      upColor: colors.up,
      downColor: colors.down,
      borderUpColor: colors.up,
      borderDownColor: colors.down,
      wickUpColor: colors.up,
      wickDownColor: colors.down,
    });

    chart.current = instance;
    series.current = candleSeries;
    markerApi.current = createSeriesMarkers(candleSeries, []);

    // Backward pagination. The library reports how many bars sit to the left
    // of the visible range; going negative means the user has panned past the
    // oldest bar loaded, which is the moment to ask for more.
    const onRangeChange = () => {
      const logical = instance.timeScale().getVisibleLogicalRange();
      if (!logical || !needOlder.current) return;
      if (logical.from > 10) return;
      const oldest = latestBars.current[0]?.open_time;
      // Guarded so one pan does not fire a request per frame, and so the same
      // cursor is never requested twice.
      if (!oldest || requestedBefore.current === oldest) return;
      requestedBefore.current = oldest;
      needOlder.current(oldest);
    };
    instance.timeScale().subscribeVisibleLogicalRangeChange(onRangeChange);

    return () => {
      instance.timeScale().unsubscribeVisibleLogicalRangeChange(onRangeChange);
      instance.remove();
      chart.current = null;
      series.current = null;
      markerApi.current = null;
    };
    // Deliberately empty: the chart is created once per mount, and the
    // handler reads current values through refs. Re-creating it when data
    // changes would discard the user's zoom and pan on every market tick.
  }, []);

  // --- feed data ------------------------------------------------------------
  useEffect(() => {
    if (!series.current || candles.length === 0) return;
    series.current.setData(candles);
    // A new cursor may now be requestable.
    requestedBefore.current = null;
  }, [candles]);

  // --- markers --------------------------------------------------------------
  useEffect(() => {
    if (!markerApi.current) return;
    const colors = palette();
    const mapped: SeriesMarker<Time>[] = [];
    for (const m of markers) {
      const seconds = Math.floor(Date.parse(m.time) / 1000);
      if (!Number.isFinite(seconds)) continue;
      const buy = m.side === "buy";
      const flat = m.side === "flat";
      mapped.push({
        time: seconds as UTCTimestamp,
        position: buy ? "belowBar" : "aboveBar",
        // A NO TRADE decision is drawn as a neutral dot rather than an arrow:
        // it is a real and common outcome, and drawing it like an entry would
        // make refusals look like trades.
        shape: flat ? "circle" : buy ? "arrowUp" : "arrowDown",
        color: flat ? colors.text : buy ? colors.up : colors.down,
        text: m.text,
        size: m.kind === "fill" ? 2 : 1,
      });
    }
    // Lightweight Charts requires markers in ascending time order.
    mapped.sort((a, b) => (a.time as number) - (b.time as number));
    markerApi.current.setMarkers(mapped);
  }, [markers]);

  return (
    <div className="chart-canvas-wrap" style={{ height }}>
      <div ref={container} className="chart-canvas" />
      {candles.length === 0 && (
        <div className="chart-canvas-empty">{emptyMessage}</div>
      )}
    </div>
  );
}
