import { expect, test } from "@playwright/test";

import { collectConsoleErrors, signIn } from "./fixtures";

/**
 * End-to-end checks for the terminal.
 *
 * These assert the properties that would be dangerous to get wrong -- that the
 * PAPER marker is always present, that no page claims live execution, that a
 * refusal is shown as a refusal, and that no navigation entry leads to an
 * empty page. They deliberately do not assert profit, accuracy or any other
 * number that depends on market data.
 */

const PAGES = [
  { path: "/", heading: "Overview" },
  { path: "/markets", heading: "Markets" },
  { path: "/chart", heading: "Chart" },
  { path: "/trade", heading: "Trade" },
  { path: "/orders", heading: "Orders" },
  { path: "/positions", heading: "Positions" },
  { path: "/portfolio", heading: "Portfolio" },
  { path: "/scanner", heading: "Scanner" },
  { path: "/strategies", heading: "Strategies" },
  { path: "/backtests", heading: "Backtests" },
  { path: "/ml", heading: "ML Lab" },
  { path: "/calendar", heading: "News & Calendar" },
  { path: "/risk", heading: "Risk" },
  { path: "/authority", heading: "Authority" },
  { path: "/connections", heading: "Connections" },
  { path: "/activity", heading: "Activity" },
  { path: "/security", heading: "Security" },
];

test.describe("terminal", () => {
  test("signs in and shows the paper marker", async ({ page }) => {
    await signIn(page);
    await expect(page.locator(".paper-badge")).toHaveText(/paper/i);
    await expect(page.locator(".topbar")).toContainText("Equity");
  });

  test("every navigation entry leads to a page with content", async ({ page }) => {
    await signIn(page);

    for (const target of PAGES) {
      const errors = collectConsoleErrors(page);
      await page.goto(target.path);

      // A panel is the unit of content on every page. A page with no panel is
      // a placeholder, which this build does not permit.
      await expect(page.locator(".panel").first()).toBeVisible({ timeout: 15_000 });
      await expect(page.locator(".paper-badge")).toBeVisible();

      const fatal = errors.filter((text) => !text.includes("favicon"));
      expect(fatal, `console errors on ${target.path}: ${fatal.join(" | ")}`).toHaveLength(0);
    }
  });

  test("reports that live execution is unavailable", async ({ page }) => {
    await signIn(page);
    await page.goto("/connections");
    await expect(page.locator(".notice").first()).toContainText(/paper/i);
    await expect(page.getByText("not available")).toBeVisible();
  });

  test("the order ticket refuses to submit without a reviewed intent", async ({ page }) => {
    await signIn(page);
    await page.goto("/trade");

    // The ticket routes through a review step; the confirming control does not
    // exist until an intent has been reviewed.
    await expect(page.getByRole("button", { name: /review order/i })).toBeVisible();
    await expect(page.getByRole("button", { name: /^confirm /i })).toHaveCount(0);
  });

  test("the risk page separates halting from closing", async ({ page }) => {
    await signIn(page);
    await page.goto("/risk");

    await expect(page.getByText(/stops NEW orders/i)).toBeVisible();
    await expect(page.getByText(/does not.*close open positions/i)).toBeVisible();
  });

  test("the strategies page states that a signal is not an order", async ({ page }) => {
    await signIn(page);
    await page.goto("/strategies");
    await expect(page.locator(".notice").first()).toContainText(/never an order/i);
  });

  test("backtests declare their fill assumptions", async ({ page }) => {
    await signIn(page);
    await page.goto("/backtests");
    await expect(page.locator(".notice").first()).toContainText(/bar AFTER the signal/i);
    await expect(page.locator(".notice").first()).toContainText(/STOP is assumed/i);
  });

  test("the ML page reports scores against a baseline", async ({ page }) => {
    await signIn(page);
    await page.goto("/ml");
    await expect(page.locator(".notice").first()).toContainText(/majority-class baseline/i);
  });
});
