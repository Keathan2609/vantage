import { execFileSync } from "node:child_process";

import { expect, type APIRequestContext, type Page, request } from "@playwright/test";

/**
 * Shared helpers for the end-to-end suite.
 *
 * Credentials come from the environment only. `signIn` fails loudly when the
 * password is absent rather than falling back to a guess, so a misconfigured
 * run reads as "you did not set VANTAGE_E2E_PASSWORD" instead of "sign-in is
 * broken".
 */

export const API =
  process.env.VANTAGE_E2E_API_BASE_URL ?? "http://localhost:8080";

export const TRADER_EMAIL = process.env.VANTAGE_E2E_EMAIL ?? "trader@vantage.local";
export const VIEWER_EMAIL = process.env.VANTAGE_E2E_VIEWER_EMAIL ?? "viewer@vantage.local";
export const ADMIN_EMAIL = process.env.VANTAGE_E2E_ADMIN_EMAIL ?? "admin@vantage.local";

function requireEnv(name: string, hint: string): string {
  const value = process.env[name];
  if (!value) {
    throw new Error(
      `${name} is not set. ${hint} It is printed by \`control-api seed\` and is ` +
        `deliberately not defaulted in this repository.`,
    );
  }
  return value;
}

export function traderPassword(): string {
  return requireEnv("VANTAGE_E2E_PASSWORD", "The suite needs the seeded trader's password.");
}

export function viewerPassword(): string {
  return (
    process.env.VANTAGE_E2E_VIEWER_PASSWORD ??
    requireEnv(
      "VANTAGE_E2E_VIEWER_PASSWORD",
      "The authorisation tests need the seeded viewer's password.",
    )
  );
}

/**
 * Signs in through the real form.
 *
 * The form is used rather than a direct API call because the login screen is
 * itself a thing that can break, and it is the only page an unauthenticated
 * visitor can reach.
 */
export async function signIn(
  page: Page,
  email: string = TRADER_EMAIL,
  password: string = traderPassword(),
): Promise<void> {
  await page.goto("/");

  // Wait for the shell to decide which of the two states it is in. Checking
  // .topbar directly races the "Connecting to the control plane" placeholder:
  // the check returns false, the helper waits for a login form that will never
  // appear, and the test times out looking like a sign-in failure.
  await Promise.race([
    page.locator(".topbar").waitFor({ state: "visible", timeout: 20_000 }),
    page.locator(".login-card").waitFor({ state: "visible", timeout: 20_000 }),
  ]);

  // Already signed in — the saved session from global setup is being reused.
  if (await page.locator(".topbar").isVisible()) return;

  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await Promise.all([
    page.waitForResponse(
      (r) => r.url().includes("/auth/login") && r.request().method() === "POST",
    ),
    page.getByRole("button", { name: "Sign in", exact: true }).click(),
  ]);

  await expect(page.locator(".topbar")).toBeVisible({ timeout: 20_000 });
}

export async function signOut(page: Page): Promise<void> {
  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page.locator(".login-card")).toBeVisible({ timeout: 20_000 });
}

/**
 * An authenticated API context, for the setup and teardown a UI test should
 * not have to click through — arming a broker fault, releasing a kill switch.
 *
 * Using the API for arrangement and the UI for the assertion keeps each test
 * about one thing.
 */
export async function apiClient(
  stateFile = "test-results/.auth/trader.json",
): Promise<{ ctx: APIRequestContext; csrf: string }> {
  // The session saved by global setup is reused rather than a fresh login
  // being performed. Signing in per spec exhausts the login budget — burst
  // ten, then two per minute — and the suite then reports the platform's own
  // rate limiting as a failure.
  const ctx = await request.newContext({ baseURL: API, storageState: stateFile });
  const state = await ctx.storageState();
  const csrf = state.cookies.find((c) => c.name === "vantage_csrf")?.value ?? "";
  if (!csrf) {
    throw new Error(
      `no CSRF cookie in ${stateFile}; global setup did not complete a sign-in`,
    );
  }
  return { ctx, csrf };
}

/** Headers for a state-changing API call. */
export function csrfHeaders(csrf: string, idempotencyKey?: string): Record<string, string> {
  const headers: Record<string, string> = { "X-Vantage-CSRF": csrf };
  if (idempotencyKey) headers["Idempotency-Key"] = idempotencyKey;
  return headers;
}

/** The first account the signed-in user owns. */
export async function firstAccountId(ctx: APIRequestContext): Promise<string> {
  const res = await ctx.get("/api/v1/accounts");
  const body = await res.json();
  const id = body.accounts?.[0]?.id;
  if (!id) throw new Error("the signed-in user owns no account");
  return id;
}

/**
 * Waits until the market-data ingestor has published a tradable quote.
 *
 * The ingestor polls every two seconds, so for a moment after a reseed the
 * newest quote is stale and the risk engine correctly refuses to trade on it.
 * Waiting here keeps the suite deterministic without weakening the check.
 */
export async function waitForTradableFeed(
  ctx: APIRequestContext,
  instrumentId = "XAUUSD.m",
  timeoutMs = 30_000,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  let state = "unknown";
  while (Date.now() < deadline) {
    const res = await ctx.get("/api/v1/market/health");
    const body = await res.json();
    const entry = (body.health ?? []).find(
      (h: { instrument_id: string }) => h.instrument_id === instrumentId,
    );
    if (entry) {
      state = entry.state;
      if (entry.tradable_by_automation) return;
    }
    await new Promise((resolve) => setTimeout(resolve, 1000));
  }
  throw new Error(
    `market data for ${instrumentId} never became tradable (last state: ${state})`,
  );
}

/** Releases every active kill switch, so one test cannot poison the next. */
export async function releaseAllKillSwitches(
  ctx: APIRequestContext,
  csrf: string,
): Promise<void> {
  const res = await ctx.get("/api/v1/kill-switches?active=true");
  const body = await res.json();
  for (const sw of body.kill_switches ?? []) {
    if (!sw.Active) continue;
    await ctx.post(`/api/v1/kill-switches/${sw.ID}/deactivate`, {
      headers: csrfHeaders(csrf),
    });
  }
}

/** Disarms every broker fault. Development-only endpoint. */
export async function resetBrokerFaults(
  ctx: APIRequestContext,
  csrf: string,
): Promise<void> {
  await ctx.post("/api/v1/dev/broker-faults/reset", { headers: csrfHeaders(csrf) });
}

/**
 * Clears the per-operation rate-limit buckets in a DEVELOPMENT Redis.
 *
 * The suite legitimately performs more control changes in a minute than a
 * human would: arming twelve fault modes, activating and releasing kill
 * switches, revoking and restoring an authority. The control-change budget is
 * burst ten then one every two seconds, and it is a real control that is NOT
 * widened to suit a test — so the development cache is cleared instead, which
 * is what the smoke suite does for the same reason.
 *
 * A no-op wherever the development container is unreachable.
 */
export function clearDevRateLimits(): void {
  try {
    execFileSync(
      "docker",
      [
        "exec",
        "vantage-redis",
        "sh",
        "-c",
        "redis-cli --scan --pattern 'vantage:rl:*' | xargs -r redis-cli del > /dev/null; echo cleared",
      ],
      { timeout: 15_000, stdio: "ignore" },
    );
  } catch {
    // Not a development stack. A genuine 429 will still fail the test loudly.
  }
}

/** Collects console errors so a page can be asserted to render cleanly. */
export function collectConsoleErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on("console", (message) => {
    if (message.type() === "error") errors.push(message.text());
  });
  page.on("pageerror", (error) => errors.push(error.message));
  return errors;
}

/**
 * A stop-loss price that sits inside the account's per-trade risk budget.
 *
 * A hardcoded stop is a trap: gold moves, and a stop that was 0.5% away when
 * the test was written is 2% away a week later. The first run of this suite
 * failed on exactly that — the risk engine correctly refused an order risking
 * 1.84% against a 1% limit, and the test reported it as a platform failure.
 *
 * `fraction` is the distance from the current price, well inside the 1% of
 * equity the seeded account allows at the minimum tradable size.
 */
export async function safeStop(
  ctx: APIRequestContext,
  instrumentId = "XAUUSD.m",
  side: "buy" | "sell" = "buy",
  fraction = 0.004,
): Promise<string> {
  const quotes = await ctx.get("/api/v1/market/quotes").then((r) => r.json());
  const quote = (quotes.quotes ?? []).find(
    (q: { instrument_id: string }) => q.instrument_id === instrumentId,
  );
  if (!quote) throw new Error(`no quote for ${instrumentId}`);

  const reference = Number.parseFloat(side === "buy" ? quote.bid : quote.ask);
  const stop = side === "buy" ? reference * (1 - fraction) : reference * (1 + fraction);
  return stop.toFixed(2);
}

/**
 * Closes every open position so the suite starts from a known state.
 *
 * Without this, positions accumulate across runs until the per-instrument
 * exposure ceiling binds and every later order is refused — correctly, for a
 * reason that has nothing to do with the test. The engine was right; the
 * fixture was dirty.
 */
export async function flattenAll(
  ctx: APIRequestContext,
  csrf: string,
  accountId: string,
): Promise<void> {
  const res = await ctx.get(`/api/v1/positions?account_id=${accountId}`);
  const body = await res.json();
  for (const position of body.positions ?? []) {
    await ctx.post(`/api/v1/positions/${position.id}/flatten`, {
      headers: csrfHeaders(csrf, newKey(`flatten-${position.id.slice(0, 8)}`)),
      data: { confirm: true },
    });
  }
}

/**
 * Cancels every order still live at the venue.
 *
 * A resting limit order parked far from the market stays working, and a run
 * that fails before cancelling it leaves it there. They accumulate until the
 * account holds more pending orders than max_pending_orders allows, and every
 * later order is then refused for a reason unrelated to the test.
 *
 * The states are the live ones from domain.OrderStatus. ACCEPTED matters most:
 * that is where a resting limit order actually sits, and a list that omitted it
 * cancelled nothing.
 */
export async function cancelAllWorking(
  ctx: APIRequestContext,
  csrf: string,
  accountId: string,
): Promise<void> {
  const res = await ctx.get(`/api/v1/orders?account_id=${accountId}&limit=200`);
  const body = await res.json();
  const live = new Set([
    "CREATED",
    "VALIDATING",
    "ACCEPTED",
    "SUBMITTED",
    "PARTIALLY_FILLED",
  ]);
  for (const order of body.orders ?? []) {
    if (!live.has(String(order.status).toUpperCase())) continue;
    await ctx.post(`/api/v1/orders/${order.id}/cancel`, {
      headers: csrfHeaders(csrf),
    });
  }
}

/** A unique idempotency key per test run. */
export function newKey(prefix: string): string {
  return `e2e-${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
}
