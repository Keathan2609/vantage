"use client";

/**
 * Order ticket.
 *
 * Three things here are deliberate and load-bearing:
 *
 * 1.  A pre-submission review that states, in the account's own currency, what
 *     the order costs in margin and what it risks if the stop is hit. A ticket
 *     that shows only lot size hides the only number that matters on a small
 *     account.
 *
 * 2.  Double-click cannot duplicate an order. The idempotency key is minted
 *     ONCE when the review opens, so every submission of that reviewed ticket
 *     carries the same key. A second click returns the first order.
 *
 * 3.  Refusals are rendered in full, with every failing risk check and its
 *     limit and observed value. "Order rejected" alone is useless; the
 *     operator needs to know which ceiling bound and by how much.
 */

import { useMemo, useState } from "react";

import { Empty, Notice, Panel, Status } from "@/components/ui";
import {
  ApiError,
  api,
  newIdempotencyKey,
  type PlaceOrderResult,
  type RiskCheck,
} from "@/lib/api";
import { decimal, humanise, money, percent } from "@/lib/format";
import { useAsync, useVantage } from "@/lib/store";

type Side = "buy" | "sell";

export default function TradePage() {
  const { account, portfolio, quotes, health, marketStatus, refresh } = useVantage();
  const instruments = useAsync(() => api.instruments(true), []);
  const authority = useAsync(
    () => (account ? api.authority(account.id) : Promise.resolve(null)),
    [account?.id],
  );
  const risk = useAsync(
    () => (account ? api.riskLimits(account.id) : Promise.resolve(null)),
    [account?.id],
  );

  const allowed = authority.data?.authority?.allowed_instruments ?? [];
  const tradable = (instruments.data?.instruments ?? []).filter((i) =>
    allowed.length === 0 ? true : allowed.includes(i.id),
  );

  const [instrumentId, setInstrumentId] = useState<string>("");
  const [side, setSide] = useState<Side>("buy");
  const [orderType, setOrderType] = useState("market");
  const [quantity, setQuantity] = useState("0.01");
  const [limitPrice, setLimitPrice] = useState("");
  const [stopPrice, setStopPrice] = useState("");
  const [stopLoss, setStopLoss] = useState("");
  const [takeProfit, setTakeProfit] = useState("");
  const [timeInForce, setTimeInForce] = useState("gtc");

  const [review, setReview] = useState<{ key: string } | null>(null);
  const [result, setResult] = useState<PlaceOrderResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const selected = useMemo(
    () => tradable.find((i) => i.id === instrumentId) ?? tradable[0],
    [tradable, instrumentId],
  );
  const quote = selected ? quotes[selected.id] : undefined;
  const feed = selected ? health[selected.id] : undefined;
  const currency = portfolio?.currency ?? account?.currency ?? "ZAR";

  // The estimate uses floats: it is a pre-trade indication rendered on screen,
  // never an amount that reaches the ledger. The server recomputes everything
  // in decimal before accepting the order.
  const estimate = useMemo(() => {
    if (!selected || !quote) return null;
    const price = Number.parseFloat(side === "buy" ? quote.ask : quote.bid);
    const qty = Number.parseFloat(quantity);
    const contract = Number.parseFloat(selected.contract_size);
    const marginRate = Number.parseFloat(selected.margin_rate);
    if (![price, qty, contract, marginRate].every(Number.isFinite) || qty <= 0) return null;

    const notionalQuote = qty * contract * price;
    const marginQuote = notionalQuote * marginRate;

    // Quote currency to account currency, from the live USDZAR cross when the
    // two differ. Missing that rate is exactly why the server refuses such an
    // order rather than guessing.
    let rate = 1;
    let rateKnown = selected.quote_currency === currency;
    if (!rateKnown) {
      const cross = quotes[`${selected.quote_currency}${currency}`];
      if (cross) {
        rate = Number.parseFloat(cross.mid);
        rateKnown = Number.isFinite(rate);
      }
    }

    const stopValue = Number.parseFloat(stopLoss);
    const riskQuote =
      Number.isFinite(stopValue) && stopValue > 0
        ? Math.abs(price - stopValue) * qty * contract
        : null;

    const equity = Number.parseFloat(portfolio?.equity ?? "0");

    return {
      price,
      notional: rateKnown ? notionalQuote * rate : null,
      margin: rateKnown ? marginQuote * rate : null,
      risk: riskQuote !== null && rateKnown ? riskQuote * rate : null,
      riskFraction:
        riskQuote !== null && rateKnown && equity > 0
          ? (riskQuote * rate) / equity
          : null,
      rateKnown,
      quoteCurrency: selected.quote_currency,
    };
  }, [selected, quote, side, quantity, stopLoss, quotes, currency, portfolio?.equity]);

  const openReview = () => {
    setError(null);
    setResult(null);
    // The key is minted here, once, and reused for every submission of this
    // reviewed ticket. That is what makes a double-click harmless.
    setReview({ key: newIdempotencyKey("ui") });
  };

  const submit = async () => {
    if (!account || !selected || !review) return;
    setBusy(true);
    setError(null);
    try {
      const response = await api.placeOrder(
        {
          account_id: account.id,
          instrument_id: selected.id,
          side,
          type: orderType,
          quantity,
          ...(orderType === "limit" || orderType === "stop_limit"
            ? { limit_price: limitPrice }
            : {}),
          ...(orderType === "stop" || orderType === "stop_limit"
            ? { stop_price: stopPrice }
            : {}),
          ...(stopLoss ? { stop_loss: stopLoss } : {}),
          ...(takeProfit ? { take_profit: takeProfit } : {}),
          time_in_force: timeInForce,
        },
        review.key,
      );
      setResult(response);
      setReview(null);
      refresh();
    } catch (cause) {
      if (cause instanceof ApiError) {
        setError(`${cause.code}: ${cause.message}`);
      } else {
        setError(cause instanceof Error ? cause.message : "The order could not be placed");
      }
    } finally {
      setBusy(false);
    }
  };

  const suggestStop = () => {
    if (!quote || !selected) return;
    const price = Number.parseFloat(side === "buy" ? quote.ask : quote.bid);
    const limits = risk.data?.limits;
    const equity = Number.parseFloat(portfolio?.equity ?? "0");
    const qty = Number.parseFloat(quantity);
    const contract = Number.parseFloat(selected.contract_size);
    if (!Number.isFinite(price) || !limits || equity <= 0) return;

    let rate = 1;
    if (selected.quote_currency !== currency) {
      const cross = quotes[`${selected.quote_currency}${currency}`];
      if (!cross) return;
      rate = Number.parseFloat(cross.mid);
    }
    // Place the stop exactly at the account's per-trade risk ceiling for the
    // chosen size, so the ticket proposes the widest stop the limits allow.
    const budget = equity * Number.parseFloat(limits.max_risk_per_trade_fraction);
    const distance = budget / (qty * contract * rate);
    const target = side === "buy" ? price - distance : price + distance;
    if (target > 0) {
      setStopLoss(target.toFixed(selected.price_precision));
    }
  };

  if (instruments.loading || authority.loading) {
    return <Empty>Loading instruments…</Empty>;
  }
  if (!account) {
    return <Empty>No account selected.</Empty>;
  }
  if (tradable.length === 0) {
    return (
      <Notice tone="warn">
        This account&rsquo;s trading authority does not cover any enabled instrument, so
        there is nothing to trade. Grant or widen the mandate on the Authority page.
      </Notice>
    );
  }

  return (
    <>
      <div className="grid sidebar-right">
        <div>
          <Panel
            title="Order ticket"
            note={`${account.name} · paper · ${account.broker_name}`}
          >
            {marketStatus && !marketStatus.tradable ? (
              <Notice tone="warn">
                The market is {humanise(marketStatus.status)}. This order will be refused
                until it reopens.
              </Notice>
            ) : null}
            {feed && !feed.tradable_by_automation ? (
              <Notice tone="warn">
                The {selected?.symbol} feed is {feed.state}
                {feed.issues.length ? ` (${feed.issues.join(", ")})` : ""}. A manual order
                may still be accepted on a degraded feed; automation will not act on one.
              </Notice>
            ) : null}

            <div className="field">
              <label htmlFor="instrument">Instrument</label>
              <select
                id="instrument"
                value={selected?.id ?? ""}
                onChange={(event) => {
                  setInstrumentId(event.target.value);
                  setReview(null);
                  setStopLoss("");
                  setTakeProfit("");
                }}
              >
                {tradable.map((instrument) => (
                  <option key={instrument.id} value={instrument.id}>
                    {instrument.symbol} — {instrument.name}
                  </option>
                ))}
              </select>
            </div>

            <div className="field">
              <label>Side</label>
              <div className="segmented">
                <button
                  type="button"
                  data-variant="buy"
                  data-selected={side === "buy"}
                  onClick={() => {
                    setSide("buy");
                    setReview(null);
                  }}
                >
                  Buy
                </button>
                <button
                  type="button"
                  data-variant="sell"
                  data-selected={side === "sell"}
                  onClick={() => {
                    setSide("sell");
                    setReview(null);
                  }}
                >
                  Sell
                </button>
              </div>
            </div>

            <div className="field-row">
              <div className="field">
                <label htmlFor="type">Order type</label>
                <select
                  id="type"
                  value={orderType}
                  onChange={(event) => {
                    setOrderType(event.target.value);
                    setReview(null);
                  }}
                >
                  {(selected?.supported_order_types ?? ["market"]).map((type) => (
                    <option key={type} value={type}>
                      {humanise(type)}
                    </option>
                  ))}
                </select>
              </div>
              <div className="field">
                <label htmlFor="tif">Time in force</label>
                <select
                  id="tif"
                  value={timeInForce}
                  onChange={(event) => setTimeInForce(event.target.value)}
                >
                  <option value="gtc">Good till cancelled</option>
                  <option value="ioc">Immediate or cancel</option>
                  <option value="day">Day</option>
                </select>
              </div>
            </div>

            <div className="field">
              <label htmlFor="quantity">
                Quantity (lots · {selected ? decimal(selected.contract_size, 0) : "?"} units
                per lot · step {selected?.quantity_step})
              </label>
              <input
                id="quantity"
                value={quantity}
                onChange={(event) => {
                  setQuantity(event.target.value);
                  setReview(null);
                }}
                inputMode="decimal"
              />
            </div>

            {orderType === "limit" || orderType === "stop_limit" ? (
              <div className="field">
                <label htmlFor="limit">Limit price</label>
                <input
                  id="limit"
                  value={limitPrice}
                  onChange={(event) => setLimitPrice(event.target.value)}
                  inputMode="decimal"
                />
              </div>
            ) : null}

            {orderType === "stop" || orderType === "stop_limit" ? (
              <div className="field">
                <label htmlFor="stopPrice">Stop trigger price</label>
                <input
                  id="stopPrice"
                  value={stopPrice}
                  onChange={(event) => setStopPrice(event.target.value)}
                  inputMode="decimal"
                />
              </div>
            ) : null}

            <div className="field-row">
              <div className="field">
                <label htmlFor="stopLoss">
                  Stop loss{risk.data?.limits.require_stop_loss ? " (required)" : ""}
                </label>
                <input
                  id="stopLoss"
                  value={stopLoss}
                  onChange={(event) => {
                    setStopLoss(event.target.value);
                    setReview(null);
                  }}
                  inputMode="decimal"
                />
              </div>
              <div className="field">
                <label htmlFor="takeProfit">Take profit</label>
                <input
                  id="takeProfit"
                  value={takeProfit}
                  onChange={(event) => setTakeProfit(event.target.value)}
                  inputMode="decimal"
                />
              </div>
            </div>

            <div className="button-row">
              <button type="button" onClick={suggestStop} disabled={!quote}>
                Stop at risk limit
              </button>
              {!review ? (
                <button type="button" data-variant="primary" onClick={openReview}>
                  Review order
                </button>
              ) : (
                <>
                  <button
                    type="button"
                    data-variant={side === "buy" ? "buy" : "sell"}
                    data-selected="true"
                    onClick={() => void submit()}
                    disabled={busy}
                  >
                    {busy
                      ? "Submitting…"
                      : `Confirm ${side} ${quantity} ${selected?.symbol ?? ""}`}
                  </button>
                  <button type="button" onClick={() => setReview(null)} disabled={busy}>
                    Cancel
                  </button>
                </>
              )}
            </div>

            {review ? (
              <Notice>
                Reviewed. This ticket carries one idempotency key, so submitting it twice
                returns the same order rather than placing a second.
              </Notice>
            ) : null}
          </Panel>

          {error ? <Notice tone="bad">{error}</Notice> : null}
          {result ? <OrderOutcome result={result} currency={currency} /> : null}
        </div>

        <div>
          <Panel title="Pre-trade estimate" flush>
            <div className="panel-body">
              {!quote ? (
                <Empty>No price for {selected?.symbol}.</Empty>
              ) : (
                <>
                  <dl className="kv">
                    <dt>Bid / Ask</dt>
                    <dd>
                      {decimal(quote.bid, selected?.price_precision ?? 2)} /{" "}
                      {decimal(quote.ask, selected?.price_precision ?? 2)}
                    </dd>
                    <dt>Spread</dt>
                    <dd>{percent(quote.spread_fraction, 4)}</dd>
                    <dt>Execution price</dt>
                    <dd>
                      {decimal(
                        side === "buy" ? quote.ask : quote.bid,
                        selected?.price_precision ?? 2,
                      )}
                    </dd>
                    <dt>Notional</dt>
                    <dd>
                      {estimate?.notional !== null && estimate?.notional !== undefined
                        ? money(estimate.notional.toFixed(2), currency)
                        : "unavailable"}
                    </dd>
                    <dt>Margin</dt>
                    <dd>
                      {estimate?.margin !== null && estimate?.margin !== undefined
                        ? money(estimate.margin.toFixed(2), currency)
                        : "unavailable"}
                    </dd>
                    <dt>Risk if stopped</dt>
                    <dd className={estimate?.risk ? "loss" : ""}>
                      {estimate?.risk !== null && estimate?.risk !== undefined
                        ? money(estimate.risk.toFixed(2), currency)
                        : "set a stop loss"}
                    </dd>
                    <dt>As % of equity</dt>
                    <dd className={estimate?.riskFraction ? "loss" : ""}>
                      {estimate?.riskFraction !== null &&
                      estimate?.riskFraction !== undefined
                        ? `${(estimate.riskFraction * 100).toFixed(2)}%`
                        : "—"}
                    </dd>
                    <dt>Free margin after</dt>
                    <dd>
                      {estimate?.margin !== null && estimate?.margin !== undefined
                        ? money(
                            (
                              Number.parseFloat(portfolio?.free_margin ?? "0") -
                              estimate.margin
                            ).toFixed(2),
                            currency,
                          )
                        : "—"}
                    </dd>
                  </dl>

                  {estimate && !estimate.rateKnown ? (
                    <Notice tone="warn">
                      No {estimate.quoteCurrency}/{currency} rate is available, so this order
                      cannot be valued in the account&rsquo;s currency. The server will refuse
                      it rather than guess.
                    </Notice>
                  ) : null}

                  {estimate?.riskFraction !== null &&
                  estimate?.riskFraction !== undefined &&
                  risk.data &&
                  estimate.riskFraction >
                    Number.parseFloat(risk.data.limits.max_risk_per_trade_fraction) ? (
                    <Notice tone="bad">
                      This risks {(estimate.riskFraction * 100).toFixed(2)}% of equity, above
                      the account ceiling of{" "}
                      {percent(risk.data.limits.max_risk_per_trade_fraction)}. It will be
                      refused.
                    </Notice>
                  ) : null}

                  <p className="tiny muted" style={{ marginBottom: 0 }}>
                    Estimates only. The control plane recomputes every figure in exact
                    decimal, converts through a recorded FX rate, and applies each risk check
                    before accepting an order.
                  </p>
                </>
              )}
            </div>
          </Panel>

          <Panel title="Instrument specification" flush>
            {selected ? (
              <div className="panel-body">
                <dl className="kv">
                  <dt>Symbol</dt>
                  <dd>{selected.symbol}</dd>
                  <dt>Contract size</dt>
                  <dd>
                    {decimal(selected.contract_size, 0)} {selected.base_currency}
                  </dd>
                  <dt>Quoted in</dt>
                  <dd>{selected.quote_currency}</dd>
                  <dt>Min / step</dt>
                  <dd>
                    {selected.min_quantity} / {selected.quantity_step}
                  </dd>
                  <dt>Tick size</dt>
                  <dd>{selected.tick_size}</dd>
                  <dt>Margin rate</dt>
                  <dd>{percent(selected.margin_rate)}</dd>
                  <dt>Max leverage</dt>
                  <dd>{decimal(selected.max_leverage, 0)}x</dd>
                  <dt>Commission</dt>
                  <dd>
                    {selected.commission_per_lot} {selected.quote_currency}/lot
                  </dd>
                </dl>
              </div>
            ) : (
              <Empty>No instrument selected.</Empty>
            )}
          </Panel>
        </div>
      </div>
    </>
  );
}

function OrderOutcome({
  result,
  currency,
}: {
  result: PlaceOrderResult;
  currency: string;
}) {
  if (result.rejection) {
    const failed = (result.risk?.checks ?? []).filter((c) => !c.passed);
    return (
      <Panel
        title="Order refused"
        note={result.duplicate ? "returned from a previous identical command" : undefined}
      >
        <Notice tone="bad">
          <strong>{humanise(result.rejection.code)}</strong> — {result.rejection.message}
        </Notice>
        {failed.length > 0 ? (
          <>
            <p className="small muted">
              Every check runs, so all failures are listed rather than only the first:
            </p>
            <CheckTable checks={failed} />
          </>
        ) : null}
      </Panel>
    );
  }

  if (!result.order) return null;

  return (
    <Panel
      title={result.duplicate ? "Existing order returned" : "Order accepted"}
      note={
        result.duplicate
          ? "this command had already been processed; no second order was placed"
          : undefined
      }
    >
      <Notice tone="ok">
        {result.order.status} · {result.order.side} {result.order.quantity}{" "}
        {result.order.symbol}
        {Number.parseFloat(result.order.filled_quantity) > 0
          ? ` · filled ${result.order.filled_quantity} at ${result.order.avg_fill_price}`
          : ""}
        {" · simulated"}
      </Notice>

      {(result.fills ?? []).length > 0 ? (
        <table>
          <thead>
            <tr>
              <th>Fill</th>
              <th className="right">Quantity</th>
              <th className="right">Price</th>
              <th className="right">Commission</th>
              <th className="right">Executed</th>
            </tr>
          </thead>
          <tbody>
            {(result.fills ?? []).map((fill) => (
              <tr key={fill.id}>
                <td className="mono muted">{fill.id.slice(0, 8)}</td>
                <td className="mono right">{fill.quantity}</td>
                <td className="mono right">{fill.price}</td>
                <td className="mono right">
                  {fill.commission} {fill.commission_currency}
                </td>
                <td className="mono right muted">
                  {new Date(fill.executed_at).toLocaleTimeString("en-GB")}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}

      {result.risk?.checks?.length ? (
        <details style={{ marginTop: 10 }}>
          <summary className="small muted" style={{ cursor: "pointer" }}>
            {result.risk.checks.length} risk checks evaluated
          </summary>
          <div style={{ marginTop: 8 }}>
            <CheckTable checks={result.risk.checks} />
          </div>
        </details>
      ) : null}
      <p className="tiny muted" style={{ marginTop: 8, marginBottom: 0 }}>
        Simulated execution against the mock venue. Balances are denominated in {currency}.
      </p>
    </Panel>
  );
}

function CheckTable({ checks }: { checks: RiskCheck[] }) {
  return (
    <table>
      <thead>
        <tr>
          <th>Check</th>
          <th>Result</th>
          <th className="right">Limit</th>
          <th className="right">Observed</th>
        </tr>
      </thead>
      <tbody>
        {checks.map((check) => (
          <tr key={check.name} title={check.message}>
            <td>{humanise(check.name)}</td>
            <td>
              <Status
                value={check.passed ? "passed" : "failed"}
                tone={check.passed ? "ok" : "bad"}
              />
            </td>
            <td className="mono right muted">{check.limit}</td>
            <td className={`mono right ${check.passed ? "" : "loss"}`}>{check.observed}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
