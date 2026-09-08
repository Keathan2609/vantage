import { expect, type Page } from "@playwright/test";

/**
 * Shared helpers for the end-to-end suite.
 *
 * Credentials come from the environment only. `signIn` fails loudly when the
 * password is absent rather than falling back to a guess, so a misconfigured
 * run reads as "you did not set VANTAGE_E2E_PASSWORD" instead of "sign-in is
 * broken".
 */

export const EMAIL = process.env.VANTAGE_E2E_EMAIL ?? "trader@vantage.local";

export function password(): string {
  const value = process.env.VANTAGE_E2E_PASSWORD;
  if (!value) {
    throw new Error(
      "VANTAGE_E2E_PASSWORD is not set. The suite needs the seeded paper account's " +
        "password; it is printed by `control-api seed` and is never committed.",
    );
  }
  return value;
}

export async function signIn(page: Page): Promise<void> {
  await page.goto("/");
  // Already signed in from a previous test in the same context.
  if (await page.locator(".topbar").isVisible().catch(() => false)) return;

  await page.getByLabel("Email").fill(EMAIL);
  await page.getByLabel("Password").fill(password());
  await page.getByRole("button", { name: "Sign in" }).click();

  await expect(page.locator(".topbar")).toBeVisible({ timeout: 20_000 });
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
