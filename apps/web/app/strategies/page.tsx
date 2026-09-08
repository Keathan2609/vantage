"use client";

import { useState } from "react";

import { Empty, Notice, Panel, Status } from "@/components/ui";
import { ApiError, api, type StrategyRow } from "@/lib/api";
import { decimal, humanise, relative } from "@/lib/format";
import { useAsync, useVantage } from "@/lib/store";

export default function StrategiesPage() {
  const { account } = useVantage();
  const strategies = useAsync(() => api.strategies(), []);
  const [selected, setSelected] = useState<StrategyRow | null>(null);
  const [run, setRun] = useState<Awaited<ReturnType<typeof api.runStrategy>> | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const rows = strategies.data?.strategies ?? [];

  const evaluate = async (strategy: StrategyRow, dryRun: boolean) => {
    if (!account) return;
    const instrument = strategy.instruments?.[0];
    if (!instrument) {
      setError("This strategy version declares no instruments.");
      return;
    }
    setBusy(true);
    setError(null);
    setRun(null);
    try {
      const result = await api.runStrategy(strategy.id, {
        account_id: account.id,
        instrument_id: instrument,
        version: strategy.version,
        dry_run: dryRun,
      });
      setRun(result);
      setSelected(strategy);
    } catch (cause) {
      setError(
        cause instanceof ApiError ? `${cause.code}: ${cause.message}` : "Evaluation failed",
      );
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <Notice>
        A strategy produces a SIGNAL, never an order. The signal is an opinion with no
        authority and no size that risk must honour; the orchestrator decides whether it
        becomes an order intent, and that intent then faces every gate in the pipeline.
        {strategies.data?.note ? ` ${strategies.data.note}` : ""}
      </Notice>

      {error ? <Notice tone="bad">{error}</Notice> : null}

      <Panel
        title="Strategy library"
        note={`${rows.length} registered across ${new Set(rows.map((r) => r.family)).size} families`}
        flush
      >
        {strategies.loading ? (
          <Empty>Loading strategies…</Empty>
        ) : rows.length === 0 ? (
          <Empty>No strategies registered.</Empty>
        ) : (
          <div className="table-scroll tall">
            <table>
              <thead>
                <tr>
                  <th>Strategy</th>
                  <th>Family</th>
                  <th>Lifecycle</th>
                  <th>Enabled</th>
                  <th>Timeframe</th>
                  <th>Instruments</th>
                  <th className="right">Runs 7d</th>
                  <th className="right">Signals</th>
                  <th className="right">Failures</th>
                  <th className="right">Last run</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {rows.map((strategy) => (
                  <tr key={strategy.id}>
                    <td>
                      <button
                        onClick={() => setSelected(strategy)}
                        style={{
                          border: "none",
                          background: "none",
                          padding: 0,
                          color: "inherit",
                          cursor: "pointer",
                          textAlign: "left",
                        }}
                      >
                        {strategy.name}
                      </button>
                      {strategy.high_risk ? (
                        <span className="badge" data-tone="bad" style={{ marginLeft: 6 }}>
                          high risk
                        </span>
                      ) : null}
                    </td>
                    <td className="muted">{humanise(strategy.family)}</td>
                    <td>
                      <span
                        className="badge"
                        data-tone={strategy.lifecycle === "PAPER" ? "accent" : undefined}
                      >
                        {strategy.lifecycle}
                      </span>
                    </td>
                    <td>
                      <Status
                        value={strategy.enabled ? "enabled" : "disabled"}
                        tone={strategy.enabled ? "ok" : "warn"}
                      />
                    </td>
                    <td className="mono muted">{strategy.timeframe || "—"}</td>
                    <td className="mono muted">
                      {(strategy.instruments ?? []).join(", ") || "—"}
                    </td>
                    <td className="mono right">{strategy.runs_7d}</td>
                    <td className="mono right">{strategy.signals_7d}</td>
                    <td className={`mono right ${strategy.failures_7d > 0 ? "loss" : "muted"}`}>
                      {strategy.failures_7d}
                    </td>
                    <td className="mono right muted">
                      {strategy.last_run_at ? relative(strategy.last_run_at) : "never"}
                    </td>
                    <td className="right">
                      <button
                        disabled={busy || strategy.lifecycle !== "PAPER" || !strategy.enabled}
                        onClick={() => void evaluate(strategy, true)}
                        title={
                          strategy.lifecycle !== "PAPER"
                            ? "Only a PAPER version generates live signals"
                            : "Evaluate now without placing an order"
                        }
                      >
                        Evaluate
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>

      {selected ? (
        <div className="grid cols-2">
          <Panel
            title={selected.name}
            note={`${humanise(selected.family)} · version ${selected.version}`}
            actions={<button onClick={() => setSelected(null)}>Close</button>}
          >
            <p className="small" style={{ marginTop: 0 }}>
              {selected.description}
            </p>
            <dl className="kv">
              <dt>Key</dt>
              <dd>{selected.key}</dd>
              <dt>Lifecycle</dt>
              <dd>{selected.lifecycle}</dd>
              <dt>Enabled</dt>
              <dd>{selected.enabled ? "yes" : "no"}</dd>
              <dt>High risk</dt>
              <dd className={selected.high_risk ? "loss" : ""}>
                {selected.high_risk ? "yes — execution barred" : "no"}
              </dd>
              <dt>Timeframe</dt>
              <dd>{selected.timeframe || "—"}</dd>
              <dt>Instruments</dt>
              <dd>{(selected.instruments ?? []).join(", ") || "—"}</dd>
              <dt>Runs / signals (7d)</dt>
              <dd>
                {selected.runs_7d} / {selected.signals_7d}
              </dd>
            </dl>

            {selected.high_risk ? (
              <Notice tone="bad">
                This is a high-risk research strategy. It can be backtested so its failure
                mode can be measured, and it cannot be enabled for execution: the API refuses
                it and a database constraint refuses the row that would allow it.
              </Notice>
            ) : null}

            <div className="button-row" style={{ marginTop: 10 }}>
              <button
                disabled={busy || selected.lifecycle !== "PAPER" || !selected.enabled}
                onClick={() => void evaluate(selected, true)}
              >
                {busy ? "Evaluating…" : "Evaluate (signal only)"}
              </button>
              <button
                data-variant="primary"
                disabled={busy || selected.lifecycle !== "PAPER" || !selected.enabled}
                onClick={() => void evaluate(selected, false)}
                title="Evaluate and route an actionable signal into the order pipeline"
              >
                Evaluate and route
              </button>
            </div>
          </Panel>

          <Panel title="Last evaluation" flush>
            {!run ? (
              <Empty>Evaluate the strategy to see its reasoning.</Empty>
            ) : (
              <div className="panel-body">
                <div className="row-gap" style={{ marginBottom: 8 }}>
                  <span
                    className="badge"
                    data-tone={
                      run.action === "buy" ? "ok" : run.action === "sell" ? "bad" : undefined
                    }
                  >
                    {run.action.replace(/_/g, " ")}
                  </span>
                  <span className="badge">{run.status}</span>
                  <span className="mono small">confidence {decimal(run.confidence, 2)}</span>
                  {run.executed ? (
                    <span className="badge" data-tone="accent">
                      order placed
                    </span>
                  ) : null}
                </div>

                <p className="small wrap" style={{ marginTop: 0 }}>
                  {run.explanation || run.skip_reason}
                </p>

                {run.skip_reason ? (
                  <Notice tone="warn">Skipped: {run.skip_reason}</Notice>
                ) : null}

                {run.rejection ? (
                  <Notice tone="bad">
                    <strong>{humanise(run.rejection.code)}</strong> — {run.rejection.message}
                  </Notice>
                ) : null}

                {run.order ? (
                  <dl className="kv">
                    <dt>Order</dt>
                    <dd>{run.order.id.slice(0, 8)}</dd>
                    <dt>Status</dt>
                    <dd>{run.order.status}</dd>
                    <dt>Quantity</dt>
                    <dd>{run.order.quantity}</dd>
                    <dt>Filled</dt>
                    <dd>
                      {run.order.filled_quantity} @ {decimal(run.order.avg_fill_price, 2)}
                    </dd>
                  </dl>
                ) : null}

                {run.indicators && Object.keys(run.indicators).length > 0 ? (
                  <>
                    <div className="panel-title" style={{ margin: "12px 0 5px" }}>
                      Indicator readings at decision time
                    </div>
                    <table>
                      <tbody>
                        {Object.entries(run.indicators).map(([name, value]) => (
                          <tr key={name}>
                            <td className="muted">{humanise(name)}</td>
                            <td className="mono right">{value}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </>
                ) : null}

                <p className="tiny muted" style={{ marginTop: 10, marginBottom: 0 }}>
                  Every evaluation is recorded with its inputs as an immutable decision
                  snapshot, so a refusal can be examined months later rather than argued
                  about.
                </p>
              </div>
            )}
          </Panel>
        </div>
      ) : null}

      <Panel title="Promotion" flush>
        <div className="panel-body">
          <p className="small" style={{ marginTop: 0 }}>
            Lifecycle advances one stage at a time and each step needs evidence:
          </p>
          <table>
            <thead>
              <tr>
                <th>Stage</th>
                <th>Requires</th>
              </tr>
            </thead>
            <tbody>
              <tr>
                <td>BACKTESTED</td>
                <td className="muted">a stored backtest for that exact version</td>
              </tr>
              <tr>
                <td>VALIDATED</td>
                <td className="muted">
                  an out-of-sample or walk-forward result — in-sample is not evidence of an
                  edge
                </td>
              </tr>
              <tr>
                <td>PAPER</td>
                <td className="muted">promotion from VALIDATED; this build&rsquo;s ceiling</td>
              </tr>
              <tr>
                <td className="muted">DEMO, LIVE</td>
                <td className="loss">
                  unreachable — refused by the API, the domain, and a database constraint
                </td>
              </tr>
            </tbody>
          </table>
          <p className="tiny muted" style={{ marginBottom: 0, marginTop: 8 }}>
            Nothing self-promotes. A strategy cannot advance its own lifecycle, and neither
            can a model.
          </p>
        </div>
      </Panel>
    </>
  );
}
