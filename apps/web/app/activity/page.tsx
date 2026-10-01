"use client";

/**
 * Activity, decisions and the audit trail.
 *
 * Three different records, kept visibly distinct because they answer different
 * questions:
 *
 *   activity  -- what happened, in one stream, for a human catching up
 *   decisions -- why an order was or was not placed, with its inputs
 *   audit     -- the hash-chained record of who did what, for evidence
 *
 * The audit panel shows the chain link explicitly. It is tamper *evidence*:
 * altering a row breaks every hash after it, which is detectable. It is not
 * immutability, and the page says so rather than implying more than the
 * mechanism provides.
 */

import { useState } from "react";

import { Empty, Notice, Panel, Status } from "@/components/ui";
import { api } from "@/lib/api";
import { decimal, fullDateTime, humanise, relative, shortId } from "@/lib/format";
import { useAsync, useVantage } from "@/lib/store";

type Tab = "activity" | "decisions" | "audit";

export default function ActivityPage() {
  const { account } = useVantage();
  const accountId = account?.id;
  const [tab, setTab] = useState<Tab>("activity");

  const activity = useAsync(
    () => (accountId ? api.activity(accountId, 120) : Promise.resolve(null)),
    [accountId],
  );
  const decisions = useAsync(
    () => (accountId ? api.decisions(accountId, 80) : Promise.resolve(null)),
    [accountId],
  );
  const audit = useAsync(() => api.audit(150), []);
  const notifications = useAsync(() => api.notifications(), []);

  const items = activity.data?.activity ?? [];
  const decisionRows = decisions.data?.decisions ?? [];
  const auditRows = audit.data?.events ?? [];
  const alerts = notifications.data?.notifications ?? [];

  // Sequence numbers should be contiguous. A gap means rows were deleted, which
  // the chain hash alone would not reveal if the deleted rows were at the tail.
  const gaps: number[] = [];
  for (let index = 1; index < auditRows.length; index += 1) {
    const newer = auditRows[index - 1];
    const older = auditRows[index];
    if (newer && older && newer.Sequence - older.Sequence !== 1) {
      gaps.push(older.Sequence);
    }
  }

  return (
    <>
      <div className="segmented" style={{ marginBottom: 10 }}>
        <button data-selected={tab === "activity"} onClick={() => setTab("activity")}>
          Activity
        </button>
        <button data-selected={tab === "decisions"} onClick={() => setTab("decisions")}>
          Decisions
        </button>
        <button data-selected={tab === "audit"} onClick={() => setTab("audit")}>
          Audit trail
        </button>
      </div>

      {tab === "activity" ? (
        <>
          {alerts.length > 0 ? (
            <Panel title="Notifications" note={`${alerts.length}`} flush>
              <table>
                <thead>
                  <tr>
                    <th>Severity</th>
                    <th>Category</th>
                    <th>Title</th>
                    <th>Detail</th>
                    <th className="right">When</th>
                  </tr>
                </thead>
                <tbody>
                  {alerts.map((alert) => (
                    <tr key={alert.ID}>
                      <td>
                        <Status value={alert.Severity} />
                      </td>
                      <td className="muted">{humanise(alert.Category)}</td>
                      <td>{alert.Title}</td>
                      <td className="wrap muted small">{alert.Body}</td>
                      <td
                        className="mono right muted"
                        title={fullDateTime(alert.CreatedAt)}
                      >
                        {relative(alert.CreatedAt)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </Panel>
          ) : null}

          <Panel
            title="Activity"
            note="orders, fills, risk refusals, reconciliation and control actions in one stream"
            actions={<button onClick={activity.reload}>Refresh</button>}
            flush
          >
            {activity.loading ? (
              <Empty>Loading activity…</Empty>
            ) : items.length === 0 ? (
              <Empty>Nothing has happened on this account yet.</Empty>
            ) : (
              <div className="table-scroll tall">
                <table>
                  <thead>
                    <tr>
                      <th>Severity</th>
                      <th>Kind</th>
                      <th>Event</th>
                      <th>Detail</th>
                      <th>Reference</th>
                      <th className="right">When</th>
                    </tr>
                  </thead>
                  <tbody>
                    {items.map((item, index) => (
                      <tr key={`${item.at}-${index}`}>
                        <td>
                          <Status value={item.severity} />
                        </td>
                        <td className="muted">{humanise(item.kind)}</td>
                        <td>{item.title}</td>
                        <td className="wrap muted small">{item.detail}</td>
                        <td className="mono tiny muted" title={item.reference}>
                          {shortId(item.reference)}
                        </td>
                        <td className="mono right muted" title={fullDateTime(item.at)}>
                          {relative(item.at)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Panel>
        </>
      ) : null}

      {tab === "decisions" ? (
        <>
          <Notice>
            One row per decision, including the ones that produced no order. A system that
            only records what it did cannot answer the more useful question, which is why it did not
            act, and that question is the common one on a small account.
          </Notice>

          <Panel title="Decision snapshots" note={`${decisionRows.length} record(s)`} flush>
            {decisions.loading ? (
              <Empty>Loading decisions…</Empty>
            ) : decisionRows.length === 0 ? (
              <Empty>No decisions recorded.</Empty>
            ) : (
              <div className="table-scroll tall">
                <table>
                  <thead>
                    <tr>
                      <th>Instrument</th>
                      <th>Signal</th>
                      <th className="right">Confidence</th>
                      <th className="right">Requested</th>
                      <th className="right">Approved</th>
                      <th>Outcome</th>
                      <th>Reason</th>
                      <th className="right">When</th>
                    </tr>
                  </thead>
                  <tbody>
                    {decisionRows.map((decision) => {
                      const reduced =
                        decision.RequestedQty &&
                        decision.ApprovedQty &&
                        decision.ApprovedQty !== decision.RequestedQty;
                      return (
                        <tr key={decision.ID}>
                          <td>{decision.InstrumentID}</td>
                          <td>
                            <span
                              className="badge"
                              data-tone={
                                decision.SignalAction === "buy"
                                  ? "ok"
                                  : decision.SignalAction === "sell"
                                    ? "bad"
                                    : undefined
                              }
                            >
                              {humanise(decision.SignalAction)}
                            </span>
                          </td>
                          <td className="mono right muted">
                            {decimal(decision.Confidence, 2)}
                          </td>
                          <td className="mono right muted">
                            {decimal(decision.RequestedQty, 2)}
                          </td>
                          <td className={`mono right ${reduced ? "warn-text" : ""}`}>
                            {decimal(decision.ApprovedQty, 2)}
                          </td>
                          <td>
                            <Status
                              value={decision.Outcome}
                              tone={
                                decision.Outcome === "executed"
                                  ? "ok"
                                  : decision.Outcome === "rejected"
                                    ? "bad"
                                    : "warn"
                              }
                            />
                          </td>
                          <td className="wrap muted small" title={decision.OutcomeCode}>
                            {decision.OutcomeReason ?? decision.OutcomeCode ?? "-"}
                          </td>
                          <td
                            className="mono right muted"
                            title={fullDateTime(decision.CreatedAt)}
                          >
                            {relative(decision.CreatedAt)}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
          </Panel>

          <Panel title="What a snapshot contains" flush>
            <div className="panel-body">
              <p className="small muted" style={{ marginTop: 0 }}>
                The signal and its confidence, the indicator readings at decision time, the
                quote used, the risk verdict with every check that ran, the account state, and
                the outcome. Enough to reconstruct the decision without re-running it.
              </p>
              <p className="small muted" style={{ marginBottom: 0 }}>
                No secret material is ever written into a snapshot: no credentials, no tokens,
                no broker login. A snapshot is meant to be readable by anyone reviewing the
                system, which is only safe if it never contains anything that grants access.
              </p>
            </div>
          </Panel>
        </>
      ) : null}

      {tab === "audit" ? (
        <>
          <Notice>
            Each row&rsquo;s hash covers its own content <em>and</em> the previous row&rsquo;s
            hash. Editing or removing a row breaks every hash after it, so tampering becomes
            detectable. This is tamper <strong>evidence</strong>, not immutability: a
            database owner can still rewrite the table, and the chain is what makes that
            visible rather than impossible.
          </Notice>

          {gaps.length > 0 ? (
            <Notice tone="bad">
              Sequence gap after {gaps.join(", ")}. Rows are missing from this window.
            </Notice>
          ) : null}

          <Panel
            title="Audit trail"
            note={`${auditRows.length} event(s)${
              auditRows.length > 0
                ? ` · sequence ${auditRows[auditRows.length - 1]?.Sequence} to ${auditRows[0]?.Sequence}`
                : ""
            }`}
            actions={<button onClick={audit.reload}>Refresh</button>}
            flush
          >
            {audit.loading ? (
              <Empty>Loading the audit trail…</Empty>
            ) : auditRows.length === 0 ? (
              <Empty>No audit events.</Empty>
            ) : (
              <div className="table-scroll tall">
                <table>
                  <thead>
                    <tr>
                      <th className="right">Seq</th>
                      <th>Actor</th>
                      <th>Action</th>
                      <th>Target</th>
                      <th>Result</th>
                      <th>Chain hash</th>
                      <th className="right">Occurred</th>
                    </tr>
                  </thead>
                  <tbody>
                    {auditRows.map((event) => (
                      <tr key={event.Sequence}>
                        <td className="mono right muted">{event.Sequence}</td>
                        <td className="muted">{humanise(event.ActorType)}</td>
                        <td>{event.Action}</td>
                        <td className="muted">{humanise(event.TargetType)}</td>
                        <td>
                          <Status
                            value={event.Result}
                            tone={event.Result === "success" ? "ok" : "bad"}
                          />
                        </td>
                        <td className="mono tiny muted" title={event.Hash}>
                          {event.Hash.slice(0, 16)}
                        </td>
                        <td
                          className="mono right muted"
                          title={fullDateTime(event.OccurredAt)}
                        >
                          {relative(event.OccurredAt)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Panel>

          <Panel title="What is recorded" flush>
            <div className="panel-body">
              <p className="small muted" style={{ marginTop: 0 }}>
                Authentication attempts, session changes, order submissions and rejections,
                risk-limit changes, trading-authority grants and revocations, kill-switch
                activations, reconciliation runs, and strategy or model lifecycle changes --
                each with the actor, the target, the result and the reason.
              </p>
              <p className="small muted" style={{ marginBottom: 0 }}>
                The application&rsquo;s database role has INSERT and SELECT on this table and
                nothing else. An UPDATE or DELETE from the application is refused by Postgres,
                not by application code that could be bypassed.
              </p>
            </div>
          </Panel>
        </>
      ) : null}
    </>
  );
}
