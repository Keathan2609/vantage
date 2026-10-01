"use client";

import { useState } from "react";

import { Empty, Notice, Panel } from "@/components/ui";
import { ApiError, api } from "@/lib/api";
import { dateTime, humanise, percentValue } from "@/lib/format";
import { useAsync } from "@/lib/store";

function metric(metrics: Record<string, unknown>, key: string): string {
  const value = metrics[key];
  if (value === undefined || value === null) return "-";
  return typeof value === "number" ? String(value) : String(value);
}

export default function BacktestsPage() {
  const strategies = useAsync(() => api.strategies(), []);
  const backtests = useAsync(() => api.backtests(), []);

  const [strategyId, setStrategyId] = useState("");
  const [days, setDays] = useState(30);
  const [sampleKind, setSampleKind] = useState("out_of_sample");
  const [capital, setCapital] = useState("500");
  const [risk, setRisk] = useState("0.01");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<Awaited<ReturnType<typeof api.runBacktest>> | null>(
    null,
  );
  const [selected, setSelected] = useState<string | null>(null);

  const eligible = (strategies.data?.strategies ?? []).filter(
    (s) => (s.instruments ?? []).length > 0,
  );
  const strategy = eligible.find((s) => s.id === strategyId) ?? eligible[0];

  const runBacktest = async () => {
    if (!strategy) return;
    const instrument = strategy.instruments?.[0];
    if (!instrument) return;
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      const to = new Date();
      const from = new Date(to.getTime() - days * 24 * 3600 * 1000);
      const response = await api.runBacktest({
        strategy_id: strategy.id,
        version: strategy.version,
        instrument_id: instrument,
        timeframe: strategy.timeframe || "1h",
        from: from.toISOString(),
        to: to.toISOString(),
        initial_capital: capital,
        currency: "ZAR",
        sample_kind: sampleKind,
        risk_per_trade: risk,
      });
      setResult(response);
      backtests.reload();
    } catch (cause) {
      setError(
        cause instanceof ApiError ? `${cause.code}: ${cause.message}` : "The backtest failed",
      );
    } finally {
      setBusy(false);
    }
  };

  const rows = backtests.data?.backtests ?? [];
  const focused = rows.find((b) => b.ID === selected);

  return (
    <>
      <Notice>
        Entries fill on the bar AFTER the signal, crossing the spread, with slippage,
        commission and overnight financing applied. When a bar covers both the stop and the
        target, the STOP is assumed, because bar data cannot say which came first, and the
        pessimistic reading is the only safe one.
      </Notice>

      <div className="grid sidebar-right">
        <div>
          <Panel
            title="Stored runs"
            note={`${rows.length} result(s)`}
            actions={<button onClick={backtests.reload}>Refresh</button>}
            flush
          >
            {backtests.loading ? (
              <Empty>Loading…</Empty>
            ) : rows.length === 0 ? (
              <Empty>No backtests yet. Run one from the panel on the right.</Empty>
            ) : (
              <div className="table-scroll tall">
                <table>
                  <thead>
                    <tr>
                      <th>Ran</th>
                      <th>Instrument</th>
                      <th>Sample</th>
                      <th className="right">Trades</th>
                      <th className="right">Return</th>
                      <th className="right">Max DD</th>
                      <th className="right">Expectancy</th>
                      <th className="right">Profit factor</th>
                      <th className="right">Warnings</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((backtest) => (
                      <tr
                        key={backtest.ID}
                        onClick={() => setSelected(backtest.ID)}
                        style={{ cursor: "pointer" }}
                      >
                        <td className="mono muted">{dateTime(backtest.StartedAt)}</td>
                        <td>{backtest.InstrumentID}</td>
                        <td>
                          <span
                            className="badge"
                            data-tone={
                              backtest.SampleKind === "in_sample" ? "warn" : undefined
                            }
                          >
                            {humanise(backtest.SampleKind)}
                          </span>
                        </td>
                        <td className="mono right">{metric(backtest.Metrics, "trades")}</td>
                        <td
                          className={`mono right ${
                            String(backtest.Metrics.total_return ?? "").startsWith("-")
                              ? "loss"
                              : "profit"
                          }`}
                        >
                          {percentValue(metric(backtest.Metrics, "total_return_pct"))}
                        </td>
                        <td className="mono right loss">
                          {percentValue(metric(backtest.Metrics, "max_drawdown_pct"))}
                        </td>
                        <td className="mono right">
                          {metric(backtest.Metrics, "expectancy")}
                        </td>
                        <td className="mono right">
                          {metric(backtest.Metrics, "profit_factor")}
                        </td>
                        <td className="mono right muted">
                          {(backtest.Warnings ?? []).length}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Panel>

          {focused ? (
            <Panel
              title="Run detail"
              note={`dataset ${focused.DatasetHash.slice(0, 12)}`}
              actions={<button onClick={() => setSelected(null)}>Close</button>}
            >
              <div className="grid cols-2">
                <div>
                  <div className="panel-title" style={{ marginBottom: 6 }}>
                    Metrics
                  </div>
                  <table>
                    <tbody>
                      {Object.entries(focused.Metrics)
                        .filter(([, value]) => typeof value !== "object")
                        .map(([key, value]) => (
                          <tr key={key}>
                            <td className="muted">{humanise(key)}</td>
                            <td className="mono right">{String(value)}</td>
                          </tr>
                        ))}
                    </tbody>
                  </table>
                </div>
                <div>
                  <div className="panel-title" style={{ marginBottom: 6 }}>
                    Provenance
                  </div>
                  <dl className="kv">
                    <dt>Period</dt>
                    <dd>
                      {dateTime(focused.PeriodStart)} → {dateTime(focused.PeriodEnd)}
                    </dd>
                    <dt>Sample kind</dt>
                    <dd>{humanise(focused.SampleKind)}</dd>
                    <dt>Initial capital</dt>
                    <dd>
                      {focused.InitialCapital} {focused.Currency}
                    </dd>
                    <dt>Frictionless</dt>
                    <dd className={focused.Frictionless ? "loss" : ""}>
                      {focused.Frictionless ? "yes, unrealistic" : "no, costs applied"}
                    </dd>
                    <dt>Dataset hash</dt>
                    <dd className="tiny">{focused.DatasetHash.slice(0, 24)}</dd>
                  </dl>

                  {(focused.Warnings ?? []).length > 0 ? (
                    <>
                      <div className="panel-title" style={{ margin: "12px 0 5px" }}>
                        Methodology warnings
                      </div>
                      {(focused.Warnings ?? []).map((warning, index) => (
                        <Notice key={index} tone="warn">
                          {warning}
                        </Notice>
                      ))}
                    </>
                  ) : null}
                </div>
              </div>
            </Panel>
          ) : null}
        </div>

        <div>
          <Panel title="Run a backtest">
            {error ? <Notice tone="bad">{error}</Notice> : null}

            <div className="field">
              <label htmlFor="strategy">Strategy</label>
              <select
                id="strategy"
                value={strategy?.id ?? ""}
                onChange={(event) => setStrategyId(event.target.value)}
              >
                {eligible.map((option) => (
                  <option key={option.id} value={option.id}>
                    {option.name} (v{option.version})
                  </option>
                ))}
              </select>
            </div>

            <div className="field-row">
              <div className="field">
                <label htmlFor="days">History (days)</label>
                <select
                  id="days"
                  value={days}
                  onChange={(event) => setDays(Number(event.target.value))}
                >
                  <option value={7}>7</option>
                  <option value={14}>14</option>
                  <option value={30}>30</option>
                </select>
              </div>
              <div className="field">
                <label htmlFor="sample">Sample kind</label>
                <select
                  id="sample"
                  value={sampleKind}
                  onChange={(event) => setSampleKind(event.target.value)}
                >
                  <option value="out_of_sample">Out of sample</option>
                  <option value="in_sample">In sample</option>
                  <option value="validation">Validation</option>
                  <option value="walk_forward">Walk forward</option>
                </select>
              </div>
            </div>

            <div className="field-row">
              <div className="field">
                <label htmlFor="capital">Initial capital (ZAR)</label>
                <input
                  id="capital"
                  value={capital}
                  onChange={(event) => setCapital(event.target.value)}
                />
              </div>
              <div className="field">
                <label htmlFor="risk">Risk per trade</label>
                <input
                  id="risk"
                  value={risk}
                  onChange={(event) => setRisk(event.target.value)}
                />
              </div>
            </div>

            <button
              data-variant="primary"
              onClick={() => void runBacktest()}
              disabled={busy || !strategy}
              style={{ width: "100%" }}
            >
              {busy ? "Running…" : "Run backtest"}
            </button>

            {sampleKind === "in_sample" ? (
              <Notice tone="warn">
                An in-sample result measures fit to the data a strategy was developed on. It
                is not evidence of a forward edge, and the stored result is labelled as such.
              </Notice>
            ) : null}
          </Panel>

          {result ? (
            <Panel title="Latest result" note={humanise(result.sample_kind)} flush>
              <div className="panel-body">
                <dl className="kv">
                  <dt>Trades</dt>
                  <dd>{result.trades}</dd>
                  <dt>Return</dt>
                  <dd
                    className={
                      String(result.metrics.total_return ?? "").startsWith("-")
                        ? "loss"
                        : "profit"
                    }
                  >
                    {percentValue(String(result.metrics.total_return_pct ?? ""))}
                  </dd>
                  <dt>Max drawdown</dt>
                  <dd className="loss">
                    {percentValue(String(result.metrics.max_drawdown_pct ?? ""))}
                  </dd>
                  <dt>Expectancy</dt>
                  <dd>{String(result.metrics.expectancy ?? "-")}</dd>
                  <dt>Win rate</dt>
                  <dd>{String(result.metrics.win_rate ?? "-")}</dd>
                  <dt>Profit factor</dt>
                  <dd>{String(result.metrics.profit_factor ?? "-")}</dd>
                  <dt>Costs</dt>
                  <dd>
                    {String(result.metrics.total_commission ?? 0)} +{" "}
                    {String(result.metrics.total_slippage ?? 0)} slippage
                  </dd>
                </dl>

                {result.warnings.map((warning, index) => (
                  <Notice key={index} tone="warn">
                    {warning}
                  </Notice>
                ))}

                <p className="tiny muted" style={{ marginBottom: 0 }}>
                  {result.note}
                </p>
              </div>
            </Panel>
          ) : null}

          <Panel title="What is measured" flush>
            <div className="panel-body">
              <p className="small muted" style={{ marginTop: 0 }}>
                Win rate is reported but is never the headline. A strategy can win 90% of the
                time and lose money, and optimising for win rate reliably produces exactly
                that. Expectancy, profit factor and maximum drawdown decide whether an
                approach is viable.
              </p>
              <p className="small muted" style={{ marginBottom: 0 }}>
                Signals the account could not afford are counted separately rather than
                dropped, so a result at R500 does not silently describe a larger account.
              </p>
            </div>
          </Panel>
        </div>
      </div>
    </>
  );
}
