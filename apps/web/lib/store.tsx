"use client";

/**
 * Shared client state: session, accounts, live market data and portfolio.
 *
 * One polling loop feeds the whole app rather than each page polling
 * independently. A terminal with a dozen panels each on its own timer produces
 * a dozen times the requests and a UI where different panels disagree about
 * the current price.
 *
 * Polling rather than a socket, deliberately: this build's market data updates
 * every couple of seconds, a 2-second poll of two small endpoints is cheap and
 * has no reconnect semantics to get wrong. A push channel is the right answer
 * at higher frequency, and the API is shaped so that swapping it in does not
 * change any page.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";

import {
  ApiError,
  api,
  type Account,
  type MarketHealth,
  type MarketStatus,
  type Portfolio,
  type Quote,
  type Session,
  type VersionInfo,
} from "./api";

const MARKET_POLL_MS = 2000;
const PORTFOLIO_POLL_MS = 5000;

interface VantageState {
  status: "loading" | "unauthenticated" | "ready";
  session: Session | null;
  version: VersionInfo | null;
  accounts: Account[];
  account: Account | null;
  selectAccount: (id: string) => void;
  quotes: Record<string, Quote>;
  health: Record<string, MarketHealth>;
  marketStatus: MarketStatus | null;
  portfolio: Portfolio | null;
  refresh: () => void;
  signOut: () => Promise<void>;
  lastError: string | null;
}

const Context = createContext<VantageState | null>(null);

export function VantageProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<VantageState["status"]>("loading");
  const [session, setSession] = useState<Session | null>(null);
  const [version, setVersion] = useState<VersionInfo | null>(null);
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [accountId, setAccountId] = useState<string | null>(null);
  const [quotes, setQuotes] = useState<Record<string, Quote>>({});
  const [health, setHealth] = useState<Record<string, MarketHealth>>({});
  const [marketStatus, setMarketStatus] = useState<MarketStatus | null>(null);
  const [portfolio, setPortfolio] = useState<Portfolio | null>(null);
  const [lastError, setLastError] = useState<string | null>(null);
  const [tick, setTick] = useState(0);

  // Kept in a ref so the polling loops read the current account without being
  // torn down and restarted every time it changes. Assigned in an effect
  // rather than during render: a render can be discarded or replayed, and a
  // ref written during one would then hold a value that never happened.
  const accountRef = useRef<string | null>(null);
  useEffect(() => {
    accountRef.current = accountId;
  }, [accountId]);

  const refresh = useCallback(() => setTick((n) => n + 1), []);

  // --- Session bootstrap --------------------------------------------------
  useEffect(() => {
    let cancelled = false;

    (async () => {
      try {
        const [sessionResult, versionResult] = await Promise.all([
          api.session(),
          api.version(),
        ]);
        if (cancelled) return;
        setSession(sessionResult);
        setVersion(versionResult);

        const accountResult = await api.accounts();
        if (cancelled) return;
        setAccounts(accountResult.accounts);
        setAccountId((current) => current ?? accountResult.accounts[0]?.id ?? null);
        setStatus("ready");
      } catch (error) {
        if (cancelled) return;
        if (error instanceof ApiError && error.isAuthFailure) {
          setStatus("unauthenticated");
          return;
        }
        setLastError(
          error instanceof Error ? error.message : "Could not reach the control plane",
        );
        setStatus("unauthenticated");
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [tick]);

  // --- Market data loop --------------------------------------------------
  useEffect(() => {
    if (status !== "ready") return;
    let cancelled = false;

    const poll = async () => {
      try {
        const [quoteResult, healthResult, statusResult] = await Promise.all([
          api.quotes(),
          api.marketHealth(),
          api.marketStatus(),
        ]);
        if (cancelled) return;
        setQuotes(Object.fromEntries(quoteResult.quotes.map((q) => [q.instrument_id, q])));
        setHealth(Object.fromEntries(healthResult.health.map((h) => [h.instrument_id, h])));
        setMarketStatus(statusResult);
        setLastError(null);
      } catch (error) {
        if (cancelled) return;
        // A failed poll leaves the last known values on screen and records the
        // error. Blanking the display would be worse: an operator would not
        // know whether the market stopped or the connection did.
        if (error instanceof ApiError && error.isAuthFailure) {
          setStatus("unauthenticated");
          return;
        }
        setLastError("Market data feed interrupted");
      }
    };

    void poll();
    const handle = window.setInterval(poll, MARKET_POLL_MS);
    return () => {
      cancelled = true;
      window.clearInterval(handle);
    };
  }, [status]);

  // --- Portfolio loop ----------------------------------------------------
  useEffect(() => {
    if (status !== "ready" || !accountId) return;
    let cancelled = false;

    const poll = async () => {
      const id = accountRef.current;
      if (!id) return;
      try {
        const result = await api.portfolio(id);
        if (!cancelled) setPortfolio(result);
      } catch (error) {
        if (error instanceof ApiError && error.isAuthFailure && !cancelled) {
          setStatus("unauthenticated");
        }
      }
    };

    void poll();
    const handle = window.setInterval(poll, PORTFOLIO_POLL_MS);
    return () => {
      cancelled = true;
      window.clearInterval(handle);
    };
  }, [status, accountId, tick]);

  const signOut = useCallback(async () => {
    try {
      await api.logout();
    } finally {
      setSession(null);
      setPortfolio(null);
      setStatus("unauthenticated");
    }
  }, []);

  const account = useMemo(
    () => accounts.find((a) => a.id === accountId) ?? null,
    [accounts, accountId],
  );

  const value = useMemo<VantageState>(
    () => ({
      status,
      session,
      version,
      accounts,
      account,
      selectAccount: setAccountId,
      quotes,
      health,
      marketStatus,
      portfolio,
      refresh,
      signOut,
      lastError,
    }),
    [
      status,
      session,
      version,
      accounts,
      account,
      quotes,
      health,
      marketStatus,
      portfolio,
      refresh,
      signOut,
      lastError,
    ],
  );

  return <Context.Provider value={value}>{children}</Context.Provider>;
}

export function useVantage(): VantageState {
  const value = useContext(Context);
  if (!value) {
    throw new Error("useVantage must be used inside VantageProvider");
  }
  return value;
}

/**
 * Load data once, with an explicit reload.
 *
 * Every page uses this rather than each inventing its own loading and error
 * handling, so a failed request looks the same everywhere.
 */
export function useAsync<T>(
  loader: () => Promise<T>,
  deps: unknown[],
): { data: T | null; error: string | null; loading: boolean; reload: () => void } {
  const [nonce, setNonce] = useState(0);

  // One state write per completed request, tagged with the request it answers.
  // `loading` is then DERIVED by comparing the answer we hold against the
  // request we want, so nothing has to set a loading flag — which also means
  // no synchronous state write inside the effect, and one render per fetch
  // instead of three.
  const [answer, setAnswer] = useState<{
    request: string;
    data: T | null;
    error: string | null;
  }>({ request: "", data: null, error: null });

  // Deps here are ids, timeframes and flags: JSON is a sound identity for
  // them, and it avoids depending on array identity across renders.
  const request = JSON.stringify([deps, nonce]);

  useEffect(() => {
    let cancelled = false;

    loader()
      .then((result) => {
        if (!cancelled) setAnswer({ request, data: result, error: null });
      })
      .catch((cause: unknown) => {
        if (cancelled) return;
        setAnswer({
          request,
          // The previous data is deliberately dropped on error: showing stale
          // figures next to an error message is how someone acts on a number
          // that is no longer true.
          data: null,
          error: cause instanceof Error ? cause.message : "Request failed",
        });
      });

    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [request]);

  return {
    data: answer.data,
    error: answer.error,
    loading: answer.request !== request,
    reload: () => setNonce((n) => n + 1),
  };
}

/**
 * The current time, as state that advances.
 *
 * Reading `Date.now()` during render is impure: the same render would produce
 * a different result if replayed, and anything derived from it silently stops
 * updating. This keeps the clock in state so a countdown, a blackout badge or
 * a freshness indicator actually moves.
 */
export function useNow(intervalMs = 1000): number {
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const handle = window.setInterval(() => setNow(Date.now()), intervalMs);
    return () => window.clearInterval(handle);
  }, [intervalMs]);

  return now;
}
