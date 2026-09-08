"use client";

import { Empty, Panel, Sparkline, Stat, StatStrip } from "@/components/ui";
import { api } from "@/lib/api";
import {
  dateTime,
  decimal,
  humanise,
  money,
  percent,
  percentValue,
  signed,
  tone,
} from "@/lib/format";
import { useAsync, useVantage } from "@/lib/store";

export default function PortfolioPage() {
  const { account, portfolio } = useVantage();
  const accountId = account?.id;

  const equity = useAsync(
    () => (accountId ? api.equityHistory(accountId, 500) : Promise.resolve(null)),
    [accountId],
  );
  const transactions = useAsync(
    () => (accountId ? api.transactions(accountId, 120) : Promise.resolve(null)),
    [accountId],
  );
  const attribution = useAsync(
    () => (accountId ? api.attribution(accountId) : Promise.resolve(null)),
    [accountId],
  );

  const currency = portfolio?.currency ?? account?.currency ?? "ZAR";
  const points = equity.data?.points ?? [];
  const series = points.map((point) => Number.parseFloat(point.equity));

  return (
    <>
      <Panel title="Account state" note="derived from the ledger, not stored as a total" flush>
        <StatStrip>
          <Stat label="Balance" value={money(portfolio?.balance, currency)} />
          <Stat
            label="Equity"
            value={money(portfolio?.equity, currency)}
            sub={"balance plus open P&L"}
          />
          <Stat
            label={"Realised P&L"}
            value={signed(portfolio?.realized_pnl, currency)}
            tone={tone(portfolio?.realized_pnl)}
          />
          <Stat
            label={"Unrealised P&L"}
            value={signed(portfolio?.unrealized_pnl, currency)}
            tone={tone(portfolio?.unrealized_pnl)}
          />
          <Stat
            label="Commission"
            value={signed(portfolio?.commission, currency)}
            tone={tone(portfolio?.commission)}
          />
          <Stat
            label="Swap"
            value={signed(portfolio?.swap, currency)}
            tone={tone(portfolio?.swap)}
            sub="overnight financing"
          />
        </StatStrip>
      </Panel>

      <div className="grid cols-2">
        <Panel
          title="Equity curve"
          note={points.length ? `${points.length} samples` : undefined}
        >
          {series.length < 2 ? (
            <Empty>
              Not enough history to plot. Equity is sampled after every state change that
              can move it.
            </Empty>
          ) : (
            <>
              <div style={{ height: 90 }}>
                <Sparkline values={series} height={90} />
              </div>
              <dl className="kv" style={{ marginTop: 10 }}>
                <dt>First sample</dt>
                <dd>{dateTime(points[0]?.recorded_at)}</dd>
                <dt>Latest sample</dt>
                <dd>{dateTime(points[points.length - 1]?.recorded_at)}</dd>
                <dt>Peak equity</dt>
                <dd>{money(portfolio?.peak_equity, currency)}</dd>
                <dt>Drawdown from peak</dt>
                <dd className={Number.parseFloat(portfolio?.drawdown_fraction ?? "0") > 0 ? "loss" : ""}>
                  {percent(portfolio?.drawdown_fraction)}
                </dd>
              </dl>
            </>
          )}
        </Panel>

        <Panel title="Margin" flush>
          <div className="panel-body">
            <dl className="kv">
              <dt>Margin used</dt>
              <dd>{money(portfolio?.margin_used, currency)}</dd>
              <dt>Free margin</dt>
              <dd>{money(portfolio?.free_margin, currency)}</dd>
              <dt>Margin level</dt>
              <dd>{percentValue(portfolio?.margin_level_pct)}</dd>
              <dt>Gross exposure</dt>
              <dd>{money(portfolio?.gross_exposure, currency)}</dd>
              <dt>Net exposure</dt>
              <dd className={tone(portfolio?.net_exposure)}>
                {signed(portfolio?.net_exposure, currency)}
              </dd>
              <dt>Open positions</dt>
              <dd>{portfolio?.open_positions ?? 0}</dd>
              <dt>Pending orders</dt>
              <dd>{portfolio?.pending_orders ?? 0}</dd>
              <dt>Leverage in use</dt>
              <dd>
                {portfolio && Number.parseFloat(portfolio.equity) > 0
                  ? `${(
                      Number.parseFloat(portfolio.gross_exposure) /
                      Number.parseFloat(portfolio.equity)
                    ).toFixed(2)}x`
                  : "—"}
              </dd>
            </dl>
            <p className="tiny muted" style={{ marginTop: 8, marginBottom: 0 }}>
              Positions are marked at the price they could be CLOSED at — a long at the bid,
              a short at the ask. Marking at the mid would overstate equity by half a spread
              on every open position.
            </p>
          </div>
        </Panel>
      </div>

      <Panel
        title={"P&L attribution by instrument"}
        note="where the money actually came from"
        flush
      >
        {attribution.loading ? (
          <Empty>Loading…</Empty>
        ) : (attribution.data?.attribution ?? []).length === 0 ? (
          <Empty>No closed positions yet, so there is nothing to attribute.</Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Instrument</th>
                <th className="right">Realised P&amp;L</th>
                <th className="right">Trades</th>
                <th className="right">Win rate</th>
              </tr>
            </thead>
            <tbody>
              {(attribution.data?.attribution ?? []).map((row) => (
                <tr key={row.key}>
                  <td>{row.label}</td>
                  <td className={`mono right ${tone(row.realized_pnl)}`}>
                    {signed(row.realized_pnl, currency)}
                  </td>
                  <td className="mono right">{row.trades}</td>
                  <td className="mono right muted">{percent(row.win_rate)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>

      <Panel
        title="Ledger"
        note="append-only; the balance is the sum of these entries"
        flush
      >
        {transactions.loading ? (
          <Empty>Loading ledger…</Empty>
        ) : (transactions.data?.transactions ?? []).length === 0 ? (
          <Empty>No ledger entries.</Empty>
        ) : (
          <div className="table-scroll tall">
            <table>
              <thead>
                <tr>
                  <th className="right">Seq</th>
                  <th>Type</th>
                  <th>Description</th>
                  <th className="right">Amount</th>
                  <th className="right">Balance after</th>
                  <th className="right">When</th>
                </tr>
              </thead>
              <tbody>
                {(transactions.data?.transactions ?? []).map((entry) => (
                  <tr key={entry.id}>
                    <td className="mono right muted">{entry.sequence}</td>
                    <td>{humanise(entry.type)}</td>
                    <td className="muted truncate" title={entry.description}>
                      {entry.description}
                    </td>
                    <td className={`mono right ${tone(entry.amount)}`}>
                      {signed(entry.amount, entry.currency)}
                    </td>
                    <td className="mono right">{decimal(entry.balance_after, 2)}</td>
                    <td className="mono right muted">{dateTime(entry.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <div className="panel-body">
          <p className="tiny muted" style={{ margin: 0 }}>
            The ledger is append-only and enforced as such by the database: the application
            role may INSERT but has no UPDATE or DELETE privilege, and a trigger refuses both
            regardless. Sequence numbers are contiguous per account, so a gap would itself be
            evidence.
          </p>
        </div>
      </Panel>
    </>
  );
}
