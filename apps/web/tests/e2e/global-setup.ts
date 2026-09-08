import { mkdirSync } from "node:fs";
import { dirname } from "node:path";

import { execFileSync } from "node:child_process";

import { request, type FullConfig } from "@playwright/test";

/**
 * Signs in once per run and saves the session for the suite to reuse.
 *
 * This is not a convenience. The login rate limit is a real control — burst
 * ten, then two per minute — and a suite that signs in per test exhausts it
 * and then reports "sign-in is broken" for twenty tests. The first run of this
 * suite did exactly that: 29 login attempts, 18 of them refused with 429.
 *
 * Widening the limit to suit the tests would have been the wrong fix. Signing
 * in once is what a real client does anyway.
 *
 * The cookie is issued by the API on `localhost`, and cookies ignore ports, so
 * the saved state is sent to the terminal on :3001 and the API on :8080 alike.
 */

export const TRADER_STATE = "test-results/.auth/trader.json";
export const VIEWER_STATE = "test-results/.auth/viewer.json";

async function saveSession(
  apiBase: string,
  email: string,
  password: string,
  path: string,
): Promise<void> {
  const ctx = await request.newContext({ baseURL: apiBase });
  const res = await ctx.post("/api/v1/auth/login", { data: { email, password } });
  if (!res.ok()) {
    throw new Error(
      `global setup could not sign in as ${email}: ${res.status()} ${await res.text()}`,
    );
  }
  const body = await res.json();
  if (body.mfa_required) {
    throw new Error(
      `${email} has multi-factor authentication enabled, so the suite cannot sign in ` +
        `unattended. Disable it on the development account or supply a TOTP secret.`,
    );
  }
  mkdirSync(dirname(path), { recursive: true });
  await ctx.storageState({ path });
  await ctx.dispose();
}

/**
 * Clears the login rate-limit buckets in a DEVELOPMENT Redis.
 *
 * The limit is a real control and is deliberately not widened to suit a test.
 * Clearing the development cache is the honest way around it, and it is a
 * no-op wherever the development container is not reachable. The smoke suite
 * does the same thing for the same reason.
 */
function clearDevRateLimits(): void {
  try {
    execFileSync(
      "docker",
      [
        "exec",
        "vantage-redis",
        "sh",
        "-c",
        "redis-cli --scan --pattern 'vantage:rl:auth_*' | xargs -r redis-cli del > /dev/null; echo cleared",
      ],
      { timeout: 15_000, stdio: "ignore" },
    );
  } catch {
    // Not reachable, or not a development stack. The run continues and will
    // fail loudly on a 429 if the bucket really is exhausted.
  }
}

export default async function globalSetup(_config: FullConfig): Promise<void> {
  const apiBase = process.env.VANTAGE_E2E_API_BASE_URL ?? "http://localhost:8080";
  clearDevRateLimits();

  const traderPassword = process.env.VANTAGE_E2E_PASSWORD;
  if (!traderPassword) {
    throw new Error(
      "VANTAGE_E2E_PASSWORD is not set. It is printed by `control-api seed` and is " +
        "deliberately not defaulted in this repository.",
    );
  }
  await saveSession(
    apiBase,
    process.env.VANTAGE_E2E_EMAIL ?? "trader@vantage.local",
    traderPassword,
    TRADER_STATE,
  );

  const viewerPassword = process.env.VANTAGE_E2E_VIEWER_PASSWORD;
  if (viewerPassword) {
    await saveSession(
      apiBase,
      process.env.VANTAGE_E2E_VIEWER_EMAIL ?? "viewer@vantage.local",
      viewerPassword,
      VIEWER_STATE,
    );
  }
}
