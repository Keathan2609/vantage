"use client";

import { useState } from "react";

import { Empty, Notice, Panel, Status } from "@/components/ui";
import { api } from "@/lib/api";
import { decimal, percent } from "@/lib/format";
import { useAsync } from "@/lib/store";

export default function ScannerPage() {
  const [timeframe, setTimeframe] = useState("1h");
  const scan = useAsync(() => api.scanner(timeframe), [timeframe]);

  const candidates = scan.data?.candidates ?? [];

  return (
    <>
      <Notice>
        The scanner ranks opportunities; it places nothing. Anything acted on here goes
        through the ordinary order pipeline with every gate applied. Cost is subtracted from
        the score, so a wide spread pushes an instrument down the list even when its trend
        looks attractive, and event risk caps it outright.
      </Notice>

      <Panel
        title="Opportunity scan"
        note={scan.data ? `${candidates.length} instrument(s) evaluated` : undefined}
        actions={
          <>
            <div className="segmented">
              {["15m", "1h", "4h"].map((tf) => (
                <button
                  key={tf}
                  data-selected={tf === timeframe}
                  onClick={() => setTimeframe(tf)}
                >
                  {tf}
                </button>
              ))}
            </div>
            <button onClick={scan.reload} disabled={scan.loading}>
              {scan.loading ? "Scanning…" : "Rescan"}
            </button>
          </>
        }
        flush
      >
        {scan.loading ? (
          <Empty>Scanning the permitted universe…</Empty>
        ) : scan.error ? (
          <Notice tone="bad">{scan.error}</Notice>
        ) : candidates.length === 0 ? (
          <Empty>
            No candidates. The scanner needs at least 50 completed bars per instrument.
          </Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th className="right">Score</th>
                <th>Instrument</th>
                <th>Regime</th>
                <th className="right">Trend</th>
                <th className="right">Momentum</th>
                <th className="right">Volatility</th>
                <th className="right">Spread</th>
                <th>Event risk</th>
                <th>Agreeing strategies</th>
              </tr>
            </thead>
            <tbody>
              {candidates.map((candidate) => (
                <tr key={candidate.instrument_id} title={candidate.explanation}>
                  <td className="mono right accent">{decimal(candidate.score, 3)}</td>
                  <td>{candidate.symbol}</td>
                  <td>
                    <span className="badge">{candidate.regime.replace(/_/g, " ")}</span>
                  </td>
                  <td
                    className={`mono right ${
                      candidate.trend_score.startsWith("-") ? "loss" : "profit"
                    }`}
                  >
                    {decimal(candidate.trend_score, 2)}
                  </td>
                  <td
                    className={`mono right ${
                      candidate.momentum_score.startsWith("-") ? "loss" : "profit"
                    }`}
                  >
                    {decimal(candidate.momentum_score, 2)}
                  </td>
                  <td className="mono right muted">{percent(candidate.volatility, 3)}</td>
                  <td className="mono right muted">
                    {percent(candidate.spread_fraction, 4)}
                  </td>
                  <td>
                    <Status
                      value={candidate.event_risk}
                      tone={
                        candidate.event_risk === "none"
                          ? "ok"
                          : candidate.event_risk === "high"
                            ? "bad"
                            : "warn"
                      }
                    />
                  </td>
                  <td className="small muted">
                    {candidate.agreeing_strategies.length === 0
                      ? "none"
                      : candidate.agreeing_strategies.join(", ")}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>

      {candidates.length > 0 ? (
        <Panel title="Reasoning" note="why each candidate scored as it did" flush>
          <table>
            <thead>
              <tr>
                <th>Instrument</th>
                <th>Explanation</th>
              </tr>
            </thead>
            <tbody>
              {candidates.map((candidate) => (
                <tr key={candidate.instrument_id}>
                  <td>{candidate.symbol}</td>
                  <td className="wrap small muted">{candidate.explanation}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Panel>
      ) : null}

      <Panel title="Autopilot" flush>
        <div className="panel-body">
          <p className="small" style={{ marginTop: 0 }}>
            Autopilot — letting Vantage choose among permitted markets rather than requiring
            an instrument to be picked by hand — is designed but not enabled in this build.
            The pipeline it would use already exists and is exercised by the scheduler:
          </p>
          <p className="small mono muted" style={{ marginBottom: 8 }}>
            universe → data quality → scanner → eligible strategies → signal aggregation →
            portfolio context → event risk → risk budget → trading authority → order intent →
            order pipeline
          </p>
          <p className="small muted" style={{ marginBottom: 0 }}>
            The constraint that makes it safe is that Autopilot cannot widen its own limits:
            it produces order intents, and every intent faces the same risk engine, authority
            check and kill switches as a manual order. Enabling it is a scheduling change,
            not a new execution path.
          </p>
        </div>
      </Panel>
    </>
  );
}
