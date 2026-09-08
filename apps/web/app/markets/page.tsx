"use client";

import Link from "next/link";

import { Empty, Panel, Status } from "@/components/ui";
import { api } from "@/lib/api";
import { age, decimal, percent, time } from "@/lib/format";
import { useAsync, useVantage } from "@/lib/store";

export default function MarketsPage() {
  const { quotes, health, marketStatus } = useVantage();
  const instruments = useAsync(() => api.instruments(false), []);

  const rows = (instruments.data?.instruments ?? []).map((instrument) => ({
    instrument,
    quote: quotes[instrument.id],
    feed: health[instrument.id],
  }));

  return (
    <>
      <Panel
        title="Market status"
        note={marketStatus ? `server time ${time(marketStatus.server_time)}` : undefined}
        flush
      >
        <div className="panel-body">
          <div className="row-gap">
            <Status
              value={marketStatus?.status}
              label={(marketStatus?.status ?? "unknown").replace(/_/g, " ")}
              tone={marketStatus?.tradable ? "ok" : "warn"}
            />
            <span className="muted">·</span>
            <span className="small">
              Sessions: {(marketStatus?.active_sessions ?? []).join(", ") || "none"}
            </span>
            {marketStatus?.next_close ? (
              <>
                <span className="muted">·</span>
                <span className="small muted">
                  next close {time(marketStatus.next_close)} UTC
                </span>
              </>
            ) : null}
          </div>
          <p className="tiny muted" style={{ marginTop: 8, marginBottom: 0 }}>
            Session boundaries are defined in their own local timezones, so London and New
            York shift independently across daylight-saving changes. The trading week opens
            Sunday 17:00 and closes Friday 17:00, New York time.
          </p>
        </div>
      </Panel>

      <Panel
        title="Instruments"
        note="specifications are data, not code: adding a market is a seed change"
        flush
      >
        {instruments.loading ? (
          <Empty>Loading instruments…</Empty>
        ) : rows.length === 0 ? (
          <Empty>No instruments configured.</Empty>
        ) : (
          <div className="table-scroll tall">
            <table>
              <thead>
                <tr>
                  <th>Symbol</th>
                  <th>Name</th>
                  <th>Class</th>
                  <th className="right">Bid</th>
                  <th className="right">Ask</th>
                  <th className="right">Spread</th>
                  <th className="right">Age</th>
                  <th>Feed</th>
                  <th className="right">Contract</th>
                  <th className="right">Min</th>
                  <th className="right">Margin</th>
                  <th>Quoted</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {rows.map(({ instrument, quote, feed }) => (
                  <tr key={instrument.id}>
                    <td>
                      {instrument.symbol}
                      {!instrument.enabled ? (
                        <span className="badge" style={{ marginLeft: 6 }}>
                          disabled
                        </span>
                      ) : null}
                    </td>
                    <td className="muted truncate">{instrument.name}</td>
                    <td className="muted tiny">{instrument.asset_class}</td>
                    <td className="mono right">
                      {quote ? decimal(quote.bid, instrument.price_precision) : "—"}
                    </td>
                    <td className="mono right">
                      {quote ? decimal(quote.ask, instrument.price_precision) : "—"}
                    </td>
                    <td className="mono right muted">
                      {quote ? percent(quote.spread_fraction, 4) : "—"}
                    </td>
                    <td className="mono right muted">
                      {quote ? age(quote.age_seconds) : "—"}
                    </td>
                    <td>
                      {feed ? (
                        <Status
                          value={feed.state}
                          tone={
                            feed.tradable_by_automation
                              ? "ok"
                              : feed.state === "degraded"
                                ? "warn"
                                : "bad"
                          }
                        />
                      ) : (
                        <span className="muted tiny">no data</span>
                      )}
                    </td>
                    <td className="mono right">{decimal(instrument.contract_size, 0)}</td>
                    <td className="mono right">{instrument.min_quantity}</td>
                    <td className="mono right">{percent(instrument.margin_rate)}</td>
                    <td className="muted">{instrument.quote_currency}</td>
                    <td className="right">
                      <Link href={`/chart?instrument=${instrument.id}`} className="accent">
                        chart
                      </Link>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>

      <Panel title="Data quality" note="what would stop automated trading" flush>
        {Object.keys(health).length === 0 ? (
          <Empty>No feed observations yet.</Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Symbol</th>
                <th>State</th>
                <th>Issues</th>
                <th className="right">Quote age</th>
                <th className="right">Spread</th>
                <th>Provider</th>
                <th>Automation</th>
              </tr>
            </thead>
            <tbody>
              {Object.values(health)
                .sort((a, b) => a.symbol.localeCompare(b.symbol))
                .map((entry) => (
                  <tr key={entry.instrument_id}>
                    <td>{entry.symbol}</td>
                    <td>
                      <Status value={entry.state} />
                    </td>
                    <td className="muted small">
                      {entry.issues.length === 0
                        ? "none"
                        : entry.issues.map((i) => i.replace(/_/g, " ")).join(", ")}
                    </td>
                    <td className="mono right">{(entry.quote_age_ms / 1000).toFixed(1)}s</td>
                    <td className="mono right muted">
                      {percent(entry.spread_fraction, 4)}
                    </td>
                    <td className="muted">{entry.provider}</td>
                    <td>
                      <Status
                        value={entry.tradable_by_automation ? "permitted" : "blocked"}
                        tone={entry.tradable_by_automation ? "ok" : "bad"}
                      />
                    </td>
                  </tr>
                ))}
            </tbody>
          </table>
        )}
        <div className="panel-body">
          <p className="tiny muted" style={{ margin: 0 }}>
            Only a fully healthy feed is tradable by automation. Degraded, stale and invalid
            feeds all fail closed — a strategy that can act in milliseconds must not act on a
            price the platform no longer trusts. A manual order may still proceed on a
            degraded feed, because the person placing it can see this table.
          </p>
        </div>
      </Panel>
    </>
  );
}
