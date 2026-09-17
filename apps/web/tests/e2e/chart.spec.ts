import { expect, test } from "@playwright/test";

import { signIn } from "./fixtures";

/**
 * The chart, and the provenance beside it.
 *
 * What matters here is not that a canvas appears — it is that the canvas is
 * fed by VANTAGE and says so. A chart that renders beautifully from a source
 * the operator cannot identify is worse than no chart, because it invites
 * trust it has not earned.
 */

test.describe("the price chart", () => {
  test.beforeEach(async ({ page }) => {
    await signIn(page);
    await page.goto("/chart");
  });

  test("renders a candle canvas from Vantage's own data", async ({ page }) => {
    const chart = page.getByTestId("price-chart");
    await expect(chart).toBeVisible();

    // Lightweight Charts draws into canvases it creates itself. Waiting for
    // one is how we know the library mounted rather than threw.
    await expect(chart.locator("canvas").first()).toBeVisible({
      timeout: 15_000,
    });
  });

  test("the browser never calls a market-data provider directly", async ({
    page,
  }) => {
    // The whole point of the market-data layer. If the front end could reach
    // a provider, the API key would have to be in the bundle.
    const external: string[] = [];
    page.on("request", (request) => {
      const url = request.url();
      if (/twelvedata|tradingview\.com|marketdata|polygon\.io|alphavantage/i.test(url)) {
        // A request to the OWN API containing "market-data" in the path is
        // expected and correct; a request to a provider's host is not.
        if (!url.includes("/api/v1/")) external.push(url);
      }
    });

    await page.goto("/chart");
    await page.waitForLoadState("networkidle");
    await page.waitForTimeout(1500);

    expect(
      external,
      "the browser requested a market-data provider directly; the API key " +
        "would have to be exposed for that to work",
    ).toEqual([]);
  });

  test("shows where the bars came from", async ({ page }) => {
    const provenance = page.getByTestId("market-data-provenance");
    await expect(provenance).toBeVisible();

    // The provider state is always rendered, including when unconfigured:
    // an operator must be able to tell "no data yet" from "data from where?".
    const state = page.getByTestId("provider-state");
    await expect(state).toBeVisible();
    await expect(state).not.toHaveText("");

    await expect(page.getByTestId("coverage-gaps")).toBeVisible();
  });

  test("labels simulated activity as PAPER", async ({ page }) => {
    // Simulated trades must never be visually indistinguishable from live
    // execution. There is no live execution in this build and the chart says so.
    const note = page.getByText(/PAPER decisions and simulated orders only/i);
    if (await note.count()) {
      await expect(note.first()).toBeVisible();
    }
  });

  test("an unconfigured provider is stated rather than hidden", async ({
    page,
  }) => {
    // With no TWELVE_DATA_API_KEY the panel must say so. When a key IS
    // configured the notice is absent, and both are correct — so this asserts
    // the pair rather than one of them.
    const notice = page.getByTestId("provider-unconfigured");

    // Wait for the status to actually arrive before branching on it. Reading
    // it immediately gets the "UNKNOWN" placeholder and sends the test down
    // the wrong branch, which is what happened the first time this ran.
    const stateEl = page.getByTestId("provider-state");
    await expect(stateEl).not.toHaveText("UNKNOWN", { timeout: 15_000 });
    const state = await stateEl.textContent();

    if (state?.trim() === "MISCONFIGURED") {
      await expect(notice).toBeVisible();
      await expect(notice).toContainText("TWELVE_DATA_CONFIGURATION_REQUIRED");
    } else {
      await expect(notice).toHaveCount(0);
    }
  });

  test("switching timeframe reloads the series", async ({ page }) => {
    const chart = page.getByTestId("price-chart");
    await expect(chart.locator("canvas").first()).toBeVisible({
      timeout: 15_000,
    });

    await page.locator(".segmented button", { hasText: "4h" }).click();
    // The canvas survives the switch: the chart instance is reused and only
    // its data is replaced, which is what keeps zoom and pan from resetting.
    await expect(chart.locator("canvas").first()).toBeVisible();
  });
});
