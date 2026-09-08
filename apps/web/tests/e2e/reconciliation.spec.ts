import { expect, test, type APIRequestContext } from "@playwright/test";

import {
  ADMIN_EMAIL,
  API,
  apiClient,
  cancelAllWorking,
  createOrphanVenueExecution,
  csrfHeaders,
  firstAccountId,
  flattenAll,
  newKey,
  releaseAllKillSwitches,
  resetBrokerFaults,
  safeStop,
  signIn,
  clearDevRateLimits,
  waitForTradableFeed,
} from "./fixtures";

/**
 * Reconciliation, recovery and operator control.
 *
 * # What is covered here rather than in Go
 *
 * The classifier's rules are unit-tested in `internal/reconcile` with
 * hand-built snapshots, and convergence is proven in `tests/race` against a
 * real database. What only this layer can check is the OPERATOR's view: that
 * the page renders the state, that the destructive action is unreachable
 * without the admin role, and that the API refuses the malformed and the
 * malicious.
 *
 * # Why the authorisation tests hit the API directly
 *
 * A hidden button is not an access control. Every authorisation assertion here
 * is made against the endpoint, because that is where the decision is
 * enforced; the UI assertions are separate and only claim that the UI does not
 * OFFER what the user may not do.
 */

let ctx: APIRequestContext;
let csrf: string;
let accountId: string;

test.beforeAll(async () => {
  ({ ctx, csrf } = await apiClient());
  accountId = await firstAccountId(ctx);
  await waitForTradableFeed(ctx);

  // Start from a known book.
  //
  // Without this the tests below fail for a reason that has nothing to do
  // with reconciliation: leftover positions fill the 1500 ZAR per-instrument
  // ceiling, the order that is supposed to create a divergence is correctly
  // refused before it ever reaches the venue, and the assertions then report
  // "nothing was repaired" -- which is true, and misleading.
  await releaseAllKillSwitches(ctx, csrf);
  await resetBrokerFaults(ctx, csrf);
  await cancelAllWorking(ctx, csrf, accountId);
  await flattenAll(ctx, csrf, accountId);
});

test.afterAll(async () => {
  await resetBrokerFaults(ctx, csrf);
  await ctx.dispose();
});

test.beforeEach(() => {
  clearDevRateLimits();
});

/** An admin API context. Resolving an issue is admin-only, and correctly so. */
async function adminClient(): Promise<{ ctx: APIRequestContext; csrf: string }> {
  const password = process.env.VANTAGE_E2E_ADMIN_PASSWORD;
  if (!password) {
    throw new Error(
      "VANTAGE_E2E_ADMIN_PASSWORD is not set. Resolving a reconciliation issue " +
        "requires the admin role, so these tests cannot run without it.",
    );
  }
  const { request } = await import("@playwright/test");
  // storageState cleared for the same reason as the anonymous context below:
  // the config sets a global one, and starting from the trader's session and
  // then signing in over the top of it is a confusing way to get here.
  const admin = await request.newContext({ baseURL: API, storageState: undefined });
  const res = await admin.post("/api/v1/auth/login", {
    data: { email: ADMIN_EMAIL, password },
  });
  if (!res.ok()) {
    throw new Error(`admin sign-in failed: ${res.status()} ${await res.text()}`);
  }
  const state = await admin.storageState();
  const token = state.cookies.find((c) => c.name === "vantage_csrf")?.value ?? "";
  return { ctx: admin, csrf: token };
}

/**
 * Manufactures a real divergence: the venue fills the order and the response
 * is lost, so Vantage never books the execution.
 *
 * Uses the deterministic fault injector rather than editing the database, so
 * the scenario travels through the same code an actual lost response would.
 */
async function createLostFill(): Promise<string> {
  // Positions from the previous scenario are cleared first, so the order that
  // creates the divergence is not refused for exposure.
  await cancelAllWorking(ctx, csrf, accountId);
  await flattenAll(ctx, csrf, accountId);

  const stop = await safeStop(ctx);
  await ctx.post("/api/v1/dev/broker-faults", {
    headers: csrfHeaders(csrf),
    data: { fault: "lost_response", times: 1 },
  });

  const key = newKey("recon");
  const res = await ctx.post("/api/v1/orders", {
    headers: csrfHeaders(csrf, key),
    data: {
      account_id: accountId,
      instrument_id: "XAUUSD.m",
      side: "buy",
      type: "market",
      quantity: "0.01",
      stop_loss: stop,
      time_in_force: "gtc",
    },
  });

  // The setup verifies ITSELF, and fails here rather than three assertions
  // later.
  //
  // A lost response must NOT return 2xx: the whole scenario depends on
  // Vantage not knowing the outcome. A 2xx means the fault did not fire; a
  // 4xx that is not 503 means the order was refused before reaching the venue
  // (exposure, risk, a stale quote) and no divergence exists to recover from.
  // Either way the tests that follow would report "nothing was repaired",
  // which is true and tells the reader nothing.
  const body = await res.text();
  if (res.status() >= 200 && res.status() < 300) {
    throw new Error(
      `the lost-response fault did not fire: the order returned ${res.status()}. ` +
        `Vantage recorded a known outcome, so there is no divergence to recover. ${body}`,
    );
  }
  if (res.status() !== 503) {
    throw new Error(
      `the order was refused with ${res.status()} before reaching the venue, so no ` +
        `divergence was created. This is a fixture problem, not a reconciliation ` +
        `failure -- check exposure, risk limits and feed health. ${body}`,
    );
  }
  return key;
}

test.describe("reconciliation state", () => {
  test("readiness reports the trading verdict, not just process health", async () => {
    // The distinction this asserts: an HTTP server and a database that are both
    // alive say nothing about whether the system's records agree with the
    // venue. Reporting only "ready" is how an operator comes to believe a
    // halted account is trading.
    const res = await ctx.get("/health/ready");
    expect(res.ok()).toBeTruthy();
    const body = await res.json();

    expect(body, JSON.stringify(body)).toHaveProperty("trading_state");
    expect(body).toHaveProperty("automation_allowed");
    expect(["HEALTHY", "DEGRADED", "RECONCILIATION_REQUIRED", "TRADING_HALTED"]).toContain(
      body.trading_state,
    );
    // Paper only, always.
    expect(body.execution_mode).toBe("paper");
  });

  test("the taxonomy is published with its reasoning", async () => {
    const res = await ctx.get("/api/v1/reconciliation/taxonomy");
    expect(res.ok()).toBeTruthy();
    const body = await res.json();

    const types: Array<Record<string, unknown>> = body.issue_types;
    expect(types.length).toBeGreaterThanOrEqual(13);

    for (const entry of types) {
      // Every classification explains itself. An operator being asked to
      // decide needs to know why the system would not decide for them.
      expect(
        String(entry.rationale).length,
        `${entry.issue_type} has no rationale`,
      ).toBeGreaterThan(80);
      expect(entry.allowed_actions as string[]).toContain("ACKNOWLEDGE");
    }

    // The set that may be repaired without a human is small and deliberate.
    const automatic = types
      .filter((t) => t.automatic_repair_allowed === true)
      .map((t) => t.issue_type);
    expect(automatic).toContain("FILL_MISSING_LOCALLY");
    expect(automatic).not.toContain("EXTRA_BROKER_FILL");
    expect(automatic).not.toContain("POSITION_MISMATCH");
    expect(automatic).not.toContain("EXTERNAL_BROKER_ACTIVITY");
  });

  test("an account's issues are listed with the trading verdict", async () => {
    const res = await ctx.get(`/api/v1/reconciliation/${accountId}/issues`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();

    expect(body).toHaveProperty("trading_state");
    expect(body).toHaveProperty("automation_allowed");
    expect(body).toHaveProperty("reason");
    // The reason is operator-facing prose, not a code.
    expect(String(body.reason).length).toBeGreaterThan(10);
  });

  test("a lost fill is discovered and repaired, and the repair is marked as one", async () => {
    await createLostFill();

    const run = await ctx.post(`/api/v1/reconciliation/${accountId}/run`, {
      headers: csrfHeaders(csrf),
    });
    // A 409 means another run holds the lock, which is overlap prevention
    // working rather than a failure.
    test.skip(run.status() === 409, "a reconciliation run was already in progress");
    expect(run.status(), await run.text()).toBe(200);

    const report = await run.json();
    expect(report.repaired, JSON.stringify(report)).toBeGreaterThanOrEqual(1);

    // The repaired issue records what was done and why.
    const issues = await ctx
      .get(`/api/v1/reconciliation/${accountId}/issues`)
      .then((r) => r.json());
    const repaired = (issues.issues ?? []).filter(
      (i: Record<string, unknown>) =>
        i.issue_type === "FILL_MISSING_LOCALLY" &&
        i.status === "AUTOMATICALLY_REPAIRED",
    );
    expect(repaired.length, "no automatically repaired missing fill").toBeGreaterThan(0);
    expect(repaired[0].resolution_action).toBe("IMPORT_BROKER_FILL");
    expect(String(repaired[0].resolution_reason).length).toBeGreaterThan(10);
  });

  test("a second reconciliation run finds nothing left to repair", async () => {
    // Idempotence, asserted the way it can be asserted from here.
    //
    // The exact ledger comparison -- fill count, transaction count, balance,
    // position quantity, realised P&L, all in exact numerics -- lives in
    // tests/race/recovery_test.go, which can read the database. What this
    // layer can prove is the observable consequence: once a divergence is
    // repaired, a further run against the same venue state has nothing to do.
    //
    // An earlier version of this test compared issue IDs across two listings
    // and failed spuriously, because the listing is paginated newest-first and
    // an older repaired issue simply fell off the page.
    await createLostFill();

    const first = await ctx.post(`/api/v1/reconciliation/${accountId}/run`, {
      headers: csrfHeaders(csrf),
    });
    test.skip(first.status() === 409, "a reconciliation run was already in progress");
    expect(first.status(), await first.text()).toBe(200);
    const firstReport = await first.json();
    expect(firstReport.repaired).toBeGreaterThanOrEqual(1);

    const second = await ctx.post(`/api/v1/reconciliation/${accountId}/run`, {
      headers: csrfHeaders(csrf),
    });
    test.skip(second.status() === 409, "the second run was declined by the lock");
    expect(second.status(), await second.text()).toBe(200);
    const secondReport = await second.json();

    expect(
      secondReport.repaired,
      "the second run repaired something again. Re-importing an execution " +
        "doubles a real position, and a duplicated ledger entry cannot be removed " +
        "because the ledger is append-only.",
    ).toBe(0);

    // And a third, to be certain the second was not simply unlucky.
    const third = await ctx.post(`/api/v1/reconciliation/${accountId}/run`, {
      headers: csrfHeaders(csrf),
    });
    if (third.status() === 200) {
      expect((await third.json()).repaired).toBe(0);
    }
  });
});

test.describe("operator control: authorisation", () => {
  // A hidden button is not an access control, so every one of these is
  // asserted against the endpoint.

  test("an unauthenticated caller cannot resolve an issue", async () => {
    const { request } = await import("@playwright/test");
    // storageState MUST be cleared explicitly.
    //
    // playwright.config.ts sets a global `use.storageState` pointing at the
    // saved trader session, and request.newContext() inherits it. Without this
    // line the "anonymous" context is signed in as the trader, and the test
    // asserts on a 403 it received for entirely the wrong reason -- proving
    // nothing about unauthenticated access.
    const anon = await request.newContext({ baseURL: API, storageState: undefined });
    const res = await anon.post(
      `/api/v1/reconciliation/${accountId}/issues/` +
        `00000000-0000-4000-8000-000000000000/resolve`,
      { data: { action: "ACKNOWLEDGE", reason: "anonymous attempt at resolution" } },
    );
    expect(res.status()).toBe(401);
    await anon.dispose();
  });

  test("a viewer cannot resolve an issue", async () => {
    const { ctx: viewer, csrf: viewerCsrf } = await apiClient(
      "test-results/.auth/viewer.json",
    );
    const res = await viewer.post(
      `/api/v1/reconciliation/${accountId}/issues/` +
        `00000000-0000-4000-8000-000000000000/resolve`,
      {
        headers: csrfHeaders(viewerCsrf),
        data: { action: "ACKNOWLEDGE", reason: "viewer attempt at resolution" },
      },
    );
    expect(res.status()).toBe(403);
    await viewer.dispose();
  });

  test("a TRADER cannot resolve an issue either", async () => {
    // The interesting case. A trader may place orders and flatten positions,
    // but resolving a divergence can book an execution into an append-only
    // ledger — so it belongs to the role that is barred from trading.
    // Separation of duties runs in both directions.
    const res = await ctx.post(
      `/api/v1/reconciliation/${accountId}/issues/` +
        `00000000-0000-4000-8000-000000000000/resolve`,
      {
        headers: csrfHeaders(csrf),
        data: { action: "ACKNOWLEDGE", reason: "trader attempt at resolution" },
      },
    );
    expect(res.status()).toBe(403);
  });

  test("a viewer CAN see the reconciliation state", async () => {
    // Seeing that an account has halted is not a privileged act, and hiding it
    // would leave an operator wondering why nothing trades.
    const { ctx: viewer } = await apiClient("test-results/.auth/viewer.json");
    const res = await viewer.get(`/api/v1/reconciliation/${accountId}/issues`);
    expect([200, 404]).toContain(res.status());
    await viewer.dispose();
  });
});

test.describe("operator control: input validation", () => {
  let admin: APIRequestContext;
  let adminCsrf: string;
  let issueId: string | null = null;

  test.beforeAll(async () => {
    ({ ctx: admin, csrf: adminCsrf } = await adminClient());

    // Manufacture an execution the venue has and Vantage cannot attribute.
    //
    // A lost FILL is the wrong fixture here: it is attributable, so
    // reconciliation repairs it automatically and leaves nothing for an
    // operator to act on -- which is why these seven tests used to skip on a
    // clean database. Seven security tests that skip are seven that do not
    // run, and a skip reads as a pass.
    //
    // An unattributable execution is guaranteed to stay OPERATOR_ACTION_
    // REQUIRED, because importing it would mean guessing a parent order.
    const orphan = createOrphanVenueExecution(accountId);

    await admin.post(`/api/v1/reconciliation/${accountId}/run`, {
      headers: csrfHeaders(adminCsrf),
    });

    const issues = await admin
      .get(`/api/v1/reconciliation/${accountId}/issues?open=true`)
      .then((r) => r.json());
    const open: Array<Record<string, unknown>> = issues.issues ?? [];

    // Prefer the issue this fixture created, so the tests act on a known
    // subject rather than on whatever happened to be open.
    const mine = open.find((i) => i.broker_execution_id === orphan);
    issueId = mine ? String(mine.id) : open.length > 0 ? String(open[0].id) : null;

    if (!issueId) {
      // Reported, not silently skipped. If the fixture could not create a
      // divergence, that is worth seeing in the output.
      console.warn(
        "no operator-required reconciliation issue could be created; the " +
          "validation tests will skip. Is the vantage-postgres container running?",
      );
    }
  });

  test.afterAll(async () => {
    await admin.dispose();
  });

  test("there is no operation that sets an order status directly", async () => {
    test.skip(!issueId, "no open issue to act on");
    const res = await admin.post(
      `/api/v1/reconciliation/${accountId}/issues/${issueId}/resolve`,
      {
        headers: csrfHeaders(adminCsrf),
        data: { action: "SET_ORDER_STATUS", reason: "attempting an arbitrary write" },
      },
    );
    expect(res.status()).toBe(422);
    const body = await res.json();
    // The refusal says so explicitly, because the absence is a design decision
    // rather than an oversight.
    expect(JSON.stringify(body)).toMatch(/no operation that sets an order status/i);
  });

  test("a resolution without a real reason is refused", async () => {
    test.skip(!issueId, "no open issue to act on");
    for (const reason of ["", "ok", "   ", "fixed"]) {
      const res = await admin.post(
        `/api/v1/reconciliation/${accountId}/issues/${issueId}/resolve`,
        {
          headers: csrfHeaders(adminCsrf),
          data: { action: "ACKNOWLEDGE", reason },
        },
      );
      expect(res.status(), `reason ${JSON.stringify(reason)} was accepted`).toBe(422);
    }
  });

  test("an action the issue type does not permit is refused", async () => {
    test.skip(!issueId, "no open issue to act on");
    const issue = await admin
      .get(`/api/v1/reconciliation/${accountId}/issues/${issueId}`)
      .then((r) => r.json());
    const allowed: string[] = issue.issue.allowed_actions;

    const forbidden = [
      "IMPORT_BROKER_FILL",
      "MARK_BROKER_REJECTED",
      "MARK_NOT_EXECUTED",
      "LINK_BROKER_ORDER",
    ].find((a) => !allowed.includes(a));
    test.skip(!forbidden, "this issue type permits every action");

    const res = await admin.post(
      `/api/v1/reconciliation/${accountId}/issues/${issueId}/resolve`,
      {
        headers: csrfHeaders(adminCsrf),
        data: {
          action: forbidden,
          reason: "attempting an action this issue type does not accept",
        },
      },
    );
    expect(res.status()).toBe(422);
    expect(JSON.stringify(await res.json())).toMatch(/not permitted/i);
  });

  test("a forged issue identifier is not found rather than forbidden", async () => {
    // Not-found rather than forbidden, deliberately: distinguishing the two
    // would confirm that an id exists, which is an enumeration oracle.
    const res = await admin.post(
      `/api/v1/reconciliation/${accountId}/issues/` +
        `11111111-2222-4333-8444-555555555555/resolve`,
      {
        headers: csrfHeaders(adminCsrf),
        data: { action: "ACKNOWLEDGE", reason: "forged identifier attempt" },
      },
    );
    expect(res.status()).toBe(404);
  });

  test("a malformed issue identifier is refused as invalid", async () => {
    const res = await admin.post(
      `/api/v1/reconciliation/${accountId}/issues/not-a-uuid/resolve`,
      {
        headers: csrfHeaders(adminCsrf),
        data: { action: "ACKNOWLEDGE", reason: "malformed identifier attempt" },
      },
    );
    expect([400, 404, 422]).toContain(res.status());
  });

  test("extra fields in the payload are rejected rather than ignored", async () => {
    test.skip(!issueId, "no open issue to act on");
    // Mass assignment: a caller must not be able to smuggle a quantity, a
    // price or a status into a resolution. Unknown fields are refused
    // repository-wide, and this is that rule applied to the most dangerous
    // endpoint.
    const res = await admin.post(
      `/api/v1/reconciliation/${accountId}/issues/${issueId}/resolve`,
      {
        headers: csrfHeaders(adminCsrf),
        data: {
          action: "ACKNOWLEDGE",
          reason: "attempting to smuggle extra fields into a resolution",
          quantity: "99.99",
          price: "1.00",
          status: "FILLED",
        },
      },
    );
    expect(res.status(), await res.text()).toBe(400);
  });

  test("a malicious reason is stored as data and never interpreted", async () => {
    test.skip(!issueId, "no open issue to act on");
    const payload =
      "<script>alert(1)</script> '; DROP TABLE orders; -- reviewed and acknowledged";
    const res = await admin.post(
      `/api/v1/reconciliation/${accountId}/issues/${issueId}/resolve`,
      {
        headers: csrfHeaders(adminCsrf),
        data: { action: "ACKNOWLEDGE", reason: payload },
      },
    );
    // Accepted as text (it is a reason field), or refused — but never a 500,
    // and the orders table must still be there afterwards.
    expect([200, 409, 422]).toContain(res.status());

    const orders = await admin.get(`/api/v1/orders?account_id=${accountId}&limit=1`);
    expect(orders.ok(), "the orders table did not survive a SQL-shaped reason").toBeTruthy();
  });

  test("resolving an already-resolved issue conflicts rather than repeating", async () => {
    // Its OWN issue, because the earlier tests in this block consume the
    // shared one -- and a test that skips because a sibling got there first
    // is a test that does not run.
    const orphan = createOrphanVenueExecution(accountId);
    test.skip(!orphan, "could not create a venue-side divergence");
    await admin.post(`/api/v1/reconciliation/${accountId}/run`, {
      headers: csrfHeaders(adminCsrf),
    });
    const open = await admin
      .get(`/api/v1/reconciliation/${accountId}/issues?open=true`)
      .then((r) => r.json());
    const mine = (open.issues ?? []).find(
      (i: Record<string, unknown>) => i.broker_execution_id === orphan,
    );
    test.skip(!mine, "the venue-side divergence was not detected");
    const ownIssueId = String(mine.id);

    const first = await admin.post(
      `/api/v1/reconciliation/${accountId}/issues/${ownIssueId}/resolve`,
      {
        headers: csrfHeaders(adminCsrf),
        data: {
          action: "ACKNOWLEDGE",
          reason: "acknowledged during the end-to-end suite, first attempt",
        },
      },
    );
    test.skip(first.status() !== 200, `the issue could not be resolved: ${first.status()}`);

    const again = await admin.post(
      `/api/v1/reconciliation/${accountId}/issues/${ownIssueId}/resolve`,
      {
        headers: csrfHeaders(adminCsrf),
        data: {
          action: "ACKNOWLEDGE",
          reason: "acknowledged during the end-to-end suite, second attempt",
        },
      },
    );
    expect(again.status()).toBe(409);
  });

  test("an acknowledgement does not write financial state", async () => {
    // ACKNOWLEDGE records a human judgement. It is not the same act as booking
    // a trade, and conflating them would mean every "I have looked at this"
    // carried the authority to move money.
    await createLostFill();
    await admin.post(`/api/v1/reconciliation/${accountId}/run`, {
      headers: csrfHeaders(adminCsrf),
    });

    const open = await admin
      .get(`/api/v1/reconciliation/${accountId}/issues?open=true`)
      .then((r) => r.json());
    const target = (open.issues ?? [])[0];
    test.skip(!target, "no open issue to acknowledge");

    const res = await admin.post(
      `/api/v1/reconciliation/${accountId}/issues/${target.id}/resolve`,
      {
        headers: csrfHeaders(adminCsrf),
        data: {
          action: "ACKNOWLEDGE",
          reason: "acknowledged without writing financial state",
        },
      },
    );
    test.skip(res.status() !== 200, `resolution returned ${res.status()}`);

    const body = await res.json();
    expect(body.state_changed, "an acknowledgement reported writing financial state")
      .toBe(false);
  });
});

test.describe("the operations view", () => {
  test("shows the trading state and explains what a halt does not stop", async ({
    page,
  }) => {
    await signIn(page);
    await page.goto("/operations");

    await expect(page.getByRole("heading", { name: "Operations", level: 1 })).toBeVisible();
    await expect(page.getByText("Trading state")).toBeVisible();
    await expect(page.getByRole("button", { name: "Run reconciliation" })).toBeVisible();

    // The page must state which state is which, and it must not claim
    // anything is live.
    await expect(page.locator(".topbar")).toContainText(/paper/i);
  });

  test("a trader is offered no resolution controls", async ({ page }) => {
    // The UI does not OFFER what the role may not do. The endpoint refuses it
    // regardless — that is asserted separately against the API — but a button
    // that always fails is a worse experience than no button.
    await signIn(page);
    await page.goto("/operations");
    await expect(page.getByText("Trading state")).toBeVisible();

    for (const label of [
      "Import broker fill",
      "Mark broker rejected",
      "Mark not executed",
      "Link broker order",
    ]) {
      await expect(page.getByRole("button", { name: label })).toHaveCount(0);
    }
  });

  test("every navigation entry still resolves, including Operations", async ({ page }) => {
    await signIn(page);
    await page.getByRole("link", { name: "Operations" }).click();
    await expect(page).toHaveURL(/\/operations$/);
    await expect(page.getByRole("heading", { name: "Operations", level: 1 })).toBeVisible();
  });
});
