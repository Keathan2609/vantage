"use client";

/**
 * Positions.
 *
 * Flatten is presented as a distinct, confirmed action rather than a row
 * button that fires on one click. Closing a position at market is irreversible
 * and costs the spread; it deserves a moment of friction. The kill switch,
 * which stops new orders, is deliberately NOT this — see the Risk page.
 */

import { useState } from "react";

import { Empty, Notice, Panel, Status } from "@/components/ui";
import { ApiError, api, newIdempotencyKey } from "@/lib/api";
import { decimal, money, relative, signed, tone } from "@/lib/format";
import { useVantage } from "@/lib/store";

export default function PositionsPage() {
  const { portfolio, account, refresh } = useVantage();
  const [confirming, setConfirming] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<{ tone: "ok" | "bad"; text: string } | null>(null);

  const currency = portfolio?.currency ?? account?.currency ?? "ZAR";
  const positions = portfolio?.positions ?? [];

  const flatten = async (positionId: string, label: string) => {
    setBusy(true);
    setMessage(null);
    try {
      const result = await api.flatten(positionId, newIdempotencyKey("flatten"));
      if (result.rejection) {
        setMessage({
          tone: "bad",
          text: `${label} was not closed: ${result.rejection.message}`,
        });
      } else {
        setMessage({ tone: "ok", text: `${label} closed at market.` });
      }
      refresh();
    } catch (cause) {
      setMessage({
        tone: "bad",
        text:
          cause instanceof ApiError
            ? `${cause.code}: ${cause.message}`
            : "The position could not be closed",
      });
    } finally {
      setBusy(false);
      setConfirming(null);
    }
  };

  return (
    <>
      {message ? <Notice tone={message.tone}>{message.text}</Notice> : null}
      {portfolio && portfolio.unvalued_positions > 0 ? (
        <Notice tone="warn">
          {portfolio.unvalued_positions} position(s) could not be priced. The exposure and
          equity figures below therefore UNDERSTATE risk — an unvalued position is shown as
          unvalued rather than as zero.
        </Notice>
      ) : null}

      <Panel
        title="Open positions"
        note={`${positions.length} open · simulated · marked at the price they could be closed at`}
        flush
      >
        {positions.length === 0 ? (
          <Empty>No open positions.</Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Instrument</th>
                <th>Side</th>
                <th className="right">Quantity</th>
                <th className="right">Entry</th>
                <th className="right">Mark</th>
                <th className="right">Stop</th>
                <th className="right">Target</th>
                <th className="right">Notional</th>
                <th className="right">Margin</th>
                <th className="right">Open P&amp;L</th>
                <th className="right">Held</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {positions.map((position) => (
                <tr key={position.id}>
                  <td>{position.symbol}</td>
                  <td>
                    <span
                      className="badge"
                      data-tone={position.side === "buy" ? "ok" : "bad"}
                    >
                      {position.side}
                    </span>
                  </td>
                  <td className="mono right">{position.quantity}</td>
                  <td className="mono right">{decimal(position.avg_entry_price, 2)}</td>
                  <td className="mono right">
                    {position.valued ? (
                      decimal(position.current_price, 2)
                    ) : (
                      <span className="loss" title={position.valuation_note}>
                        unvalued
                      </span>
                    )}
                  </td>
                  <td className="mono right muted">
                    {position.stop_loss ? decimal(position.stop_loss, 2) : "none"}
                  </td>
                  <td className="mono right muted">
                    {position.take_profit ? decimal(position.take_profit, 2) : "—"}
                  </td>
                  <td className="mono right">{money(position.notional_value, currency)}</td>
                  <td className="mono right">{money(position.margin_used, currency)}</td>
                  <td className={`mono right ${tone(position.unrealized_pnl)}`}>
                    {signed(position.unrealized_pnl, position.unrealized_pnl_currency)}
                  </td>
                  <td className="mono right muted">{relative(position.opened_at)}</td>
                  <td className="right">
                    {confirming === position.id ? (
                      <span className="row-gap">
                        <button
                          data-variant="danger"
                          disabled={busy}
                          onClick={() =>
                            void flatten(
                              position.id,
                              `${position.side} ${position.quantity} ${position.symbol}`,
                            )
                          }
                        >
                          {busy ? "Closing…" : "Confirm close"}
                        </button>
                        <button onClick={() => setConfirming(null)} disabled={busy}>
                          Keep
                        </button>
                      </span>
                    ) : (
                      <button onClick={() => setConfirming(position.id)}>Flatten</button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>

      {confirming ? (
        <Notice tone="warn">
          Flattening closes the position at the current market price, crossing the spread.
          It passes through the ordinary order pipeline, so it is refused if the market is
          closed or the feed is untrusted — Flatten is an order, not an override.
        </Notice>
      ) : null}

      <div className="grid cols-2">
        <Panel title="Exposure by instrument" flush>
          {Object.keys(portfolio?.exposure_by_instrument ?? {}).length === 0 ? (
            <Empty>No exposure.</Empty>
          ) : (
            <table>
              <thead>
                <tr>
                  <th>Instrument</th>
                  <th className="right">Gross exposure</th>
                </tr>
              </thead>
              <tbody>
                {Object.entries(portfolio?.exposure_by_instrument ?? {}).map(
                  ([instrument, value]) => (
                    <tr key={instrument}>
                      <td>{instrument}</td>
                      <td className="mono right">{money(value, currency)}</td>
                    </tr>
                  ),
                )}
              </tbody>
            </table>
          )}
        </Panel>

        <Panel
          title="Exposure by currency"
          note="a long gold position is also a short dollar position"
          flush
        >
          {Object.keys(portfolio?.exposure_by_currency ?? {}).length === 0 ? (
            <Empty>No currency exposure.</Empty>
          ) : (
            <table>
              <thead>
                <tr>
                  <th>Currency</th>
                  <th className="right">Net exposure</th>
                  <th>Direction</th>
                </tr>
              </thead>
              <tbody>
                {Object.entries(portfolio?.exposure_by_currency ?? {}).map(
                  ([code, value]) => (
                    <tr key={code}>
                      <td>{code}</td>
                      <td className={`mono right ${tone(value)}`}>
                        {signed(value, currency)}
                      </td>
                      <td>
                        <Status
                          value={value.startsWith("-") ? "short" : "long"}
                          tone="info"
                        />
                      </td>
                    </tr>
                  ),
                )}
              </tbody>
            </table>
          )}
        </Panel>
      </div>
    </>
  );
}
