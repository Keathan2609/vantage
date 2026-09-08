"use client";

/**
 * Account security: sessions, password, multi-factor enrolment, and — for an
 * administrator — users and audit-chain verification.
 *
 * The enrolment secret and recovery codes are shown exactly once, in this
 * page's memory, and are never written to storage the browser keeps. They are
 * not placed in localStorage, not in the URL, and not re-fetchable: the server
 * has no endpoint that returns them again.
 */

import { useState } from "react";

import { Empty, Notice, Panel, Status } from "@/components/ui";
import { ApiError, api, type MfaEnrolment } from "@/lib/api";
import { fullDateTime, humanise, relative } from "@/lib/format";
import { useAsync, useVantage } from "@/lib/store";

export default function SecurityPage() {
  const { session, refresh } = useVantage();
  const isAdmin = session?.user.role === "admin";

  const sessions = useAsync(() => api.sessions(), []);
  const users = useAsync(
    () => (isAdmin ? api.adminUsers() : Promise.resolve(null)),
    [isAdmin],
  );

  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [enrolment, setEnrolment] = useState<MfaEnrolment | null>(null);
  const [activationCode, setActivationCode] = useState("");
  const [disablePassword, setDisablePassword] = useState("");
  const [disableCode, setDisableCode] = useState("");
  const [verification, setVerification] = useState<
    Awaited<ReturnType<typeof api.verifyAuditChain>> | null
  >(null);
  const [busy, setBusy] = useState(false);
  const [signedOut, setSignedOut] = useState(false);
  const [message, setMessage] = useState<{ tone: "ok" | "bad" | "warn"; text: string } | null>(
    null,
  );

  const fail = (cause: unknown, fallback: string) =>
    setMessage({
      tone: "bad",
      text: cause instanceof ApiError ? cause.message : fallback,
    });

  const changePassword = async () => {
    if (newPassword !== confirmPassword) {
      setMessage({ tone: "bad", text: "The new password and its confirmation differ." });
      return;
    }
    setBusy(true);
    setMessage(null);
    try {
      const result = await api.changePassword(currentPassword, newPassword);
      // Changing the password revokes every session, this one included, so the
      // page stops trying to load data and says plainly what happened.
      setMessage({ tone: "ok", text: result.message ?? "Password changed." });
      setCurrentPassword("");
      setNewPassword("");
      setConfirmPassword("");
      setSignedOut(true);
    } catch (cause) {
      fail(cause, "Could not change the password");
    } finally {
      setBusy(false);
    }
  };

  const enroll = async () => {
    setBusy(true);
    setMessage(null);
    try {
      setEnrolment(await api.enrollMfa());
    } catch (cause) {
      fail(cause, "Could not start enrolment");
    } finally {
      setBusy(false);
    }
  };

  const activate = async () => {
    setBusy(true);
    setMessage(null);
    try {
      await api.activateMfa(activationCode.trim());
      setMessage({
        tone: "ok",
        text: "Multi-factor authentication is enabled. Keep your recovery codes somewhere safe.",
      });
      setActivationCode("");
      setEnrolment(null);
      refresh();
    } catch (cause) {
      fail(cause, "That code was not accepted");
    } finally {
      setBusy(false);
    }
  };

  const disable = async () => {
    setBusy(true);
    setMessage(null);
    try {
      await api.disableMfa(disablePassword, disableCode.trim());
      setMessage({
        tone: "warn",
        text: "Multi-factor authentication is disabled. Only the password now protects this account.",
      });
      setDisablePassword("");
      setDisableCode("");
      refresh();
    } catch (cause) {
      fail(cause, "Could not disable multi-factor authentication");
    } finally {
      setBusy(false);
    }
  };

  const revoke = async (id: string) => {
    setBusy(true);
    setMessage(null);
    try {
      await api.revokeSession(id);
      setMessage({ tone: "ok", text: "Session revoked." });
      sessions.reload();
    } catch (cause) {
      fail(cause, "Could not revoke that session");
    } finally {
      setBusy(false);
    }
  };

  const verifyChain = async () => {
    setBusy(true);
    setMessage(null);
    try {
      setVerification(await api.verifyAuditChain());
    } catch (cause) {
      // A 409 means the chain does not verify, and that is a finding, not a
      // failed request. It is reported prominently rather than as an error.
      if (cause instanceof ApiError && cause.status === 409) {
        setMessage({
          tone: "bad",
          text: "AUDIT CHAIN VERIFICATION FAILED. The audit log has been altered. Investigate before trading.",
        });
      } else {
        fail(cause, "Could not verify the audit chain");
      }
    } finally {
      setBusy(false);
    }
  };

  const rows = sessions.data?.sessions ?? [];

  return (
    <>
      {message ? <Notice tone={message.tone}>{message.text}</Notice> : null}

      {signedOut ? (
        <Notice tone="warn">
          Every session was signed out, including this one. Nothing on this page will load
          until you sign in again.{" "}
          <button onClick={refresh} style={{ marginLeft: 6 }}>
            Sign in again
          </button>
        </Notice>
      ) : null}

      {!session?.user.mfa_enabled ? (
        <Notice tone="warn">
          Multi-factor authentication is not enabled on this account. A password alone is one
          credential away from full control of the terminal.
        </Notice>
      ) : null}

      <div className="grid sidebar-right">
        <div>
          <Panel
            title="Active sessions"
            note={`${rows.length} session(s)`}
            actions={<button onClick={sessions.reload}>Refresh</button>}
            flush
          >
            {sessions.loading ? (
              <Empty>Loading sessions…</Empty>
            ) : rows.length === 0 ? (
              <Empty>No sessions.</Empty>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>Address</th>
                    <th>Client</th>
                    <th className="right">Started</th>
                    <th className="right">Last seen</th>
                    <th className="right">Expires</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {rows.map((item) => (
                    <tr key={item.id}>
                      <td className="mono">{item.ip_address ?? "—"}</td>
                      <td className="truncate muted small" title={item.user_agent}>
                        {item.user_agent ?? "—"}
                        {item.current ? (
                          <span className="badge" data-tone="accent" style={{ marginLeft: 6 }}>
                            this session
                          </span>
                        ) : null}
                      </td>
                      <td
                        className="mono right muted"
                        title={fullDateTime(item.created_at)}
                      >
                        {relative(item.created_at)}
                      </td>
                      <td className="mono right muted">{relative(item.last_seen_at)}</td>
                      <td className="mono right muted">{relative(item.expires_at)}</td>
                      <td className="right">
                        <button disabled={busy || item.current} onClick={() => void revoke(item.id)}>
                          Revoke
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Panel>

          <Panel title="Multi-factor authentication" flush>
            <div className="panel-body">
              <dl className="kv">
                <dt>Status</dt>
                <dd>
                  <Status
                    value={session?.user.mfa_enabled ? "enabled" : "not enabled"}
                    tone={session?.user.mfa_enabled ? "ok" : "warn"}
                  />
                </dd>
                <dt>This session</dt>
                <dd>
                  {/* With no second factor configured there is nothing to have
                      satisfied, and calling the session "verified" would
                      overstate what the password alone established. */}
                  <Status
                    value={
                      !session?.user.mfa_enabled
                        ? "password only"
                        : session?.mfa_satisfied
                          ? "second factor verified"
                          : "challenge outstanding"
                    }
                    tone={
                      !session?.user.mfa_enabled
                        ? "warn"
                        : session?.mfa_satisfied
                          ? "ok"
                          : "bad"
                    }
                  />
                </dd>
              </dl>

              {!session?.user.mfa_enabled ? (
                enrolment ? (
                  <>
                    <Notice tone="warn">{enrolment.message}</Notice>
                    <div className="field">
                      <label>Secret (enter manually if you cannot scan)</label>
                      <code className="mono small wrap">{enrolment.secret}</code>
                    </div>
                    <div className="field">
                      <label>Provisioning URI</label>
                      <code className="mono tiny wrap">{enrolment.provisioning_uri}</code>
                    </div>
                    <div className="field">
                      <label>Recovery codes — shown once</label>
                      <div className="mono small" style={{ lineHeight: 1.7 }}>
                        {enrolment.recovery_codes.join("  ")}
                      </div>
                    </div>
                    <div className="field">
                      <label htmlFor="activate">Code from your authenticator</label>
                      <input
                        id="activate"
                        autoComplete="one-time-code"
                        value={activationCode}
                        onChange={(event) => setActivationCode(event.target.value)}
                      />
                    </div>
                    <button
                      data-variant="primary"
                      disabled={busy}
                      onClick={() => void activate()}
                    >
                      Verify and enable
                    </button>
                    <p className="tiny muted" style={{ marginTop: 8, marginBottom: 0 }}>
                      Enrolment is not complete until a code verifies. Until then the account
                      still signs in with the password alone, so an abandoned enrolment cannot
                      lock anyone out.
                    </p>
                  </>
                ) : (
                  <>
                    <button data-variant="primary" disabled={busy} onClick={() => void enroll()}>
                      Begin enrolment
                    </button>
                    <p className="tiny muted" style={{ marginTop: 8, marginBottom: 0 }}>
                      The secret is stored encrypted with a versioned key and bound to this
                      user, so a ciphertext copied to another row will not decrypt. Recovery
                      codes are stored only as hashes and are consumed on use.
                    </p>
                  </>
                )
              ) : (
                <>
                  <div className="field-row">
                    <div className="field">
                      <label htmlFor="dp">Password</label>
                      <input
                        id="dp"
                        type="password"
                        autoComplete="current-password"
                        value={disablePassword}
                        onChange={(event) => setDisablePassword(event.target.value)}
                      />
                    </div>
                    <div className="field">
                      <label htmlFor="dc">Current code</label>
                      <input
                        id="dc"
                        autoComplete="one-time-code"
                        value={disableCode}
                        onChange={(event) => setDisableCode(event.target.value)}
                      />
                    </div>
                  </div>
                  <button data-variant="danger" disabled={busy} onClick={() => void disable()}>
                    Disable multi-factor authentication
                  </button>
                  <p className="tiny muted" style={{ marginTop: 8, marginBottom: 0 }}>
                    Both factors are required to remove the second factor. Turning off a
                    security control is precisely what someone holding a stolen session would
                    try first.
                  </p>
                </>
              )}
            </div>
          </Panel>

          {isAdmin ? (
            <Panel title="Users" note="administration" flush>
              {users.loading ? (
                <Empty>Loading users…</Empty>
              ) : (users.data?.users ?? []).length === 0 ? (
                <Empty>No users.</Empty>
              ) : (
                <table>
                  <thead>
                    <tr>
                      <th>Email</th>
                      <th>Name</th>
                      <th>Role</th>
                      <th>MFA</th>
                      <th>State</th>
                      <th className="right">Last login</th>
                    </tr>
                  </thead>
                  <tbody>
                    {(users.data?.users ?? []).map((user) => (
                      <tr key={user.id}>
                        <td>{user.email}</td>
                        <td className="muted">{user.display_name}</td>
                        <td>
                          <span className="badge">{user.role}</span>
                        </td>
                        <td>
                          <Status
                            value={user.mfa_enabled ? "on" : "off"}
                            tone={user.mfa_enabled ? "ok" : "warn"}
                          />
                        </td>
                        <td>
                          <Status
                            value={user.disabled ? "disabled" : "active"}
                            tone={user.disabled ? "bad" : "ok"}
                          />
                        </td>
                        <td className="mono right muted">
                          {user.last_login_at ? relative(user.last_login_at) : "never"}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </Panel>
          ) : null}
        </div>

        <div>
          <Panel title="Change password" flush>
            <div className="panel-body">
              <div className="field">
                <label htmlFor="cp">Current password</label>
                <input
                  id="cp"
                  type="password"
                  autoComplete="current-password"
                  value={currentPassword}
                  onChange={(event) => setCurrentPassword(event.target.value)}
                />
              </div>
              <div className="field">
                <label htmlFor="np">New password</label>
                <input
                  id="np"
                  type="password"
                  autoComplete="new-password"
                  value={newPassword}
                  onChange={(event) => setNewPassword(event.target.value)}
                />
              </div>
              <div className="field">
                <label htmlFor="cf">Confirm new password</label>
                <input
                  id="cf"
                  type="password"
                  autoComplete="new-password"
                  value={confirmPassword}
                  onChange={(event) => setConfirmPassword(event.target.value)}
                />
              </div>
              <button
                data-variant="primary"
                disabled={busy || !currentPassword || !newPassword}
                onClick={() => void changePassword()}
                style={{ width: "100%" }}
              >
                {busy ? "Working…" : "Change password"}
              </button>
              <p className="tiny muted" style={{ marginTop: 8, marginBottom: 0 }}>
                Passwords are hashed with Argon2id — memory-hard, so a stolen hash is
                expensive to attack with hardware that makes short work of a fast hash.
                Changing the password revokes every other session.
              </p>
            </div>
          </Panel>

          <Panel title="Role" flush>
            <div className="panel-body">
              <dl className="kv">
                <dt>Signed in as</dt>
                <dd>{session?.user.email ?? "—"}</dd>
                <dt>Role</dt>
                <dd>{humanise(session?.user.role)}</dd>
                <dt>Session expires</dt>
                <dd>{session ? relative(session.expires_at) : "—"}</dd>
              </dl>
              <table>
                <thead>
                  <tr>
                    <th>Role</th>
                    <th>May</th>
                  </tr>
                </thead>
                <tbody>
                  <tr>
                    <td>VIEWER</td>
                    <td className="muted">read only</td>
                  </tr>
                  <tr>
                    <td>TRADER</td>
                    <td className="muted">
                      read, place and cancel orders, run research, change risk limits and
                      authority
                    </td>
                  </tr>
                  <tr>
                    <td>ADMIN</td>
                    <td className="muted">
                      user administration and audit verification — <strong>not</strong> trading
                    </td>
                  </tr>
                </tbody>
              </table>
              <p className="tiny muted" style={{ marginBottom: 0 }}>
                Administration and trading are separate on purpose. An administrator can
                manage who has access but cannot place an order, so compromising the
                administrative role does not directly move a position.
              </p>
            </div>
          </Panel>

          {isAdmin ? (
            <Panel title="Audit chain" flush>
              <div className="panel-body">
                <button disabled={busy} onClick={() => void verifyChain()}>
                  {busy ? "Verifying…" : "Verify the chain"}
                </button>
                {verification ? (
                  <>
                    <dl className="kv" style={{ marginTop: 10 }}>
                      <dt>Verified</dt>
                      <dd>
                        <Status
                          value={verification.verified ? "intact" : "broken"}
                          tone={verification.verified ? "ok" : "bad"}
                        />
                      </dd>
                      <dt>Events checked</dt>
                      <dd>{verification.events_checked}</dd>
                      <dt>Head sequence</dt>
                      <dd>{verification.head_sequence}</dd>
                      <dt>Head hash</dt>
                      <dd className="tiny">{verification.head_hash.slice(0, 24)}</dd>
                    </dl>
                    <p className="tiny muted" style={{ marginBottom: 0 }}>
                      {verification.note}
                    </p>
                  </>
                ) : (
                  <p className="tiny muted" style={{ marginTop: 8, marginBottom: 0 }}>
                    Recomputes every hash from the first event and reports the first broken
                    link, so an investigation has a starting point rather than a boolean.
                  </p>
                )}
              </div>
            </Panel>
          ) : null}

          <Panel title="Session handling" flush>
            <div className="panel-body">
              <p className="small muted" style={{ marginTop: 0 }}>
                The session lives in an HttpOnly, SameSite cookie the browser sends
                automatically. No token is ever readable by JavaScript, so a cross-site
                scripting bug in this interface cannot exfiltrate a session. State-changing
                requests additionally echo a readable CSRF cookie into a header, which a
                cross-site request cannot set.
              </p>
              <p className="small muted" style={{ marginBottom: 0 }}>
                Repeated failed sign-ins lock the account for a period, and per-operation rate
                limits apply on top. The kill switch has a deliberately generous limit: an
                operator must never be throttled out of stopping trading.
              </p>
            </div>
          </Panel>
        </div>
      </div>
    </>
  );
}
