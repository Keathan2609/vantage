"use client";

/**
 * Risk and kill switches.
 *
 * The page states plainly, next to the button, that a kill switch stops NEW
 * orders and does not close anything. Conflating the two is how an operator
 * hits "stop" in an incident and is surprised either way — expecting
 * liquidation and not getting it, or not expecting it and getting it.
 */

import { useState } from "react";

import { Empty, Meter, Notice, Panel, Status } from "@/components/ui";
import { ApiError, api } from "@/lib/api";
import { dateTime, humanise, money, percent, relative } from "@/lib/format";
import { useAsync, useVantage } from "@/lib/store";

export default function RiskPage() {
  const { account, portfolio } = useVantage();
  const accountId = account?.id;

  const risk = useAsync(
    () => (accountId ? api.riskLimits(accountId) : Promise.resolve(null)),
    [accountId],
  );
  const events = useAsync(
    () => (accountId ? api.riskEvents(accountId, 80) : Promise.resolve(null)),
    [accountId],
  );
  const switches = useAsync(() => api.killSwitches(false), []);

  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<{ tone: "ok" | "bad"; text: string } | null>(null);

  const currency = risk.data?.limits.currency ?? account?.currency ?? "ZAR";
  const limits = risk.data?.limits;
  const utilisation = risk.data?.utilisation ?? {};
  const active = (switches.data?.kill_switches ?? []).filter((s) => s.Active);

  const activate = async () => {
    if (!accountId || reason.trim().length < 3) {
      setMessage({ tone: "bad", text: "Give a reason: it is audited and shown to anyone halted." });
      return;
    }
    setBusy(true);
    setMessage(null);
    try {
      const result = await api.activateKillSwitch("account", accountId, reason.trim());
      setMessage({ tone: "ok", text: result.message });
      setReason("");
      switches.reload();
    } catch (cause) {
      setMessage({
        tone: "bad",
        text: cause instanceof ApiError ? cause.message : "Could not activate the kill switch",
      });
    } finally {
      setBusy(false);
    }
  };

  const deactivate = async (id: string) => {
    setBusy(true);
    setMessage(null);
    try {
      await api.deactivateKillSwitch(id);
      setMessage({ tone: "ok", text: "Kill switch released. New orders are accepted again." });
      switches.reload();
    } catch (cause) {
      setMessage({
        tone: "bad",
        text: cause instanceof ApiError ? cause.message : "Could not release the kill switch",
      });
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      {message ? <Notice tone={message.tone}>{message.text}</Notice> : null}

      {active.length > 0 ? (
        <Notice tone="bad">
          <strong>{active.length} kill switch(es) active.</strong> New orders are refused
          within scope. Open positions are unaffected.
        </Notice>
      ) : null}

      <div className="grid sidebar-right">
        <div>
          <Panel
            title="Risk limits and utilisation"
            note="enforced outside strategy code; a strategy cannot read or widen these"
            flush
          >
            {risk.loading ? (
              <Empty>Loading limits…</Empty>
            ) : !limits ? (
              <Empty>No limits configured.</Empty>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>Limit</th>
                    <th className="right">Configured</th>
                    <th className="right">Current</th>
                    <th style={{ width: 140 }}>Utilisation</th>
                  </tr>
                </thead>
                <tbody>
                  <LimitRow
                    label="Max risk per trade"
                    configured={percent(limits.max_risk_per_trade_fraction)}
                    current="per order"
                  />
                  <LimitRow
                    label="Max daily loss"
                    configured={money(limits.max_daily_loss, currency)}
                    current={
                      portfolio?.day_pnl?.startsWith("-")
                        ? money(portfolio.day_pnl.slice(1), currency)
                        : money("0", currency)
                    }
                    utilisation={utilisation.daily_loss}
                  />
                  <LimitRow
                    label="Max drawdown"
                    configured={percent(limits.max_drawdown_fraction)}
                    current={percent(portfolio?.drawdown_fraction)}
                    utilisation={utilisation.drawdown}
                  />
                  <LimitRow
                    label="Max order quantity"
                    configured={`${limits.max_order_quantity} lots`}
                    current="per order"
                  />
                  <LimitRow
                    label="Max order notional"
                    configured={money(limits.max_order_notional, currency)}
                    current="per order"
                  />
                  <LimitRow
                    label="Max gross exposure"
                    configured={money(limits.max_gross_exposure, currency)}
                    current={money(portfolio?.gross_exposure, currency)}
                    utilisation={utilisation.gross_exposure}
                  />
                  <LimitRow
                    label="Max net exposure"
                    configured={money(limits.max_net_exposure, currency)}
                    current={money(portfolio?.net_exposure, currency)}
                    utilisation={utilisation.net_exposure}
                  />
                  <LimitRow
                    label="Max per-instrument exposure"
                    configured={money(limits.max_per_instrument_exposure, currency)}
                    current="per instrument"
                  />
                  <LimitRow
                    label="Max concentration"
                    configured={percent(limits.max_concentration_fraction)}
                    current="applies with 2+ instruments"
                  />
                  <LimitRow
                    label="Max leverage"
                    configured={`${limits.max_leverage}x`}
                    current={
                      portfolio && Number.parseFloat(portfolio.equity) > 0
                        ? `${(
                            Number.parseFloat(portfolio.gross_exposure) /
                            Number.parseFloat(portfolio.equity)
                          ).toFixed(2)}x`
                        : "—"
                    }
                  />
                  <LimitRow
                    label="Max open positions"
                    configured={String(limits.max_open_positions)}
                    current={String(portfolio?.open_positions ?? 0)}
                    utilisation={utilisation.open_positions}
                  />
                  <LimitRow
                    label="Max pending orders"
                    configured={String(limits.max_pending_orders)}
                    current={String(portfolio?.pending_orders ?? 0)}
                    utilisation={utilisation.pending_orders}
                  />
                  <LimitRow
                    label="Max spread"
                    configured={percent(limits.max_spread_fraction, 3)}
                    current="checked per order"
                  />
                  <LimitRow
                    label="Stop loss required"
                    configured={limits.require_stop_loss ? "yes" : "no"}
                    current="per order"
                  />
                  <LimitRow
                    label="Event blackout"
                    configured={`${limits.event_blackout_before_minutes}m before / ${limits.event_blackout_after_minutes}m after`}
                    current={
                      limits.block_on_high_impact_events ? "enforced" : "not enforced"
                    }
                  />
                </tbody>
              </table>
            )}
          </Panel>

          <Panel title="Risk events" note="every refusal and breach, with its reason" flush>
            {events.loading ? (
              <Empty>Loading…</Empty>
            ) : (events.data?.events ?? []).length === 0 ? (
              <Empty>No risk events recorded.</Empty>
            ) : (
              <div className="table-scroll">
                <table>
                  <thead>
                    <tr>
                      <th>Severity</th>
                      <th>Check</th>
                      <th>Code</th>
                      <th>Message</th>
                      <th className="right">When</th>
                    </tr>
                  </thead>
                  <tbody>
                    {(events.data?.events ?? []).map((event) => (
                      <tr key={event.ID}>
                        <td>
                          <Status value={event.Severity} />
                        </td>
                        <td className="muted">{humanise(event.Check)}</td>
                        <td className="tiny muted">{event.Code}</td>
                        <td className="truncate wrap" title={event.Message}>
                          {event.Message}
                        </td>
                        <td className="mono right muted" title={dateTime(event.CreatedAt)}>
                          {relative(event.CreatedAt)}
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
          <Panel title="Kill switch" flush>
            <div className="panel-body">
              <Notice tone="warn">
                A kill switch stops NEW orders within its scope. It does <strong>not</strong>{" "}
                close open positions. Automatic liquidation on an alarm would dump positions
                into exactly the conditions that raised the alarm — closing a position is the
                separate, confirmed Flatten action.
              </Notice>

              <div className="field">
                <label htmlFor="reason">Reason (audited)</label>
                <input
                  id="reason"
                  value={reason}
                  onChange={(event) => setReason(event.target.value)}
                  placeholder="e.g. investigating unexpected fills"
                />
              </div>
              <button
                data-variant="danger"
                onClick={() => void activate()}
                disabled={busy}
                style={{ width: "100%" }}
              >
                {busy ? "Working…" : "Halt new orders on this account"}
              </button>
            </div>
          </Panel>

          <Panel title="Switch history" flush>
            {(switches.data?.kill_switches ?? []).length === 0 ? (
              <Empty>No kill switch has ever been activated.</Empty>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>Scope</th>
                    <th>State</th>
                    <th>Reason</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {(switches.data?.kill_switches ?? []).map((sw) => (
                    <tr key={sw.ID}>
                      <td>{sw.Scope}</td>
                      <td>
                        <Status
                          value={sw.Active ? "active" : "released"}
                          tone={sw.Active ? "bad" : "ok"}
                        />
                      </td>
                      <td className="muted truncate" title={sw.Reason}>
                        {sw.Reason}
                      </td>
                      <td className="right">
                        {sw.Active ? (
                          <button onClick={() => void deactivate(sw.ID)} disabled={busy}>
                            Release
                          </button>
                        ) : (
                          <span className="tiny muted">
                            {relative(sw.DeactivatedAt ?? sw.ActivatedAt)}
                          </span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Panel>

          <Panel title="How refusals work" flush>
            <div className="panel-body">
              <p className="small muted" style={{ marginTop: 0 }}>
                Every check runs on every order, and all failures are reported rather than
                only the first. That way an operator who fixes one breached ceiling is not
                surprised by the next three.
              </p>
              <p className="small muted" style={{ marginBottom: 0 }}>
                Risk may only <strong>reduce</strong>. The engine can shrink a requested size
                or refuse it; there is no path by which it can approve more than was asked
                for. NO TRADE is a first-class outcome, and on a small account it is the
                common one.
              </p>
            </div>
          </Panel>
        </div>
      </div>
    </>
  );
}

function LimitRow({
  label,
  configured,
  current,
  utilisation,
}: {
  label: string;
  configured: string;
  current: string;
  utilisation?: string;
}) {
  const fraction = utilisation ? Number.parseFloat(utilisation) : null;
  return (
    <tr>
      <td>{label}</td>
      <td className="mono right muted">{configured}</td>
      <td className="mono right">{current}</td>
      <td>
        {fraction !== null && Number.isFinite(fraction) ? (
          <>
            <Meter fraction={fraction} />
            <span className="tiny muted">{(fraction * 100).toFixed(0)}%</span>
          </>
        ) : (
          <span className="tiny muted">—</span>
        )}
      </td>
    </tr>
  );
}
