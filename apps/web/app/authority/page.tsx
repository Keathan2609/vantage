"use client";

/**
 * Trading authority.
 *
 * An authority is the mandate that says what Vantage may do on an account:
 * which instruments, which order types, which strategies, and up to what size.
 * Without an active authority every order is refused, manual ones included.
 *
 * The page repeats the server's notice verbatim rather than paraphrasing it. An
 * authority is a technical control; describing it as permission, consent or
 * authorisation would invite exactly the reading the notice exists to prevent.
 */

import { useState } from "react";

import { Empty, Notice, Panel, Status } from "@/components/ui";
import { ApiError, api } from "@/lib/api";
import { decimal, fullDateTime, humanise, money, relative } from "@/lib/format";
import { useAsync, useVantage } from "@/lib/store";

const ORDER_TYPES = ["market", "limit", "stop", "stop_limit"];

export default function AuthorityPage() {
  const { account, refresh: refreshShell } = useVantage();
  const accountId = account?.id;

  const authority = useAsync(
    () => (accountId ? api.authority(accountId) : Promise.resolve(null)),
    [accountId],
  );
  const instruments = useAsync(() => api.instruments(true), []);
  const strategies = useAsync(() => api.strategies(), []);
  const reconciliation = useAsync(
    () => (accountId ? api.reconciliation(accountId) : Promise.resolve(null)),
    [accountId],
  );

  const [chosenInstruments, setChosenInstruments] = useState<string[]>([]);
  const [chosenTypes, setChosenTypes] = useState<string[]>(["market"]);
  const [chosenStrategies, setChosenStrategies] = useState<string[]>([]);
  const [automation, setAutomation] = useState(false);
  const [maxQuantity, setMaxQuantity] = useState("0.10");
  const [maxNotional, setMaxNotional] = useState("2000");
  const [maxExposure, setMaxExposure] = useState("2000");
  const [maxLeverage, setMaxLeverage] = useState("20");
  const [maxDailyLoss, setMaxDailyLoss] = useState("50");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<{ tone: "ok" | "bad"; text: string } | null>(null);

  const current = authority.data?.authority ?? null;
  const currency = current?.currency ?? account?.currency ?? "ZAR";
  const paperStrategies = (strategies.data?.strategies ?? []).filter(
    (strategy) => !strategy.high_risk,
  );

  const toggle = (list: string[], value: string, set: (next: string[]) => void) => {
    set(list.includes(value) ? list.filter((item) => item !== value) : [...list, value]);
  };

  const report = (cause: unknown, fallback: string) => {
    setMessage({
      tone: "bad",
      text: cause instanceof ApiError ? `${cause.code}: ${cause.message}` : fallback,
    });
  };

  const grant = async () => {
    if (!accountId) return;
    setBusy(true);
    setMessage(null);
    try {
      await api.createAuthority({
        account_id: accountId,
        automation_enabled: automation,
        allowed_instruments: chosenInstruments,
        allowed_strategy_ids: chosenStrategies,
        allowed_order_types: chosenTypes,
        max_order_quantity: maxQuantity,
        max_order_notional: maxNotional,
        max_position_exposure: maxExposure,
        max_leverage: maxLeverage,
        max_daily_loss: maxDailyLoss,
      });
      setMessage({ tone: "ok", text: "Authority granted." });
      authority.reload();
    } catch (cause) {
      report(cause, "Could not grant the authority");
    } finally {
      setBusy(false);
    }
  };

  const revoke = async () => {
    if (!current) return;
    setBusy(true);
    setMessage(null);
    try {
      const result = await api.revokeAuthority(
        current.id,
        reason.trim() || "revoked from the authority page",
      );
      setMessage({ tone: "ok", text: result.message });
      setReason("");
      authority.reload();
    } catch (cause) {
      report(cause, "Could not revoke the authority");
    } finally {
      setBusy(false);
    }
  };

  const setAutomationEnabled = async (enabled: boolean) => {
    if (!current) return;
    setBusy(true);
    setMessage(null);
    try {
      await api.updateAuthority(current.id, {
        automation_enabled: enabled,
        version: current.version,
      });
      setMessage({
        tone: "ok",
        text: enabled
          ? "Automation enabled within this authority's limits."
          : "Automation disabled. Scheduled strategies will produce signals and place nothing.",
      });
      authority.reload();
    } catch (cause) {
      report(cause, "Could not change automation");
    } finally {
      setBusy(false);
    }
  };

  const setTrading = async (enabled: boolean) => {
    if (!accountId) return;
    setBusy(true);
    setMessage(null);
    try {
      await api.setTradingEnabled(
        accountId,
        enabled,
        reason.trim() || (enabled ? "resumed by operator" : "suspended by operator"),
      );
      setMessage({
        tone: "ok",
        text: enabled ? "Trading resumed on this account." : "Trading suspended on this account.",
      });
      refreshShell();
    } catch (cause) {
      report(cause, "Could not change the account's trading state");
    } finally {
      setBusy(false);
    }
  };

  const runReconciliation = async () => {
    if (!accountId) return;
    setBusy(true);
    setMessage(null);
    try {
      const result = await api.runReconciliation(accountId);
      setMessage({
        tone: result.clean ? "ok" : "bad",
        text: result.clean
          ? `Reconciliation clean: ${result.orders_compared} order(s) and ${result.positions_compared} position(s) agree with the venue.`
          : `Reconciliation found ${result.critical} critical discrepancy(ies). Automation is blocked until they are resolved.`,
      });
      reconciliation.reload();
    } catch (cause) {
      report(cause, "Reconciliation could not run");
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <Notice>
        {current?.notice ??
          authority.data?.notice ??
          "Trading authority is a technical control that limits what Vantage may do on this account. It is not a legal or regulatory authorisation."}
      </Notice>

      {message ? <Notice tone={message.tone}>{message.text}</Notice> : null}

      {!current && !authority.loading ? (
        <Notice tone="warn">
          {authority.data?.message ??
            "No active trading authority exists for this account. Every order — manual or automated — will be refused until one is granted."}
        </Notice>
      ) : null}

      <div className="grid sidebar-right">
        <div>
          <Panel
            title="Active authority"
            note={current ? `version ${current.version}` : undefined}
            actions={<button onClick={authority.reload}>Refresh</button>}
          >
            {authority.loading ? (
              <Empty>Loading…</Empty>
            ) : !current ? (
              <Empty>None. Grant one from the panel on the right.</Empty>
            ) : (
              <>
                <div className="grid cols-2">
                  <dl className="kv">
                    <dt>Mode</dt>
                    <dd>{current.mode}</dd>
                    <dt>Active</dt>
                    <dd>
                      <Status
                        value={current.active ? "active" : "inactive"}
                        tone={current.active ? "ok" : "bad"}
                      />
                    </dd>
                    <dt>Automation</dt>
                    <dd>
                      <Status
                        value={current.automation_enabled ? "enabled" : "disabled"}
                        tone={current.automation_enabled ? "ok" : "warn"}
                      />
                    </dd>
                    <dt>Valid from</dt>
                    <dd>{fullDateTime(current.valid_from)}</dd>
                    <dt>Valid until</dt>
                    <dd>
                      {current.valid_until
                        ? `${fullDateTime(current.valid_until)} (${relative(current.valid_until)})`
                        : "no expiry"}
                    </dd>
                  </dl>
                  <dl className="kv">
                    <dt>Max order quantity</dt>
                    <dd>{decimal(current.max_order_quantity, 2)} lots</dd>
                    <dt>Max order notional</dt>
                    <dd>{money(current.max_order_notional, currency)}</dd>
                    <dt>Max position exposure</dt>
                    <dd>{money(current.max_position_exposure, currency)}</dd>
                    <dt>Max leverage</dt>
                    <dd>{decimal(current.max_leverage, 0)}x</dd>
                    <dt>Max daily loss</dt>
                    <dd>{money(current.max_daily_loss, currency)}</dd>
                  </dl>
                </div>

                <div className="panel-title" style={{ margin: "12px 0 5px" }}>
                  Scope
                </div>
                <table>
                  <tbody>
                    <tr>
                      <td className="muted">Instruments</td>
                      <td className="mono">
                        {current.allowed_instruments.length === 0
                          ? "none"
                          : current.allowed_instruments.join(", ")}
                      </td>
                    </tr>
                    <tr>
                      <td className="muted">Order types</td>
                      <td className="mono">
                        {current.allowed_order_types.join(", ") || "none"}
                      </td>
                    </tr>
                    <tr>
                      <td className="muted">Strategies</td>
                      <td className="mono tiny">
                        {current.allowed_strategy_ids.length === 0
                          ? "any enabled PAPER strategy"
                          : current.allowed_strategy_ids
                              .map(
                                (id) =>
                                  paperStrategies.find((s) => s.id === id)?.name ??
                                  id.slice(0, 8),
                              )
                              .join(", ")}
                      </td>
                    </tr>
                  </tbody>
                </table>

                <div className="button-row" style={{ marginTop: 12 }}>
                  <button
                    disabled={busy}
                    onClick={() => void setAutomationEnabled(!current.automation_enabled)}
                  >
                    {current.automation_enabled ? "Disable automation" : "Enable automation"}
                  </button>
                  <button data-variant="danger" disabled={busy} onClick={() => void revoke()}>
                    Revoke authority
                  </button>
                </div>
                <div className="field" style={{ marginTop: 8 }}>
                  <label htmlFor="reason">Reason (audited, used for revocation)</label>
                  <input
                    id="reason"
                    value={reason}
                    onChange={(event) => setReason(event.target.value)}
                    placeholder="e.g. widening scope, replacing with a narrower mandate"
                  />
                </div>
              </>
            )}
          </Panel>

          <Panel
            title="Account trading state"
            note="a separate switch from the authority"
            flush
          >
            <div className="panel-body">
              <dl className="kv">
                <dt>Account</dt>
                <dd>{account?.name ?? "—"}</dd>
                <dt>Mode</dt>
                <dd>{account?.mode ?? "—"}</dd>
                <dt>Broker</dt>
                <dd>{account?.broker_name ?? "—"}</dd>
                <dt>Account enabled</dt>
                <dd>
                  <Status
                    value={account?.enabled ? "enabled" : "disabled"}
                    tone={account?.enabled ? "ok" : "bad"}
                  />
                </dd>
                <dt>Trading enabled</dt>
                <dd>
                  <Status
                    value={account?.trading_enabled ? "enabled" : "suspended"}
                    tone={account?.trading_enabled ? "ok" : "bad"}
                  />
                </dd>
              </dl>
              <div className="button-row">
                <button
                  disabled={busy || !account}
                  onClick={() => void setTrading(!account?.trading_enabled)}
                >
                  {account?.trading_enabled ? "Suspend trading" : "Resume trading"}
                </button>
              </div>
              <p className="tiny muted" style={{ marginBottom: 0 }}>
                Three separate things can stop an order: the account&rsquo;s trading flag, the
                authority, and a kill switch. They are deliberately not combined — an operator
                needs to be able to suspend one account without revoking a mandate they will
                want back in ten minutes.
              </p>
            </div>
          </Panel>
        </div>

        <div>
          <Panel title="Grant an authority" flush>
            <div className="panel-body">
              {current ? (
                <Notice tone="warn">
                  An active authority already exists. Only one can be active per account —
                  revoke the current one before granting a replacement.
                </Notice>
              ) : null}

              <div className="field">
                <label>Instruments this authority covers</label>
                <div className="button-row" style={{ flexWrap: "wrap" }}>
                  {(instruments.data?.instruments ?? []).map((instrument) => (
                    <button
                      key={instrument.id}
                      data-selected={chosenInstruments.includes(instrument.id)}
                      onClick={() =>
                        toggle(chosenInstruments, instrument.id, setChosenInstruments)
                      }
                    >
                      {instrument.symbol}
                    </button>
                  ))}
                </div>
              </div>

              <div className="field">
                <label>Order types</label>
                <div className="button-row" style={{ flexWrap: "wrap" }}>
                  {ORDER_TYPES.map((type) => (
                    <button
                      key={type}
                      data-selected={chosenTypes.includes(type)}
                      onClick={() => toggle(chosenTypes, type, setChosenTypes)}
                    >
                      {humanise(type)}
                    </button>
                  ))}
                </div>
              </div>

              <div className="field">
                <label>Strategies (none selected means any enabled PAPER strategy)</label>
                <div className="button-row" style={{ flexWrap: "wrap" }}>
                  {paperStrategies.slice(0, 12).map((strategy) => (
                    <button
                      key={strategy.id}
                      data-selected={chosenStrategies.includes(strategy.id)}
                      onClick={() =>
                        toggle(chosenStrategies, strategy.id, setChosenStrategies)
                      }
                      title={strategy.description}
                    >
                      {strategy.name}
                    </button>
                  ))}
                </div>
              </div>

              <div className="field-row">
                <div className="field">
                  <label htmlFor="qty">Max order quantity (lots)</label>
                  <input
                    id="qty"
                    value={maxQuantity}
                    onChange={(event) => setMaxQuantity(event.target.value)}
                  />
                </div>
                <div className="field">
                  <label htmlFor="notional">Max order notional ({currency})</label>
                  <input
                    id="notional"
                    value={maxNotional}
                    onChange={(event) => setMaxNotional(event.target.value)}
                  />
                </div>
              </div>
              <div className="field-row">
                <div className="field">
                  <label htmlFor="exposure">Max position exposure ({currency})</label>
                  <input
                    id="exposure"
                    value={maxExposure}
                    onChange={(event) => setMaxExposure(event.target.value)}
                  />
                </div>
                <div className="field">
                  <label htmlFor="leverage">Max leverage</label>
                  <input
                    id="leverage"
                    value={maxLeverage}
                    onChange={(event) => setMaxLeverage(event.target.value)}
                  />
                </div>
              </div>
              <div className="field">
                <label htmlFor="dailyloss">Max daily loss ({currency})</label>
                <input
                  id="dailyloss"
                  value={maxDailyLoss}
                  onChange={(event) => setMaxDailyLoss(event.target.value)}
                />
              </div>
              <div className="field">
                <label>
                  <input
                    type="checkbox"
                    checked={automation}
                    onChange={(event) => setAutomation(event.target.checked)}
                    style={{ marginRight: 6 }}
                  />
                  Allow automation to act within these limits
                </label>
              </div>

              <button
                data-variant="primary"
                disabled={busy || !!current || chosenInstruments.length === 0}
                onClick={() => void grant()}
                style={{ width: "100%" }}
              >
                {busy ? "Working…" : "Grant authority"}
              </button>
              <p className="tiny muted" style={{ marginTop: 8, marginBottom: 0 }}>
                These ceilings are an upper bound, not a target. The risk engine applies its
                own limits on top, and whichever is smaller wins.
              </p>
            </div>
          </Panel>

          <Panel title="Reconciliation" flush>
            <div className="panel-body">
              {reconciliation.data ? (
                <dl className="kv">
                  <dt>Automation</dt>
                  <dd>
                    <Status
                      value={
                        reconciliation.data.automation_blocked ? "blocked" : "permitted"
                      }
                      tone={reconciliation.data.automation_blocked ? "bad" : "ok"}
                    />
                  </dd>
                  <dt>Critical discrepancies</dt>
                  <dd className={reconciliation.data.critical_discrepancies > 0 ? "loss" : ""}>
                    {reconciliation.data.critical_discrepancies}
                  </dd>
                  <dt>Last run</dt>
                  <dd>
                    {reconciliation.data.last_run
                      ? `${humanise(reconciliation.data.last_run.Status)} · ${relative(
                          reconciliation.data.last_run.StartedAt,
                        )}`
                      : "never"}
                  </dd>
                  <dt>Compared</dt>
                  <dd>
                    {reconciliation.data.last_run
                      ? `${reconciliation.data.last_run.OrdersCompared} order(s), ${reconciliation.data.last_run.PositionsCompared} position(s)`
                      : "—"}
                  </dd>
                </dl>
              ) : (
                <Empty>No reconciliation state.</Empty>
              )}
              <button disabled={busy} onClick={() => void runReconciliation()}>
                Reconcile now
              </button>
              <p className="tiny muted" style={{ marginTop: 8, marginBottom: 0 }}>
                Reconciliation compares Vantage&rsquo;s records against the venue&rsquo;s. A
                critical disagreement blocks automation rather than being logged and ignored:
                a system that does not know its own position must not open another.
              </p>
            </div>
          </Panel>
        </div>
      </div>
    </>
  );
}
