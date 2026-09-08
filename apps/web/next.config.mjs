/**
 * Next.js configuration.
 *
 * The static, request-independent headers live here. The
 * Content-Security-Policy does not: it carries a per-request nonce and is set
 * by middleware.ts.
 */

// Content-Security-Policy is deliberately NOT here. It needs a per-request
// nonce, which only middleware can generate, and two CSP headers would be
// enforced as their intersection — the static one has no nonce, so nothing
// would run. See middleware.ts.
const securityHeaders = [
  { key: "X-Content-Type-Options", value: "nosniff" },
  // Cross-origin isolation. Set here rather than in middleware so it also
  // covers /_next/static, which the middleware matcher deliberately skips —
  // a ZAP baseline scan flagged exactly that gap on the static chunks.
  { key: "Cross-Origin-Embedder-Policy", value: "require-corp" },
  { key: "Cross-Origin-Opener-Policy", value: "same-origin" },
  { key: "Cross-Origin-Resource-Policy", value: "same-origin" },
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
  // Next 16 writes AGENTS.md and CLAUDE.md into this directory on every dev
  // start. They are turned off deliberately.
  //
  // A generated CLAUDE.md is not inert: it becomes directory-scoped
  // instructions for anyone working in apps/web, competing with the one at the
  // repository root that was actually written on purpose. Framework-authored
  // guidance that nobody reviewed is worse than none, and a file that
  // reappears on every `npm run dev` is a permanent dirty working tree.
  agentRules: false,
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
