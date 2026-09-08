"use client";

/**
 * Overview: everything an operator needs to know before doing anything else.
 *
 * Ordered by what would stop you trading, not by what is pleasant to look at:
 * blocking conditions first (kill switches, reconciliation, authority, feed
 * health), then account state, then activity.
 */

import Link from "next/link";

import { Empty, Meter, Notice, Panel, Stat, StatStrip, Status } from "@/components/ui";
import { api } from "@/lib/api";
import {
  age,
  decimal,
  dateTime,
  humanise,
  money,
  percent,
  relative,
  signed,
  tone,
} from "@/lib/format";
import { useAsync, useVantage, useNow } from "@/lib/store";

export default function OverviewPage() {
  const { account, portfolio, quotes, health, marketStatus, version } = useVantage();
  const accountId = account?.id;

  const risk = useAsync(
    () => (accountId ? api.riskLimits(accountId) : Promise.resolve(null)),
    [accountId],
  );
  const authority = useAsync(
    () => (accountId ? api.authority(accountId) : Promise.resolve(null)),
    [accountId],
  );
  const reconciliation = useAsync(
    () => (accountId ? api.reconciliation(accountId) : Promise.resolve(null)),
    [accountId],
  );
  const switches = useAsync(() => api.killSwitches(true), []);
  const signals = useAsync(
    () => (accountId ? api.signals(accountId, 8) : Promise.resolve(null)),
    [accountId],
  );
  const activity = useAsync(
    () => (accountId ? api.activity(accountId, 14) : Promise.resolve(null)),
    [accountId],
  );
  const calendar = useAsync(() => api.calendar("week", "high"), []);
  const strategies = useAsync(() => api.strategies(), []);

  // A ticking clock, so "in 3h" counts down instead of freezing at load.
  const now = useNow(30_000);

  const currency = portfolio?.currency ?? account?.currency ?? "ZAR";
  const activeSwitches = switches.data?.kill_switches ?? [];
  const blocked = reconciliation.data?.automation_blocked ?? false;
  const mandate = authority.data?.authority ?? null;

  const nextEvent = (calendar.data?.events ?? [])
    .filter((event) => new Date(event.ScheduledAt).getTime() > now)
    .sort(
      (a, b) => new Date(a.ScheduledAt).getTime() - new Date(b.ScheduledAt).getTime(),
    )[0];

  const unhealthyFeeds = Object.values(health).filter(
    (entry) => !entry.tradable_by_automation,
  );

  return (
    <>
      {/* Anything that would prevent trading is stated before the numbers. */}
      {activeSwitches.length > 0 ? (
        <Notice tone="bad">
          <strong>Trading halted.</strong>{" "}
          {activeSwitches.map((sw) => `${sw.Scope}: ${sw.Reason}`).join(" · ")} — new orders
          are refused within scope. Open positions are NOT closed; use Flatten on a position
          to close it deliberately.
        </Notice>
      ) : null}

      {blocked ? (
        <Notice tone="bad">
          <strong>Automated trading paused.</strong>{" "}
          {reconciliation.data?.critical_discrepancies} unresolved reconciliation
          discrepancy(ies) mean Vantage and the venue disagree about what is open. Sizing a
          trade against a position book known to be wrong is guesswork.{" "}
          <Link href="/connections" className="accent">
            Review reconciliation
          </Link>
        </Notice>
      ) : null}

      {!mandate ? (
        <Notice tone="warn">
          <strong>No active trading authority.</strong> Manual and automated orders on this
          account will be refused until a mandate is granted.{" "}
          <Link href="/authority" className="accent">
            Grant authority
          </Link>
        </Notice>
      ) : !mandate.automation_enabled ? (
        <Notice tone="warn">
          Automation is switched off on this account&rsquo;s trading authority. Manual paper
          orders are still accepted; strategies will not place any.
        </Notice>
      ) : null}

      {marketStatus && !marketStatus.tradable ? (
        <Notice tone="warn">
          The market is {humanise(marketStatus.status)}. Orders are refused while it is
          closed, and no market data is ingested.
        </Notice>
      ) : null}

      {unhealthyFeeds.length > 0 && marketStatus?.tradable ? (
        <Notice tone="warn">
          {unhealthyFeeds.length} instrument feed(s) are not fit for automated trading:{" "}
          {unhealthyFeeds.map((f) => `${f.symbol} (${f.state})`).join(", ")}. Automation
          fails closed on an untrusted feed.
        </Notice>
      ) : null}

      <Panel
        title="Account"
        note={`${account?.name ?? "—"} · ${account?.broker_name ?? "—"} · simulated`}
        flush
      >
        <StatStrip>
          <Stat label="Equity" value={money(portfolio?.equity, currency)} />
          <Stat
            label={"Day P&L"}
            value={signed(portfolio?.day_pnl, currency)}
            tone={tone(portfolio?.day_pnl)}
            sub={`limit ${money(risk.data?.limits.max_daily_loss, currency)}`}
          />
          <Stat
            label={"Open P&L"}
            value={signed(portfolio?.unrealized_pnl, currency)}
            tone={tone(portfolio?.unrealized_pnl)}
            sub={`${portfolio?.open_positions ?? 0} position(s)`}
          />
          <Stat
            label={"Realised P&L"}
            value={signed(portfolio?.realized_pnl, currency)}
            tone={tone(portfolio?.realized_pnl)}
          />
          <Stat
            label="Free margin"
            value={money(portfolio?.free_margin, currency)}
            sub={`used ${money(portfolio?.margin_used, currency)}`}
          />
          <Stat
            label="Drawdown"
            value={percent(portfolio?.drawdown_fraction)}
            sub={`peak ${money(portfolio?.peak_equity, currency)}`}
          />
        </StatStrip>
      </Panel>

      <div className="grid sidebar-right">
        <div>
          <Panel
            title="Risk utilisation"
            note={risk.data ? "share of each configured limit in use" : undefined}
            flush
          >
            {risk.loading ? (
              <Empty>Loading limits…</Empty>
            ) : !risk.data ? (
              <Empty>No risk limits are configured for this account.</Empty>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>Limit</th>
                    <th className="right">Used</th>
                    <th className="right">Ceiling</th>
                    <th style={{ width: 130 }}>Utilisation</th>
                  </tr>
                </thead>
                <tbody>
                  {[
                    {
                      key: "daily_loss",
                      label: "Daily loss",
                      used: portfolio?.day_pnl?.startsWith("-")
                        ? money(portfolio.day_pnl.slice(1), currency)
                        : money("0", currency),
                      limit: money(risk.data.limits.max_daily_loss, currency),
                    },
                    {
                      key: "drawdown",
                      label: "Drawdown",
                      used: percent(portfolio?.drawdown_fraction),
                      limit: percent(risk.data.limits.max_drawdown_fraction),
                    },
                    {
                      key: "gross_exposure",
                      label: "Gross exposure",
                      used: money(portfolio?.gross_exposure, currency),
                      limit: money(risk.data.limits.max_gross_exposure, currency),
                    },
                    {
                      key: "open_positions",
                      label: "Open positions",
                      used: String(portfolio?.open_positions ?? 0),
                      limit: String(risk.data.limits.max_open_positions),
                    },
                    {
                      key: "pending_orders",
                      label: "Pending orders",
                      used: String(portfolio?.pending_orders ?? 0),
                      limit: String(risk.data.limits.max_pending_orders),
                    },
                  ].map((row) => {
                    const utilisation = Number.parseFloat(
                      risk.data?.utilisation[row.key] ?? "0",
                    );
                    return (
                      <tr key={row.key}>
                        <td>{row.label}</td>
                        <td className="mono right">{row.used}</td>
                        <td className="mono right muted">{row.limit}</td>
                        <td>
                          <Meter fraction={utilisation} />
                          <span className="tiny muted">
                            {Number.isFinite(utilisation)
                              ? `${(utilisation * 100).toFixed(0)}%`
                              : "—"}
                          </span>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            )}
          </Panel>

          <Panel title="Markets" note="live prices with feed health" flush>
            {Object.keys(quotes).length === 0 ? (
              <Empty>
                No quotes yet. Market data is only ingested while the market is open.
              </Empty>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>Symbol</th>
                    <th className="right">Bid</th>
                    <th className="right">Ask</th>
                    <th className="right">Spread</th>
                    <th className="right">Age</th>
                    <th>Feed</th>
                  </tr>
                </thead>
                <tbody>
                  {Object.values(quotes)
                    .sort((a, b) => a.symbol.localeCompare(b.symbol))
                    .map((quote) => {
                      const feed = health[quote.instrument_id];
                      return (
                        <tr key={quote.instrument_id}>
                          <td>
                            <Link href={`/chart?instrument=${quote.instrument_id}`}>
                              {quote.symbol}
                            </Link>
                          </td>
                          <td className="mono right">{decimal(quote.bid, 2)}</td>
                          <td className="mono right">{decimal(quote.ask, 2)}</td>
                          <td className="mono right muted">
                            {percent(quote.spread_fraction, 3)}
                          </td>
                          <td className="mono right muted">{age(quote.age_seconds)}</td>
                          <td>
                            <Status
                              value={feed?.state ?? "unknown"}
                              tone={
                                feed?.tradable_by_automation
                                  ? "ok"
                                  : feed?.state === "degraded"
                                    ? "warn"
                                    : "bad"
                              }
                            />
                          </td>
                        </tr>
                      );
                    })}
                </tbody>
              </table>
            )}
          </Panel>

          <Panel title="Recent activity" note="orders, refusals, decisions and audit" flush>
            {activity.loading ? (
              <Empty>Loading activity…</Empty>
            ) : (activity.data?.activity ?? []).length === 0 ? (
              <Empty>Nothing has happened on this account yet.</Empty>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th style={{ width: 74 }}>Kind</th>
                    <th>Event</th>
                    <th>Detail</th>
                    <th className="right">When</th>
                  </tr>
                </thead>
                <tbody>
                  {(activity.data?.activity ?? []).map((item, index) => (
                    <tr key={`${item.at}-${index}`}>
                      <td className="muted tiny">{item.kind}</td>
                      <td>
                        <Status
                          value={item.severity}
                          label={item.title}
                          tone={
                            item.severity === "critical"
                              ? "bad"
                              : item.severity === "warning"
                                ? "warn"
                                : "ok"
                          }
                        />
                      </td>
                      <td className="muted truncate" title={item.detail}>
                        {item.detail}
                      </td>
                      <td className="mono right muted" title={dateTime(item.at)}>
                        {relative(item.at)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Panel>
        </div>

        <div>
          <Panel title="Next high-impact event" flush>
            {nextEvent ? (
              <div className="panel-body">
                <div className="row-gap" style={{ marginBottom: 6 }}>
                  <span className="badge" data-tone="bad">
                    {nextEvent.Impact}
                  </span>
                  <span className="badge">{nextEvent.Currency}</span>
                  <span className="mono">{relative(nextEvent.ScheduledAt)}</span>
                </div>
                <div style={{ marginBottom: 4 }}>{nextEvent.EventName}</div>
                <dl className="kv">
                  <dt>Scheduled</dt>
                  <dd>{dateTime(nextEvent.ScheduledAt)}</dd>
                  <dt>Forecast</dt>
                  <dd>{nextEvent.Forecast ?? "—"}</dd>
                  <dt>Previous</dt>
                  <dd>{nextEvent.Previous ?? "—"}</dd>
                </dl>
                <p className="tiny muted" style={{ marginBottom: 0, marginTop: 8 }}>
                  Automated trading is blocked{" "}
                  {risk.data?.limits.event_blackout_before_minutes ?? 15} minutes before and{" "}
                  {risk.data?.limits.event_blackout_after_minutes ?? 10} after a high-impact
                  release. Spreads widen and stops slip through releases.
                </p>
              </div>
            ) : (
              <Empty>No high-impact release scheduled this week.</Empty>
            )}
          </Panel>

          <Panel title="Trading authority" flush>
            {mandate ? (
              <div className="panel-body">
                <dl className="kv">
                  <dt>State</dt>
                  <dd>
                    <Status value={mandate.active ? "active" : "inactive"} />
                  </dd>
                  <dt>Automation</dt>
                  <dd>
                    <Status
                      value={mandate.automation_enabled ? "enabled" : "disabled"}
                      tone={mandate.automation_enabled ? "ok" : "warn"}
                    />
                  </dd>
                  <dt>Instruments</dt>
                  <dd>{mandate.allowed_instruments.join(", ") || "none"}</dd>
                  <dt>Max order</dt>
                  <dd>{decimal(mandate.max_order_quantity, 2)} lots</dd>
                  <dt>Max notional</dt>
                  <dd>{money(mandate.max_order_notional, mandate.currency)}</dd>
                  <dt>Expires</dt>
                  <dd>{mandate.valid_until ? relative(mandate.valid_until) : "no expiry"}</dd>
                </dl>
                <p className="tiny muted" style={{ marginTop: 8, marginBottom: 0 }}>
                  {mandate.notice}
                </p>
              </div>
            ) : (
              <Empty>No authority granted.</Empty>
            )}
          </Panel>

          <Panel title="Latest signals" flush>
            {(signals.data?.signals ?? []).length === 0 ? (
              <Empty>
                No signals yet. Strategies are evaluated once per completed bar while the
                market is open.
              </Empty>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>Action</th>
                    <th>Instrument</th>
                    <th className="right">Conf.</th>
                    <th className="right">Bar</th>
                  </tr>
                </thead>
                <tbody>
                  {(signals.data?.signals ?? []).map((signal) => (
                    <tr key={signal.ID} title={signal.Explanation}>
                      <td>
                        <Badge action={signal.Action} />
                      </td>
                      <td className="mono">{signal.InstrumentID}</td>
                      <td className="mono right">{decimal(signal.Confidence, 2)}</td>
                      <td className="mono right muted">{dateTime(signal.BarTime)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Panel>

          <Panel title="Strategies at PAPER" flush>
            {(strategies.data?.strategies ?? []).filter((s) => s.lifecycle === "PAPER")
              .length === 0 ? (
              <Empty>No strategy has been promoted to PAPER.</Empty>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>Strategy</th>
                    <th className="right">Runs 7d</th>
                    <th className="right">Signals</th>
                  </tr>
                </thead>
                <tbody>
                  {(strategies.data?.strategies ?? [])
                    .filter((s) => s.lifecycle === "PAPER")
                    .map((strategy) => (
                      <tr key={strategy.id}>
                        <td>
                          <Link href={`/strategies?focus=${strategy.id}`}>
                            {strategy.name}
                          </Link>
                          <div className="tiny muted">{humanise(strategy.family)}</div>
                        </td>
                        <td className="mono right">{strategy.runs_7d}</td>
                        <td className="mono right">{strategy.signals_7d}</td>
                      </tr>
                    ))}
                </tbody>
              </table>
            )}
          </Panel>

          <Panel title="Build" flush>
            <div className="panel-body">
              <dl className="kv">
                <dt>Version</dt>
                <dd>{version?.version ?? "—"}</dd>
                <dt>Execution mode</dt>
                <dd>{version?.execution_mode ?? "—"}</dd>
                <dt>Funds</dt>
                <dd>{version?.simulated_funds ? "simulated" : "real"}</dd>
                <dt>Live trading</dt>
                <dd className="loss">
                  {version?.live_trading_available ? "available" : "not available"}
                </dd>
              </dl>
              <p className="tiny muted" style={{ marginTop: 8, marginBottom: 0 }}>
                No live broker adapter is compiled into this build, and the database refuses
                to store a non-paper order.
              </p>
            </div>
          </Panel>
        </div>
      </div>
    </>
  );
}

function Badge({ action }: { action: string }) {
  const tone =
    action === "buy" ? "ok" : action === "sell" ? "bad" : undefined;
  return (
    <span className="badge" data-tone={tone}>
      {action.replace(/_/g, " ")}
    </span>
  );
}
