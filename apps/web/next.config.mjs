/**
 * Next.js configuration.
 *
 * The interesting part is the header set. The terminal is a single-origin
 * application that loads no third-party script, font or frame, so the policy
 * can be strict enough to be worth having rather than a list of allowances.
 *
 * `connect-src` includes the control-plane origin because the API is served
 * from a different port in development. It is read from the environment rather
 * than hardcoded so a deployment does not have to patch this file.
 */

/** @type {string} */
const apiOrigin = process.env.NEXT_PUBLIC_VANTAGE_API_BASE_URL ?? "http://localhost:8080";

// Next.js's development server evaluates strings as JavaScript for hot
// reloading, which needs 'unsafe-eval'. That relaxation is confined to the dev
// server: a production build does not use eval, and shipping the allowance
// would hand any injected string a way to execute.
const devEval = process.env.NODE_ENV === "production" ? "" : " 'unsafe-eval'";

const csp = [
  "default-src 'self'",
  // Next.js injects inline bootstrap and hydration scripts. 'unsafe-inline'
  // is required for those in the App Router's default setup; it is scoped to
  // scripts from this origin only, and no third-party script is loaded at all.
  `script-src 'self' 'unsafe-inline'${devEval}`,
  // React's inline style attributes (the meter widths, the sparkline colours).
  "style-src 'self' 'unsafe-inline'",
  "img-src 'self' data:",
  "font-src 'self'",
  `connect-src 'self' ${apiOrigin}`,
  "frame-ancestors 'none'",
  "form-action 'self'",
  "base-uri 'self'",
  "object-src 'none'",
].join("; ");

const securityHeaders = [
  { key: "Content-Security-Policy", value: csp },
  { key: "X-Content-Type-Options", value: "nosniff" },
  { key: "X-Frame-Options", value: "DENY" },
  { key: "Referrer-Policy", value: "no-referrer" },
  {
    key: "Permissions-Policy",
    value: "camera=(), microphone=(), geolocation=(), payment=(), usb=()",
  },
  // The terminal is not a public page and must not be indexed or cached by an
  // intermediary.
  { key: "X-Robots-Tag", value: "noindex, nofollow" },
];

/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,
  poweredByHeader: false,
  // A standalone build is what the container image copies; it keeps the image
  // to the server plus the modules actually imported.
  output: "standalone",
  typescript: {
    // A type error must fail the build. Shipping a terminal that does not
    // typecheck is how a money-formatting change becomes a runtime crash.
    ignoreBuildErrors: false,
  },
  eslint: {
    ignoreDuringBuilds: false,
  },
  async headers() {
    return [{ source: "/(.*)", headers: securityHeaders }];
  },
};

export default nextConfig;
