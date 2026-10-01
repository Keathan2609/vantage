"use client";

/**
 * Operations: reconciliation state, unresolved divergence, and the constrained
 * actions an administrator may take.
 *
 * # What this page is for
 *
 * Answering, in one place, the question an operator actually has when nothing
 * is trading: is anything stopped, why, and what am I allowed to do about it.
 *
 * # Why the actions are so narrow
 *
 * Resolving a divergence can book an execution into an append-only ledger.
 * The button set is therefore derived from `allowed_actions`, which the SERVER
 * computes from the repair policy -- this page does not decide what is
 * permitted, and deliberately cannot. A terminal that re-implemented the
 * policy would eventually offer an action the server refuses, which reads to
 * an operator as a broken button rather than as a rule.
 *
 * Note also what no control here can do: name a quantity, a price, or a target
 * order status. IMPORT_BROKER_FILL books the execution the VENUE reported, out
 * of the issue's own stored evidence. The operator chooses which remedy, never
 * what the resulting numbers are.
 *
 * # Resolved issues are not hidden
 *
 * A divergence that keeps recurring is itself a finding, and history is the
 * only way to see that. The default view is everything, with a filter for the
 * open ones rather than the reverse.
 */

import { useCallback, useMemo, useState } from "react";

import { Empty, Notice, Panel, Stat, StatStrip, Status } from "@/components/ui";
import {
  ApiError,
  api,
  type ReconciliationIssue,
  type TradingState,
} from "@/lib/api";
import { fullDateTime, humanise, relative } from "@/lib/format";
import { useAsync, useVantage } from "@/lib/store";

/** Maps a trading state to the tone the Status component understands. */
function stateTone(state: TradingState): "ok" | "warn" | "bad" {
  switch (state) {
    case "HEALTHY":
      return "ok";
    case "DEGRADED":
      return "warn";
    default:
      // RECONCILIATION_REQUIRED and TRADING_HALTED both stop automation, and
      // both should look like it.
      return "bad";
  }
}

function severityTone(severity: string): "ok" | "warn" | "bad" {
  if (severity === "critical") return "bad";
  if (severity === "warning") return "warn";
  return "ok";
}

/**
 * Groups issues the way an operator triages them.
 *
 * Not by severity, and not by time. The first question is "is this waiting for
 * me", and an AUTOMATICALLY_REPAIRED critical issue needs no attention at all
 * while an OPERATOR_ACTION_REQUIRED warning does.
 */
function groupIssues(issues: ReconciliationIssue[]) {
  return {
    operator: issues.filter((i) => i.status === "OPERATOR_ACTION_REQUIRED"),
    open: issues.filter((i) => i.status === "OPEN"),
    repaired: issues.filter((i) => i.status === "AUTOMATICALLY_REPAIRED"),
    resolved: issues.filter(
      (i) => i.status === "RESOLVED" || i.status === "UNRESOLVABLE",
    ),
  };
}

export default function OperationsPage() {
  const { session, accounts } = useVantage();
  const isAdmin = session?.user.role === "admin";

  const overview = useAsync(() => api.operations(), []);
  const [selectedAccount, setSelectedAccount] = useState<string>("");

  const accountId = selectedAccount || accounts[0]?.id || "";
  const issues = useAsync(
    () => (accountId ? api.reconciliationIssues(accountId) : Promise.resolve(null)),
    [accountId],
  );

  const [busy, setBusy] = useState(false);
  const [openOnly, setOpenOnly] = useState(false);
  const [message, setMessage] = useState<{ tone: "ok" | "bad" | "warn"; text: string } | null>(
    null,
  );
  const [expanded, setExpanded] = useState<string | null>(null);
  const [reasons, setReasons] = useState<Record<string, string>>({});

  const refreshAll = useCallback(() => {
    overview.reload();
    issues.reload();
  }, [overview, issues]);

  const runReconciliation = useCallback(async () => {
    if (!accountId) return;
    setBusy(true);
    setMessage(null);
    try {
      const report = await api.runReconciliation(accountId);
      setMessage({
        tone: report.clean ? "ok" : "warn",
        text: report.clean
          ? `Reconciliation clean: ${report.orders_compared} order comparisons, ` +
            `${report.executions_seen} executions seen, ${report.repaired} repaired.`
          : `Reconciliation found divergence: ${report.critical} critical, ` +
            `${report.repaired} repaired automatically. Review the issues below.`,
      });
      refreshAll();
    } catch (error) {
      // A 409 means a run is already in progress. That is overlap prevention
      // working, not a failure, and it must not read as one.
      const conflict = error instanceof ApiError && error.status === 409;
      setMessage({
        tone: conflict ? "warn" : "bad",
        text:
          error instanceof ApiError
            ? error.message
            : "Reconciliation could not be started.",
      });
    } finally {
      setBusy(false);
    }
  }, [accountId, refreshAll]);

  const resolve = useCallback(
    async (issue: ReconciliationIssue, action: string) => {
      const reason = (reasons[issue.id] ?? "").trim();
      if (reason.length < 10) {
        setMessage({
          tone: "bad",
          text:
            "A reason of at least ten characters is required. It is the only durable " +
            "record of why this repair was applied, and the server refuses without it.",
        });
        return;
      }
      setBusy(true);
      setMessage(null);
      try {
        const result = await api.resolveReconciliationIssue(accountId, issue.id, {
          action,
          reason,
        });
        setMessage({
          tone: result.state_changed ? "warn" : "ok",
          text: result.state_changed
            ? `Financial state was written: ${result.detail}`
            : `Issue closed: ${result.detail}`,
        });
        setReasons((prev) => ({ ...prev, [issue.id]: "" }));
        refreshAll();
      } catch (error) {
        setMessage({
          tone: "bad",
          text:
            error instanceof ApiError
              ? error.message
              : "The action could not be applied.",
        });
      } finally {
        setBusy(false);
      }
    },
    [accountId, reasons, refreshAll],
  );

  const visible = useMemo(() => {
    const all = issues.data?.issues ?? [];
    return openOnly ? all.filter((i) => i.resolved_at === null) : all;
  }, [issues.data, openOnly]);
  const groups = useMemo(() => groupIssues(visible), [visible]);

  return (
    <div className="stack">
      <header className="page-head">
        <h1>Operations</h1>
        <p className="muted">
          Reconciliation state and the divergence between Vantage&rsquo;s records and the
          venue&rsquo;s. Every balance and trade here is simulated.
        </p>
      </header>

      {message && <Notice tone={message.tone}>{message.text}</Notice>}

      <Panel
        title="System"
        note="Whether automated trading may run, and why not"
        actions={
          <button type="button" onClick={refreshAll} disabled={busy}>
            Refresh
          </button>
        }
      >
        {overview.loading && <Empty>Loading operations state&hellip;</Empty>}
        {overview.error && <Notice tone="bad">{overview.error}</Notice>}
        {overview.data && (
          <>
            <StatStrip>
              <Stat
                label="Trading state"
                value={
                  <Status
                    tone={stateTone(overview.data.trading_state)}
                    label={humanise(overview.data.trading_state)}
                  />
                }
                sub="process health says nothing about this"
              />
              <Stat label="Halted accounts" value={overview.data.halted_accounts} />
              <Stat label="Open issues" value={overview.data.open_issues} />
              <Stat label="Critical" value={overview.data.critical_issues} />
            </StatStrip>

            {/*
              Stated explicitly, because it is the single most common
              misreading of a halted system: manual trading and every read
              path keep working. An operator can see the warning and decide,
              which an algorithm cannot.
            */}
            {overview.data.trading_state !== "HEALTHY" && (
              <Notice tone="warn">
                Automated trading is stopped. Manual trading and all read access are
                unaffected: a person can see the warning and decide, which an algorithm
                cannot.
              </Notice>
            )}

            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>Account</th>
                    <th>Broker</th>
                    <th>State</th>
                    <th>Automation</th>
                    <th>Halt scope</th>
                    <th className="num">Open</th>
                    <th className="num">Needs operator</th>
                    <th className="num">Uncertain</th>
                    <th>Last reconciled</th>
                  </tr>
                </thead>
              <tbody>
                {(overview.data.accounts ?? []).map((account) => (
                  <tr key={account.account_id}>
                    <td>
                      <code>{account.account_id.slice(0, 8)}</code>
                    </td>
                    <td>{account.broker_name}</td>
                    <td>
                      <Status
                        tone={stateTone(account.trading_state)}
                        label={humanise(account.trading_state)}
                      />
                    </td>
                    <td>{account.automation_allowed ? "permitted" : "stopped"}</td>
                    <td>{account.halt_scope === "NONE" ? "-" : humanise(account.halt_scope)}</td>
                    <td className="num">{account.open_issues}</td>
                    <td className="num">{account.operator_action_required}</td>
                    <td className="num">{account.uncertain_orders}</td>
                    <td>
                      {account.last_success_at
                        ? relative(account.last_success_at)
                        : "never"}
                    </td>
                  </tr>
                ))}
                {(overview.data.accounts ?? []).length === 0 && (
                  <tr>
                    <td colSpan={9}>
                      <Empty>No accounts.</Empty>
                    </td>
                  </tr>
                )}
                </tbody>
              </table>
            </div>
          </>
        )}
      </Panel>

      <Panel
        title="Reconciliation"
        note={issues.data?.reason ?? "Divergence between Vantage and the venue"}
        actions={
          <div className="row-gap">
            {accounts.length > 1 && (
              <label className="field">
                <span>Account</span>
                <select
                  value={accountId}
                  onChange={(event) => setSelectedAccount(event.target.value)}
                >
                  {accounts.map((account) => (
                    <option key={account.id} value={account.id}>
                      {account.name}
                    </option>
                  ))}
                </select>
              </label>
            )}
            <label className="field">
              <span>Unresolved only</span>
              <input
                type="checkbox"
                checked={openOnly}
                onChange={(event) => setOpenOnly(event.target.checked)}
              />
            </label>
            <button
              type="button"
              onClick={runReconciliation}
              disabled={busy || !accountId}
            >
              Run reconciliation
            </button>
          </div>
        }
      >
        {issues.loading && <Empty>Loading issues&hellip;</Empty>}
        {issues.error && <Notice tone="bad">{issues.error}</Notice>}

        {issues.data && (
          <>
            <StatStrip>
              <Stat
                label="Automation"
                value={
                  <Status
                    tone={issues.data.automation_allowed ? "ok" : "bad"}
                    label={issues.data.automation_allowed ? "Permitted" : "Stopped"}
                  />
                }
              />
              <Stat
                label="Needs an operator"
                value={issues.data.operator_action_required}
              />
              <Stat
                label="Uncertain orders"
                value={issues.data.uncertain_orders}
                sub="venue-side outcome unknown"
              />
              <Stat
                label="Failed runs in a row"
                value={issues.data.consecutive_failures}
              />
            </StatStrip>

            {!isAdmin && groups.operator.length > 0 && (
              <Notice tone="warn">
                {groups.operator.length} issue(s) need an administrator. Resolving a
                divergence can write to the ledger, so it requires the admin role, which
                is deliberately barred from placing orders.
              </Notice>
            )}

            <IssueGroup
              heading="Operator action required"
              hint="Vantage will not guess at these. Each one needs a person to decide."
              issues={groups.operator}
              isAdmin={isAdmin}
              busy={busy}
              expanded={expanded}
              onExpand={setExpanded}
              reasons={reasons}
              onReason={(id, value) =>
                setReasons((prev) => ({ ...prev, [id]: value }))
              }
              onResolve={resolve}
            />
            <IssueGroup
              heading="Open"
              hint="Detected and awaiting the next run's repair."
              issues={groups.open}
              isAdmin={isAdmin}
              busy={busy}
              expanded={expanded}
              onExpand={setExpanded}
              reasons={reasons}
              onReason={(id, value) =>
                setReasons((prev) => ({ ...prev, [id]: value }))
              }
              onResolve={resolve}
            />
            <IssueGroup
              heading="Automatically repaired"
              hint="Vantage corrected its own records on provable evidence. No action needed."
              issues={groups.repaired}
              isAdmin={isAdmin}
              busy={busy}
              expanded={expanded}
              onExpand={setExpanded}
              reasons={reasons}
              onReason={(id, value) =>
                setReasons((prev) => ({ ...prev, [id]: value }))
              }
              onResolve={resolve}
            />
            <IssueGroup
              heading="Resolved"
              hint="Retained deliberately: a divergence that keeps recurring is itself a finding."
              issues={groups.resolved}
              isAdmin={isAdmin}
              busy={busy}
              expanded={expanded}
              onExpand={setExpanded}
              reasons={reasons}
              onReason={(id, value) =>
                setReasons((prev) => ({ ...prev, [id]: value }))
              }
              onResolve={resolve}
            />

            {visible.length === 0 && (
              <Empty>
                No reconciliation issues. Vantage&rsquo;s records agree with the venue.
              </Empty>
            )}
          </>
        )}
      </Panel>
    </div>
  );
}

function IssueGroup({
  heading,
  hint,
  issues,
  isAdmin,
  busy,
  expanded,
  onExpand,
  reasons,
  onReason,
  onResolve,
}: {
  heading: string;
  hint: string;
  issues: ReconciliationIssue[];
  isAdmin: boolean;
  busy: boolean;
  expanded: string | null;
  onExpand: (id: string | null) => void;
  reasons: Record<string, string>;
  onReason: (id: string, value: string) => void;
  onResolve: (issue: ReconciliationIssue, action: string) => void;
}) {
  if (issues.length === 0) return null;

  return (
    <section className="issue-group">
      <h3>
        {heading} <span className="muted">({issues.length})</span>
      </h3>
      <p className="muted small">{hint}</p>

      <ul className="issue-list">
        {issues.map((issue) => {
          const isOpen = expanded === issue.id;
          return (
            <li key={issue.id} className="issue">
              <div className="issue-head">
                <Status
                  tone={severityTone(issue.severity)}
                  label={humanise(issue.issue_type)}
                />
                <span className="muted small">
                  {issue.instrument_id ?? "-"} &middot; detected{" "}
                  {relative(issue.detected_at)}
                  {issue.check_count > 1 && ` · seen ${issue.check_count}×`}
                </span>
                <button
                  type="button"
                  data-variant="secondary"
                  onClick={() => onExpand(isOpen ? null : issue.id)}
                  aria-expanded={isOpen}
                >
                  {isOpen ? "Hide evidence" : "Evidence"}
                </button>
              </div>

              <p className="issue-description">{issue.description}</p>

              <dl className="issue-meta">
                <div>
                  <dt>Repair</dt>
                  <dd>
                    {issue.automatic_repair_allowed
                      ? "automatic where provable"
                      : humanise(issue.repair_class)}
                  </dd>
                </div>
                <div>
                  <dt>Halts</dt>
                  <dd>
                    {issue.halt_scope === "NONE" ? "nothing" : humanise(issue.halt_scope)}
                  </dd>
                </div>
                <div>
                  <dt>Last checked</dt>
                  <dd>{relative(issue.last_checked_at)}</dd>
                </div>
                {issue.resolved_at && (
                  <div>
                    <dt>Resolved</dt>
                    <dd>
                      {humanise(issue.resolution_action ?? "")} &middot;{" "}
                      {fullDateTime(issue.resolved_at)}
                    </dd>
                  </div>
                )}
              </dl>

              {isOpen && (
                <div className="issue-evidence">
                  {/*
                    The rationale is the server's own explanation of WHY this
                    class of divergence is or is not repaired automatically.
                    An operator being asked to decide needs to know why the
                    system would not decide for them.
                  */}
                  <p className="rationale">{issue.rationale}</p>
                  <div className="evidence-grid">
                    <div>
                      <h4>Vantage</h4>
                      <pre>{JSON.stringify(issue.local_state, null, 2)}</pre>
                    </div>
                    <div>
                      <h4>Venue</h4>
                      <pre>{JSON.stringify(issue.broker_state, null, 2)}</pre>
                    </div>
                  </div>
                  {issue.resolution_reason && (
                    <p className="muted small">
                      Resolution: {issue.resolution_reason}
                    </p>
                  )}
                </div>
              )}

              {isAdmin && issue.resolved_at === null && (
                <div className="issue-actions">
                  <label className="field">
                    <span>Reason (required, retained permanently)</span>
                    <input
                      type="text"
                      value={reasons[issue.id] ?? ""}
                      onChange={(event) => onReason(issue.id, event.target.value)}
                      placeholder="Why this resolution is correct"
                      minLength={10}
                    />
                  </label>
                  <div className="row-gap">
                    {issue.allowed_actions.map((action) => (
                      <button
                        key={action}
                        type="button"
                        data-variant={
                          action === "IMPORT_BROKER_FILL" ||
                          action === "MARK_BROKER_REJECTED" ||
                          action === "MARK_NOT_EXECUTED" ||
                          action === "LINK_BROKER_ORDER"
                            ? "danger"
                            : "secondary"
                        }
                        onClick={() => onResolve(issue, action)}
                        disabled={busy}
                        title={
                          action === "IMPORT_BROKER_FILL"
                            ? "Books the execution the VENUE reported, from this issue's " +
                              "own evidence. You cannot specify the quantity or price."
                            : undefined
                        }
                      >
                        {humanise(action)}
                      </button>
                    ))}
                  </div>
                </div>
              )}
            </li>
          );
        })}
      </ul>
    </section>
  );
}
