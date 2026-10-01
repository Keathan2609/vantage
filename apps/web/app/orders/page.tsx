"use client";

import { useState } from "react";

import { Empty, Notice, Panel, Status } from "@/components/ui";
import { ApiError, api, type Order } from "@/lib/api";
import { decimal, dateTime, humanise, relative } from "@/lib/format";
import { useAsync, useVantage } from "@/lib/store";

export default function OrdersPage() {
  const { account, refresh } = useVantage();
  const [openOnly, setOpenOnly] = useState(false);
  const [selected, setSelected] = useState<string | null>(null);
  const [message, setMessage] = useState<{ tone: "ok" | "bad"; text: string } | null>(null);

  const orders = useAsync(
    () =>
      account
        ? api.orders({ accountId: account.id, openOnly, limit: 200 })
        : Promise.resolve(null),
    [account?.id, openOnly],
  );

  const detail = useAsync(
    () => (selected ? api.order(selected) : Promise.resolve(null)),
    [selected],
  );

  const cancel = async (order: Order) => {
    setMessage(null);
    try {
      await api.cancelOrder(order.id);
      setMessage({ tone: "ok", text: `Order ${order.id.slice(0, 8)} cancelled.` });
      orders.reload();
      refresh();
    } catch (cause) {
      setMessage({
        tone: "bad",
        text:
          cause instanceof ApiError
            ? `${cause.code}: ${cause.message}`
            : "The order could not be cancelled",
      });
    }
  };

  const rows = orders.data?.orders ?? [];

  return (
    <>
      {message ? <Notice tone={message.tone}>{message.text}</Notice> : null}

      <Panel
        title="Orders"
        note={`${rows.length} shown · simulated`}
        actions={
          <div className="segmented">
            <button data-selected={!openOnly} onClick={() => setOpenOnly(false)}>
              All
            </button>
            <button data-selected={openOnly} onClick={() => setOpenOnly(true)}>
              Open only
            </button>
          </div>
        }
        flush
      >
        {orders.loading ? (
          <Empty>Loading orders…</Empty>
        ) : rows.length === 0 ? (
          <Empty>No orders on this account yet.</Empty>
        ) : (
          <div className="table-scroll tall">
            <table>
              <thead>
                <tr>
                  <th>Created</th>
                  <th>Instrument</th>
                  <th>Side</th>
                  <th>Type</th>
                  <th className="right">Quantity</th>
                  <th className="right">Filled</th>
                  <th className="right">Avg fill</th>
                  <th>Status</th>
                  <th>Source</th>
                  <th>Reason</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {rows.map((order) => (
                  <tr
                    key={order.id}
                    onClick={() => setSelected(order.id)}
                    style={{ cursor: "pointer" }}
                  >
                    <td className="mono muted" title={dateTime(order.created_at)}>
                      {relative(order.created_at)}
                    </td>
                    <td>{order.symbol}</td>
                    <td>
                      <span className="badge" data-tone={order.side === "buy" ? "ok" : "bad"}>
                        {order.side}
                      </span>
                    </td>
                    <td className="muted">{humanise(order.type)}</td>
                    <td className="mono right">{order.quantity}</td>
                    <td className="mono right">{order.filled_quantity}</td>
                    <td className="mono right">
                      {Number.parseFloat(order.avg_fill_price) > 0
                        ? decimal(order.avg_fill_price, 2)
                        : "-"}
                    </td>
                    <td>
                      <Status value={order.status.toLowerCase()} label={order.status} />
                    </td>
                    <td className="muted tiny">{order.source}</td>
                    <td className="muted truncate" title={order.reject_reason}>
                      {order.reject_code ? humanise(order.reject_code) : ""}
                    </td>
                    <td className="right">
                      {["ACCEPTED", "SUBMITTED", "PARTIALLY_FILLED"].includes(order.status) ? (
                        <button
                          onClick={(event) => {
                            event.stopPropagation();
                            void cancel(order);
                          }}
                        >
                          Cancel
                        </button>
                      ) : null}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>

      {selected ? (
        <Panel
          title="Order detail"
          note="fills and the full state-transition history"
          actions={<button onClick={() => setSelected(null)}>Close</button>}
        >
          {detail.loading ? (
            <Empty>Loading…</Empty>
          ) : !detail.data ? (
            <Empty>Order not found.</Empty>
          ) : (
            <div className="grid cols-2">
              <div>
                <dl className="kv">
                  <dt>Order</dt>
                  <dd>{detail.data.order.id}</dd>
                  <dt>Status</dt>
                  <dd>{detail.data.order.status}</dd>
                  <dt>Instrument</dt>
                  <dd>{detail.data.order.symbol}</dd>
                  <dt>Side / type</dt>
                  <dd>
                    {detail.data.order.side} {humanise(detail.data.order.type)}
                  </dd>
                  <dt>Quantity</dt>
                  <dd>{detail.data.order.quantity}</dd>
                  <dt>Filled</dt>
                  <dd>
                    {detail.data.order.filled_quantity} @{" "}
                    {decimal(detail.data.order.avg_fill_price, 2)}
                  </dd>
                  <dt>Stop / target</dt>
                  <dd>
                    {detail.data.order.stop_loss ?? "none"} /{" "}
                    {detail.data.order.take_profit ?? "-"}
                  </dd>
                  <dt>Broker order</dt>
                  <dd>{detail.data.order.broker_order_id ?? "not assigned"}</dd>
                  <dt>Source</dt>
                  <dd>{detail.data.order.source}</dd>
                  <dt>Mode</dt>
                  <dd>{detail.data.order.mode} (simulated)</dd>
                </dl>
                {detail.data.order.reject_reason ? (
                  <Notice tone="bad">
                    <strong>{humanise(detail.data.order.reject_code)}</strong> --{" "}
                    {detail.data.order.reject_reason}
                  </Notice>
                ) : null}
              </div>

              <div>
                <div className="panel-title" style={{ marginBottom: 6 }}>
                  Fills
                </div>
                {detail.data.fills.length === 0 ? (
                  <Empty>No fills.</Empty>
                ) : (
                  <table>
                    <thead>
                      <tr>
                        <th className="right">Quantity</th>
                        <th className="right">Price</th>
                        <th className="right">Commission</th>
                        <th className="right">Executed</th>
                      </tr>
                    </thead>
                    <tbody>
                      {detail.data.fills.map((fill) => (
                        <tr key={fill.id}>
                          <td className="mono right">{fill.quantity}</td>
                          <td className="mono right">{decimal(fill.price, 2)}</td>
                          <td className="mono right">
                            {fill.commission} {fill.commission_currency}
                          </td>
                          <td className="mono right muted">{dateTime(fill.executed_at)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}

                <div className="panel-title" style={{ margin: "14px 0 6px" }}>
                  State transitions
                </div>
                <table>
                  <thead>
                    <tr>
                      <th>From</th>
                      <th>To</th>
                      <th>Reason</th>
                      <th className="right">When</th>
                    </tr>
                  </thead>
                  <tbody>
                    {detail.data.transitions.map((transition, index) => (
                      <tr key={`${transition.OccurredAt}-${index}`}>
                        <td className="muted">{transition.FromStatus ?? "-"}</td>
                        <td>{transition.ToStatus}</td>
                        <td className="muted truncate" title={transition.Reason ?? ""}>
                          {transition.Reason ?? ""}
                        </td>
                        <td className="mono right muted">
                          {dateTime(transition.OccurredAt)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                <p className="tiny muted" style={{ marginTop: 8 }}>
                  Illegal transitions are impossible: the state machine refuses them and the
                  attempt is recorded rather than applied.
                </p>
              </div>
            </div>
          )}
        </Panel>
      ) : null}
    </>
  );
}
