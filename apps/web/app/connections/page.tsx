"use client";

/**
 * Connections: what Vantage is talking to, and in which mode.
 *
 * The page exists to make one thing impossible to be wrong about — which
 * broker adapter is loaded. A trading system whose operator has to guess
 * whether orders are simulated has a much worse problem than a missing page.
 *
 * No credential, endpoint secret or token is displayed here, and the API does
 * not return one. The adapter reports its name, mode and health; nothing else
 * about a connection is exposed to the browser.
 */

import { Empty, Notice, Panel, Status } from "@/components/ui";
import { api } from "@/lib/api";
import { age, fullDateTime, humanise, relative } from "@/lib/format";
import { useAsync, useVantage } from "@/lib/store";

export default function ConnectionsPage() {
  const { version, quotes, health, marketStatus, account } = useVantage();
  const connections = useAsync(() => api.connections(), []);
  const instruments = useAsync(() => api.instruments(false), []);

  const rows = connections.data?.connections ?? [];
  const healthRows = Object.values(health);
  const degraded = healthRows.filter((row) => !row.tradable_by_automation);

  return (
    <>
      <Notice tone={version?.live_trading_available ? "bad" : "ok"}>
        {version?.live_trading_available ? (
          <>
            <strong>This build reports that live execution is available.</strong> That should
            be impossible here — treat it as a defect and stop trading until it is explained.
          </>
        ) : (
          <>
            <strong>Execution mode: {version?.execution_mode ?? "unknown"}.</strong> The live
            broker adapter is not compiled into this binary, the configuration loader refuses
            any mode other than paper, and the database refuses a non-paper account row. No
            real-money venue is reachable from this process.
          </>
        )}
      </Notice>

      {degraded.length > 0 ? (
        <Notice tone="warn">
          {degraded.length} instrument feed(s) are not fit for automation:{" "}
          {degraded.map((row) => row.symbol).join(", ")}. Automated orders on those
          instruments are refused; the data is still shown, marked, rather than hidden.
        </Notice>
      ) : null}

      <div className="grid sidebar-right">
        <div>
          <Panel
            title="Connections"
            note={`${rows.length} adapter(s)`}
            actions={<button onClick={connections.reload}>Refresh</button>}
            flush
          >
            {connections.loading ? (
              <Empty>Checking connections…</Empty>
            ) : rows.length === 0 ? (
              <Empty>No connections reported.</Empty>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>Component</th>
                    <th>Kind</th>
                    <th>Mode</th>
                    <th>Status</th>
                    <th className="right">Latency</th>
                    <th>Detail</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((connection) => (
                    <tr key={`${connection.kind}-${connection.name}`}>
                      <td>{connection.name}</td>
                      <td className="muted">{humanise(connection.kind)}</td>
                      <td>
                        <span
                          className="badge"
                          data-tone={connection.mode === "paper" ? "accent" : undefined}
                        >
                          {connection.mode}
                        </span>
                      </td>
                      <td>
                        <Status value={connection.status} />
                      </td>
                      <td className="mono right muted">
                        {connection.latency_ms === undefined
                          ? "—"
                          : `${connection.latency_ms} ms`}
                      </td>
                      <td className="wrap muted small">{connection.detail ?? "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Panel>

          <Panel
            title="Market data health"
            note="per instrument, as the risk engine sees it"
            flush
          >
            {healthRows.length === 0 ? (
              <Empty>No feed health reported yet.</Empty>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>Instrument</th>
                    <th>State</th>
                    <th className="right">Quote age</th>
                    <th>Provider</th>
                    <th>Automation</th>
                    <th>Issues</th>
                  </tr>
                </thead>
                <tbody>
                  {healthRows.map((row) => {
                    const quote = quotes[row.instrument_id];
                    return (
                      <tr key={row.instrument_id}>
                        <td>{row.symbol}</td>
                        <td>
                          <Status value={row.state} />
                        </td>
                        <td className="mono right muted">
                          {quote ? age(quote.age_seconds) : `${row.quote_age_ms} ms`}
                        </td>
                        <td className="muted">{row.provider}</td>
                        <td>
                          <Status
                            value={row.tradable_by_automation ? "permitted" : "refused"}
                            tone={row.tradable_by_automation ? "ok" : "bad"}
                          />
                        </td>
                        <td className="wrap muted small">
                          {(row.issues ?? []).join("; ") || "none"}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            )}
          </Panel>

          <Panel
            title="Instrument universe"
            note={`${(instruments.data?.instruments ?? []).length} instrument(s) defined`}
            flush
          >
            {instruments.loading ? (
              <Empty>Loading…</Empty>
            ) : (
              <div className="table-scroll">
                <table>
                  <thead>
                    <tr>
                      <th>Symbol</th>
                      <th>Name</th>
                      <th>Class</th>
                      <th>Quote</th>
                      <th className="right">Contract</th>
                      <th className="right">Min qty</th>
                      <th className="right">Step</th>
                      <th className="right">Margin</th>
                      <th>Enabled</th>
                    </tr>
                  </thead>
                  <tbody>
                    {(instruments.data?.instruments ?? []).map((instrument) => (
                      <tr key={instrument.id}>
                        <td>{instrument.symbol}</td>
                        <td className="muted">{instrument.name}</td>
                        <td className="muted">{humanise(instrument.asset_class)}</td>
                        <td className="mono">{instrument.quote_currency}</td>
                        <td className="mono right muted">{instrument.contract_size}</td>
                        <td className="mono right muted">{instrument.min_quantity}</td>
                        <td className="mono right muted">{instrument.quantity_step}</td>
                        <td className="mono right muted">{instrument.margin_rate}</td>
                        <td>
                          <Status
                            value={instrument.enabled ? "enabled" : "disabled"}
                            tone={instrument.enabled ? "ok" : "warn"}
                          />
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Panel>
        </div>

        <div>
          <Panel title="Build" flush>
            <div className="panel-body">
              <dl className="kv">
                <dt>Version</dt>
                <dd>{version?.version ?? "—"}</dd>
                <dt>Commit</dt>
                <dd className="tiny">{version?.commit ?? "—"}</dd>
                <dt>Execution mode</dt>
                <dd>{version?.execution_mode ?? "—"}</dd>
                <dt>Simulated funds</dt>
                <dd className={version?.simulated_funds ? "profit" : "loss"}>
                  {version?.simulated_funds ? "yes" : "no"}
                </dd>
                <dt>Live execution</dt>
                <dd className={version?.live_trading_available ? "loss" : "profit"}>
                  {version?.live_trading_available ? "AVAILABLE" : "not available"}
                </dd>
                <dt>Account broker</dt>
                <dd>{account?.broker_name ?? "—"}</dd>
              </dl>
            </div>
          </Panel>

          <Panel title="Session clock" flush>
            <div className="panel-body">
              <dl className="kv">
                <dt>Market status</dt>
                <dd>
                  <Status
                    value={marketStatus?.status}
                    tone={marketStatus?.tradable ? "ok" : "warn"}
                  />
                </dd>
                <dt>Active sessions</dt>
                <dd>{(marketStatus?.active_sessions ?? []).join(", ") || "none"}</dd>
                <dt>Server time</dt>
                <dd className="mono">{fullDateTime(marketStatus?.server_time)}</dd>
                <dt>Next close</dt>
                <dd>
                  {marketStatus?.next_close ? relative(marketStatus.next_close) : "—"}
                </dd>
              </dl>
              <p className="tiny muted" style={{ marginBottom: 0 }}>
                Session boundaries are resolved in New York local time, so the weekly open and
                close move correctly across daylight-saving changes rather than drifting by an
                hour twice a year.
              </p>
            </div>
          </Panel>

          <Panel title="What is not here" flush>
            <div className="panel-body">
              <p className="small muted" style={{ marginTop: 0 }}>
                No credential, API key, account number or endpoint secret is returned by this
                API or rendered by this page. Broker credentials are held encrypted, are
                readable only by the control plane, and are excluded from logs, error
                responses, decision snapshots and audit metadata by construction — the
                response types have no field for them.
              </p>
              <p className="small muted" style={{ marginBottom: 0 }}>
                The quant service reaches the market data it needs through a read-only
                database role and has no broker client at all. There is no code path by which
                Python can place an order.
              </p>
            </div>
          </Panel>
        </div>
      </div>
    </>
  );
}
