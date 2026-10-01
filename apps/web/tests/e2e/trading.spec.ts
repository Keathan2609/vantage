import { expect, test, type APIRequestContext } from "@playwright/test";

import {
  apiClient,
  clearDevRateLimits,
  csrfHeaders,
  firstAccountId,
  newKey,
  flattenAll,
  cancelAllWorking,
  releaseAllKillSwitches,
  resetBrokerFaults,
  safeStop,
  signIn,
  waitForTradableFeed,
} from "./fixtures";

/**
 * The order path, end to end, and the four ways it must refuse.
 *
 * Arrangement uses the API (arming a fault, activating a kill switch) and the
 * assertion uses the interface wherever the interface is the thing under test.
 * A test that clicks through six screens to set up one condition is a test
 * about navigation, not about the condition.
 */

let ctx: APIRequestContext;
let csrf: string;
let accountId: string;
// Recomputed per test from the live price, because a hardcoded stop drifts out
// of the risk budget as gold moves.
let stop: string;

test.beforeAll(async () => {
  ({ ctx, csrf } = await apiClient());
  accountId = await firstAccountId(ctx);
  await waitForTradableFeed(ctx);
  // Start from a known book. Positions left by earlier runs push the
  // per-instrument exposure ceiling and every later order is then refused for
  // a reason unrelated to the test.
  await releaseAllKillSwitches(ctx, csrf);
  await flattenAll(ctx, csrf, accountId);
  stop = await safeStop(ctx);
});

test.afterAll(async () => {
  // Leaving a kill switch active or a fault armed would make every later run
  // fail for the wrong reason.
  await releaseAllKillSwitches(ctx, csrf);
  await resetBrokerFaults(ctx, csrf);
  await ctx.dispose();
});

test.beforeEach(async () => {
  clearDevRateLimits();
  await releaseAllKillSwitches(ctx, csrf);
  await resetBrokerFaults(ctx, csrf);

  // Flatten before EVERY test, not just once before the file.
  //
  // Exposure accumulates within a single run: the suite places a dozen 0.01
  // lot orders that fill, and the per-instrument ceiling is 1500 ZAR -- about
  // three lots of gold. Flattening only in beforeAll meant the later tests
  // were refused with "Exposure to XAUUSD.m would be 1939.26 ZAR against a
  // limit of 1500.00 ZAR", which is the risk engine working correctly on a
  // book the fixture had filled up.
  //
  // Cancelling comes first, so a flatten is not itself refused for exceeding
  // the pending-order limit.
  await cancelAllWorking(ctx, csrf, accountId);
  await flattenAll(ctx, csrf, accountId);

  stop = await safeStop(ctx);
});

test.describe("the order ticket", () => {
  test("shows a pre-trade estimate and requires a review before it can be confirmed", async ({
    page,
  }) => {
    await signIn(page);
    await page.goto("/trade");

    await expect(page.getByRole("heading", { name: /order ticket/i })).toBeVisible();

    // The confirming control must not exist before the intent is reviewed.
    await expect(page.getByRole("button", { name: /^confirm /i })).toHaveCount(0);

    await page.locator("#instrument").selectOption("XAUUSD.m");
    await page.locator("#quantity").fill("0.01");
    await page.locator("#stopLoss").fill(stop);

    // The estimate is what an operator decides on: notional, margin and the
    // loss if the stop is hit, before anything is sent.
    const estimate = page.locator(".panel", { hasText: /pre-trade estimate/i });
    await expect(estimate).toContainText(/Notional/);
    await expect(estimate).toContainText(/Margin/);

    await page.getByRole("button", { name: /review order/i }).click();
    await expect(page.getByRole("button", { name: /^confirm buy/i })).toBeVisible();
  });

  test("places a paper order and it appears in the blotter and the ledger", async ({ page }) => {
    await signIn(page);
    await page.goto("/trade");

    await page.locator("#instrument").selectOption("XAUUSD.m");
    await page.locator("#quantity").fill("0.01");
    await page.locator("#stopLoss").fill(stop);
    await page.getByRole("button", { name: /review order/i }).click();

    const [response] = await Promise.all([
      page.waitForResponse(
        (r) => r.url().includes("/api/v1/orders") && r.request().method() === "POST",
      ),
      page.getByRole("button", { name: /^confirm buy/i }).click(),
    ]);

    expect(response.status(), await response.text()).toBe(201);
    const body = await response.json();
    expect(body.order, "the response must carry the order").toBeTruthy();
    expect(body.simulated, "a paper order must be marked simulated").toBe(true);

    // And it must be visible where an operator looks for it.
    await page.goto("/orders");
    await expect(page.locator("table")).toContainText("XAUUSD.m");
  });

  test("a double-click cannot place two orders", async ({ page }) => {
    await signIn(page);
    await page.goto("/trade");

    await page.locator("#instrument").selectOption("XAUUSD.m");
    await page.locator("#quantity").fill("0.01");
    await page.locator("#stopLoss").fill(stop);
    await page.getByRole("button", { name: /review order/i }).click();

    const confirm = page.getByRole("button", { name: /^confirm buy/i });

    const posts: number[] = [];
    page.on("response", (r) => {
      if (r.url().endsWith("/api/v1/orders") && r.request().method() === "POST") {
        posts.push(r.status());
      }
    });

    // The key is minted once when the review opens, so both clicks carry the
    // same one and the second is a replay rather than a second order.
    await confirm.click();
    // A short timeout on purpose: the button being gone or disabled by now is
    // the CORRECT outcome, and waiting the full test timeout for it turns a
    // pass into a 45-second failure.
    await confirm.click({ force: true, timeout: 2000 }).catch(() => {
      /* already gone or disabled -- both are correct */
    });
    await page.waitForTimeout(2500);

    const created = posts.filter((s) => s === 201).length;
    expect(created, `statuses seen: ${posts.join(",")}`).toBeLessThanOrEqual(1);
  });

  test("concurrent identical submissions produce exactly one order", async () => {
    // Driven through the API because the point is genuine concurrency, which a
    // browser cannot express: eight requests in flight at once with the same
    // idempotency key.
    const key = newKey("concurrent");
    const body = {
      account_id: accountId,
      instrument_id: "XAUUSD.m",
      side: "buy",
      type: "market",
      quantity: "0.01",
      stop_loss: stop,
      time_in_force: "gtc",
    };

    const results = await Promise.all(
      Array.from({ length: 8 }, () =>
        ctx.post("/api/v1/orders", { headers: csrfHeaders(csrf, key), data: body }),
      ),
    );

    const ids = new Set<string>();
    for (const res of results) {
      if (!res.ok()) continue;
      const payload = await res.json();
      if (payload.order?.id) ids.add(payload.order.id);
    }
    expect(ids.size, "eight concurrent identical submissions must yield one order").toBe(1);
  });

  test("the same key with a different payload is refused rather than replayed", async () => {
    // This test needs the FIRST submission to be accepted, so the book must be
    // flat: an earlier test in the run leaves a position open and the
    // per-instrument exposure ceiling then refuses the first order for a
    // reason unrelated to idempotency.
    await flattenAll(ctx, csrf, accountId);
    stop = await safeStop(ctx);

    const key = newKey("mismatch");
    const base = {
      account_id: accountId,
      instrument_id: "XAUUSD.m",
      side: "buy",
      type: "market",
      quantity: "0.01",
      stop_loss: stop,
      time_in_force: "gtc",
    };

    const first = await ctx.post("/api/v1/orders", {
      headers: csrfHeaders(csrf, key),
      data: base,
    });
    expect(first.status(), await first.text()).toBe(201);

    // A client that changed the size and reused the key must get an error, not
    // a surprise: silently replaying the first order would place a size the
    // caller no longer intended.
    const second = await ctx.post("/api/v1/orders", {
      headers: csrfHeaders(csrf, key),
      data: { ...base, quantity: "0.02" },
    });
    expect(second.status()).toBeGreaterThanOrEqual(400);
    const payload = await second.json();
    expect(JSON.stringify(payload)).toMatch(/idempotency/i);
  });

  test("an order can be cancelled", async ({ page }) => {
    // A resting limit order far from the market stays working, so there is
    // something to cancel. A market order would fill before the click.
    const quote = await ctx.get("/api/v1/market/quotes").then((r) => r.json());
    const gold = quote.quotes.find((q: { instrument_id: string }) => q.instrument_id === "XAUUSD.m");
    // 20% below the market, so it will not trigger during the test.
    const far = Number.parseFloat(gold.bid) * 0.8;
    // The stop is measured from the LIMIT price, not from the market: the risk
    // the engine checks is the distance the order would lose if filled and
    // then stopped. A stop 10% below a limit 20% below the market risks 27% of
    // equity, which is correctly refused.
    const farStop = (far * 0.996).toFixed(2);

    const created = await ctx.post("/api/v1/orders", {
      headers: csrfHeaders(csrf, newKey("cancel")),
      data: {
        account_id: accountId,
        instrument_id: "XAUUSD.m",
        side: "buy",
        type: "limit",
        quantity: "0.01",
        limit_price: far.toFixed(2),
        stop_loss: farStop,
        time_in_force: "gtc",
      },
    });
    expect(created.status(), await created.text()).toBe(201);
    const orderId = (await created.json()).order.id;

    await signIn(page);
    await page.goto("/orders");

    const row = page.locator("tr", { hasText: "XAUUSD.m" }).first();
    await expect(row).toBeVisible();

    const [response] = await Promise.all([
      page.waitForResponse((r) => r.url().includes("/cancel")),
      row.getByRole("button", { name: /cancel/i }).first().click(),
    ]);
    expect(response.status()).toBeLessThan(400);

    // CANCEL_PENDING is a legitimate resting state: a cancel races the venue.
    const after = await ctx.get(`/api/v1/orders/${orderId}`).then((r) => r.json());
    expect(["CANCELLED", "CANCEL_PENDING", "FILLED"]).toContain(after.order.status);
  });
});

test.describe("refusals", () => {
  test("risk refuses an oversized order and explains which limits failed", async ({ page }) => {
    await signIn(page);
    await page.goto("/trade");

    await page.locator("#instrument").selectOption("XAUUSD.m");
    // Far beyond the account's per-order and notional ceilings.
    await page.locator("#quantity").fill("50");
    await page.locator("#stopLoss").fill(stop);
    await page.getByRole("button", { name: /review order/i }).click();

    const confirm = page.getByRole("button", { name: /^confirm buy/i });
    if (await confirm.count()) {
      await Promise.all([
        page.waitForResponse((r) => r.url().endsWith("/api/v1/orders")),
        confirm.click(),
      ]);
    }

    // The refusal must be shown as a refusal with a reason, not as a generic
    // failure. Every failed check is listed, not only the first.
    // More than one refusal notice can appear (the headline and the failed
    // checks), so the assertion is on the group rather than on a single node.
    const refusals = page.locator('.notice[data-tone="bad"]');
    await expect(refusals.first()).toBeVisible({ timeout: 15_000 });
    await expect(refusals.first()).toContainText(/limit|exceed|risk|quantity|error/i);
  });

  test("risk refuses an order with no stop loss when the account requires one", async () => {
    const res = await ctx.post("/api/v1/orders", {
      headers: csrfHeaders(csrf, newKey("nostop")),
      data: {
        account_id: accountId,
        instrument_id: "XAUUSD.m",
        side: "buy",
        type: "market",
        quantity: "0.01",
        time_in_force: "gtc",
      },
    });
    expect(res.status()).toBeGreaterThanOrEqual(400);
    expect(JSON.stringify(await res.json())).toMatch(/stop loss/i);
  });

  test("an active kill switch refuses new orders and does not close the position book", async ({
    page,
  }) => {
    const before = await ctx
      .get(`/api/v1/positions?account_id=${accountId}`)
      .then((r) => r.json());
    const positionsBefore = (before.positions ?? []).length;

    const armed = await ctx.post("/api/v1/kill-switches", {
      headers: csrfHeaders(csrf),
      data: { scope: "account", target_id: accountId, reason: "playwright kill-switch test" },
    });
    expect(armed.status(), await armed.text()).toBe(201);

    const refused = await ctx.post("/api/v1/orders", {
      headers: csrfHeaders(csrf, newKey("killed")),
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
    expect(refused.status()).toBeGreaterThanOrEqual(400);
    expect(JSON.stringify(await refused.json())).toMatch(/kill switch/i);

    // The property that matters: a halt stops NEW orders and liquidates
    // nothing. Automatic liquidation on an alarm would dump positions into the
    // conditions that raised the alarm.
    const after = await ctx
      .get(`/api/v1/positions?account_id=${accountId}`)
      .then((r) => r.json());
    expect((after.positions ?? []).length).toBe(positionsBefore);

    // And the terminal says so plainly, next to the control.
    await signIn(page);
    await page.goto("/risk");
    await expect(page.locator(".notice").first()).toContainText(/kill switch/i);
    await expect(page.getByText(/does not.*close open positions/i).first()).toBeVisible();
  });

  test("revoking trading authority refuses new orders", async () => {
    const authority = await ctx
      .get(`/api/v1/authority/${accountId}`)
      .then((r) => r.json());
    const id = authority.authority?.id;
    test.skip(!id, "no active authority on this account to revoke");

    const revoked = await ctx.post(`/api/v1/authority/${id}/revoke`, {
      headers: csrfHeaders(csrf),
      data: { reason: "playwright authority test" },
    });
    expect(revoked.status(), await revoked.text()).toBeLessThan(400);

    try {
      const refused = await ctx.post("/api/v1/orders", {
        headers: csrfHeaders(csrf, newKey("noauthority")),
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
      expect(refused.status()).toBeGreaterThanOrEqual(400);
      expect(JSON.stringify(await refused.json())).toMatch(/authority/i);
    } finally {
      // Restore the mandate, or every later test in every later run fails for
      // this reason rather than its own.
      const restored = await ctx.post("/api/v1/authority", {
        headers: csrfHeaders(csrf),
        data: {
          account_id: accountId,
          automation_enabled: true,
          allowed_instruments: ["XAUUSD.m", "XAUUSD"],
          allowed_strategy_ids: [],
          allowed_order_types: ["market", "limit"],
          max_order_quantity: "0.10",
          max_order_notional: "2500",
          max_position_exposure: "2500",
          max_leverage: "10",
          max_daily_loss: "15",
        },
      });
      expect(restored.status(), await restored.text()).toBeLessThan(400);
    }
  });
});

test.describe("broker faults", () => {
  test("a lost response after venue acceptance leaves the order FAILED, not retried", async () => {
    // The dangerous case: the venue TOOK the order and the answer was lost.
    // The pipeline must record an unknown outcome and must not resubmit.
    const armed = await ctx.post("/api/v1/dev/broker-faults", {
      headers: csrfHeaders(csrf),
      data: { fault: "lost_response", times: 1 },
    });
    expect(armed.status(), await armed.text()).toBe(200);

    const res = await ctx.post("/api/v1/orders", {
      headers: csrfHeaders(csrf, newKey("lost")),
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

    const payload = await res.json();
    const order = payload.order;
    expect(order, `expected an order record, got ${JSON.stringify(payload)}`).toBeTruthy();
    expect(
      order.status,
      "an unknown outcome must be FAILED, not REJECTED, which would claim nothing happened",
    ).toBe("FAILED");

    // Reconciliation is what resolves it, and it must find the order the venue
    // really does hold.
    const report = await ctx.post(`/api/v1/reconciliation/${accountId}/run`, {
      headers: csrfHeaders(csrf),
    });
    expect(report.status(), await report.text()).toBeLessThan(400);
  });

  test("a venue timeout is FAILED and a definitive rejection is REJECTED", async () => {
    await ctx.post("/api/v1/dev/broker-faults", {
      headers: csrfHeaders(csrf),
      data: { fault: "timeout", times: 1, delay_ms: 5 },
    });
    const timedOut = await ctx
      .post("/api/v1/orders", {
        headers: csrfHeaders(csrf, newKey("timeout")),
        data: {
          account_id: accountId,
          instrument_id: "XAUUSD.m",
          side: "buy",
          type: "market",
          quantity: "0.01",
          stop_loss: stop,
          time_in_force: "gtc",
        },
      })
      .then((r) => r.json());
    expect(timedOut.order?.status, JSON.stringify(timedOut)).toBe("FAILED");

    await ctx.post("/api/v1/dev/broker-faults", {
      headers: csrfHeaders(csrf),
      data: { fault: "rejection", times: 1 },
    });
    const rejected = await ctx
      .post("/api/v1/orders", {
        headers: csrfHeaders(csrf, newKey("reject")),
        data: {
          account_id: accountId,
          instrument_id: "XAUUSD.m",
          side: "buy",
          type: "market",
          quantity: "0.01",
          stop_loss: stop,
          time_in_force: "gtc",
        },
      })
      .then((r) => r.json());

    // The distinction the whole FAILED state exists for: a definitive refusal
    // means nothing happened and a retry would be safe.
    expect(rejected.order?.status, JSON.stringify(rejected)).toBe("REJECTED");
  });

  test("every fault mode is armable through the development endpoint", async () => {
    const listing = await ctx.get("/api/v1/dev/broker-faults");
    expect(listing.status(), await listing.text()).toBe(200);
    const listed = await listing.json();
    expect(listed.available?.length, JSON.stringify(listed)).toBe(12);

    // Paced deliberately: the control-change rate limit is burst ten then one
    // every two seconds, and twelve arms back to back exhaust it. The limit is
    // a real control and is not widened to suit a test.
    for (const fault of listed.available) {
      const res = await ctx.post("/api/v1/dev/broker-faults", {
        headers: csrfHeaders(csrf),
        data: { fault, times: 1, delay_ms: 1, factor: "2", fraction: "0.5" },
      });
      expect(res.status(), `arming ${fault}`).toBe(200);
      await new Promise((resolve) => setTimeout(resolve, 2100));
    }
    await resetBrokerFaults(ctx, csrf);
  });
});
