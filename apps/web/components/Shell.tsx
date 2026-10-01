"use client";

/**
 * The application shell: navigation, the persistent top bar, and the sign-in
 * gate.
 *
 * The top bar is the part that matters. It carries the PAPER marker and the
 * account's live figures on every page, so the mode and the state of the
 * account are never more than one glance away. It does not collapse, scroll
 * away, or become a menu item.
 */

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useState, type ReactNode } from "react";

import { api } from "@/lib/api";
import { age, decimal, money, percent, relative, signed, tone } from "@/lib/format";
import { useVantage } from "@/lib/store";

const NAV: Array<{ section: string; items: Array<{ href: string; label: string }> }> = [
  {
    section: "Trading",
    items: [
      { href: "/", label: "Overview" },
      { href: "/markets", label: "Markets" },
      { href: "/chart", label: "Chart" },
      { href: "/trade", label: "Trade" },
      { href: "/orders", label: "Orders" },
      { href: "/positions", label: "Positions" },
      { href: "/portfolio", label: "Portfolio" },
    ],
  },
  {
    section: "Research",
    items: [
      { href: "/scanner", label: "Scanner" },
      { href: "/strategies", label: "Strategies" },
      { href: "/backtests", label: "Backtests" },
      { href: "/ml", label: "ML Lab" },
      { href: "/calendar", label: "News & Calendar" },
    ],
  },
  {
    section: "Control",
    items: [
      { href: "/operations", label: "Operations" },
      { href: "/risk", label: "Risk" },
      { href: "/authority", label: "Authority" },
      { href: "/connections", label: "Connections" },
      { href: "/activity", label: "Activity" },
      { href: "/security", label: "Security" },
    ],
  },
];

export function Shell({ children }: { children: ReactNode }) {
  const { status } = useVantage();

  if (status === "loading") {
    return (
      <div className="login-shell">
        <div className="muted">Connecting to the control plane…</div>
      </div>
    );
  }
  if (status === "unauthenticated") {
    return <SignIn />;
  }

  return (
    <div className="app">
      <div className="brand">
        <span className="brand-mark" aria-hidden="true" />
        <span className="brand-name">Vantage</span>
      </div>
      <TopBar />
      <Navigation />
      <main className="main">{children}</main>
    </div>
  );
}

function Navigation() {
  const pathname = usePathname();
  const { portfolio } = useVantage();

  return (
    <nav className="nav" aria-label="Primary">
      {NAV.map((group) => (
        <div key={group.section}>
          <div className="nav-section">{group.section}</div>
          {group.items.map((item) => {
            const active =
              item.href === "/" ? pathname === "/" : pathname.startsWith(item.href);
            let badge: string | null = null;
            if (item.href === "/positions" && portfolio?.open_positions) {
              badge = String(portfolio.open_positions);
            }
            if (item.href === "/orders" && portfolio?.pending_orders) {
              badge = String(portfolio.pending_orders);
            }
            return (
              <Link
                key={item.href}
                href={item.href}
                className="nav-link"
                data-active={active}
              >
                <span>{item.label}</span>
                {badge ? <span className="nav-badge">{badge}</span> : null}
              </Link>
            );
          })}
        </div>
      ))}
    </nav>
  );
}

function TopBar() {
  const {
    session,
    account,
    accounts,
    selectAccount,
    portfolio,
    quotes,
    health,
    marketStatus,
    lastError,
    signOut,
  } = useVantage();

  const currency = portfolio?.currency ?? account?.currency ?? "ZAR";
  const dayTone = tone(portfolio?.day_pnl);
  const unrealisedTone = tone(portfolio?.unrealized_pnl);

  // The primary market is shown in the bar itself, since it is the instrument
  // the account actually trades.
  const primary = quotes["XAUUSD.m"] ?? Object.values(quotes)[0];
  const primaryHealth = primary ? health[primary.instrument_id] : undefined;

  return (
    <div className="topbar">
      <div className="paper-badge" title="All balances and trades are simulated.">
        Paper
      </div>

      {accounts.length > 1 ? (
        <div className="tile">
          <div className="tile-label">Account</div>
          <select
            value={account?.id ?? ""}
            onChange={(event) => selectAccount(event.target.value)}
            style={{ width: 150, padding: "1px 4px", fontSize: 11 }}
            aria-label="Select account"
          >
            {accounts.map((a) => (
              <option key={a.id} value={a.id}>
                {a.name}
              </option>
            ))}
          </select>
        </div>
      ) : (
        <div className="tile">
          <div className="tile-label">Account</div>
          <div className="tile-value small">{account?.name ?? "-"}</div>
        </div>
      )}

      <div className="tile">
        <div className="tile-label">Equity</div>
        <div className="tile-value">{money(portfolio?.equity, currency)}</div>
      </div>
      <div className="tile">
        <div className="tile-label">Balance</div>
        <div className="tile-value">{money(portfolio?.balance, currency)}</div>
      </div>
      <div className="tile">
        <div className="tile-label">Day P&amp;L</div>
        <div className={`tile-value ${dayTone}`}>{signed(portfolio?.day_pnl, currency)}</div>
      </div>
      <div className="tile">
        <div className="tile-label">Open P&amp;L</div>
        <div className={`tile-value ${unrealisedTone}`}>
          {signed(portfolio?.unrealized_pnl, currency)}
        </div>
      </div>
      <div className="tile">
        <div className="tile-label">Free margin</div>
        <div className="tile-value">{money(portfolio?.free_margin, currency)}</div>
      </div>
      <div className="tile">
        <div className="tile-label">Drawdown</div>
        <div className="tile-value">{percent(portfolio?.drawdown_fraction)}</div>
      </div>

      {primary ? (
        <div className="tile">
          <div className="tile-label">{primary.symbol}</div>
          <div className="tile-value">
            {decimal(primary.bid, 2)}
            <span className="muted small"> / </span>
            {decimal(primary.ask, 2)}
            <span className="muted small"> {age(primary.age_seconds)}</span>
          </div>
        </div>
      ) : null}

      <div className="tile">
        <div className="tile-label">Feed</div>
        <div className="tile-value small">
          <span
            className="status"
            data-tone={
              primaryHealth?.tradable_by_automation
                ? "ok"
                : primaryHealth?.state === "degraded"
                  ? "warn"
                  : "bad"
            }
          >
            {primaryHealth?.state ?? "unknown"}
          </span>
        </div>
      </div>

      <div className="tile">
        <div className="tile-label">Market</div>
        <div className="tile-value small">
          <span className="status" data-tone={marketStatus?.tradable ? "ok" : "warn"}>
            {(marketStatus?.status ?? "unknown").replace(/_/g, " ")}
          </span>
        </div>
      </div>

      <div className="spacer" />

      {lastError ? (
        <div className="tile">
          <div className="tile-label">Warning</div>
          <div className="tile-value small loss">{lastError}</div>
        </div>
      ) : null}

      <div className="tile">
        <div className="tile-label">{session?.user.role ?? "-"}</div>
        <div className="tile-value small">
          {session?.user.email ?? "-"}
          {marketStatus?.next_close ? (
            <span className="muted"> · closes {relative(marketStatus.next_close)}</span>
          ) : null}
        </div>
      </div>
      <div className="tile">
        <button onClick={() => void signOut()}>Sign out</button>
      </div>
    </div>
  );
}

function SignIn() {
  const { refresh, lastError } = useVantage();
  const [email, setEmail] = useState("trader@vantage.local");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  // A session that has passed the password step but not the second factor can
  // do nothing except verify or log out, so the challenge is a distinct stage
  // rather than a message on the password form.
  const [stage, setStage] = useState<"password" | "challenge">("password");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const result = await api.login(email, password);
      // The password is not kept after it has been used.
      setPassword("");
      if (result.mfa_required) {
        setStage("challenge");
        setBusy(false);
        return;
      }
      refresh();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Sign-in failed");
      setBusy(false);
    }
  };

  const verify = async (event: React.FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.verifyMfa(code.trim());
      setCode("");
      refresh();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "That code was not accepted");
      setBusy(false);
    }
  };

  if (stage === "challenge") {
    return (
      <div className="login-shell">
        <form className="login-card" onSubmit={verify}>
          <div className="login-brand">
            <span className="brand-mark" aria-hidden="true" />
            <span className="brand-name">Vantage</span>
          </div>
          <p className="page-sub" style={{ marginBottom: 16 }}>
            Enter the six-digit code from your authenticator, or one recovery code.
          </p>

          {error ? <Alert>{error}</Alert> : null}

          <div className="field">
            <label htmlFor="code">Authentication code</label>
            <input
              id="code"
              inputMode="text"
              autoComplete="one-time-code"
              autoFocus
              value={code}
              onChange={(event) => setCode(event.target.value)}
              required
            />
          </div>
          <button
            type="submit"
            data-variant="primary"
            disabled={busy}
            style={{ width: "100%" }}
          >
            {busy ? "Verifying…" : "Verify"}
          </button>
          <button
            type="button"
            onClick={() => {
              setStage("password");
              setError(null);
              setCode("");
            }}
            style={{ width: "100%", marginTop: 6 }}
          >
            Start over
          </button>
          <p className="tiny muted" style={{ marginTop: 14, marginBottom: 0 }}>
            A recovery code is accepted once and then consumed. Until this step completes the
            session can read nothing and place nothing.
          </p>
        </form>
      </div>
    );
  }

  return (
    <div className="login-shell">
      <form className="login-card" onSubmit={submit}>
        <div className="login-brand">
          <span className="brand-mark" aria-hidden="true" />
          <span className="brand-name">Vantage</span>
        </div>
        <p className="page-sub" style={{ marginBottom: 16 }}>
          Paper trading terminal. All balances and trades in this build are simulated and
          no real funds can move.
        </p>

        {error ? <Alert>{error}</Alert> : null}
        {!error && lastError ? <Alert>{lastError}</Alert> : null}

        <div className="field">
          <label htmlFor="email">Email</label>
          <input
            id="email"
            type="email"
            autoComplete="username"
            value={email}
            onChange={(event) => setEmail(event.target.value)}
            required
          />
        </div>
        <div className="field">
          <label htmlFor="password">Password</label>
          <input
            id="password"
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            required
          />
        </div>
        <button type="submit" data-variant="primary" disabled={busy} style={{ width: "100%" }}>
          {busy ? "Signing in…" : "Sign in"}
        </button>

        <p className="tiny muted" style={{ marginTop: 14, marginBottom: 0 }}>
          Development credentials are printed by <code>control-api seed</code> and exist only
          on this machine.
        </p>
      </form>
    </div>
  );
}

function Alert({ children }: { children: ReactNode }) {
  return (
    <div className="notice" data-tone="bad" role="alert">
      {children}
    </div>
  );
}
