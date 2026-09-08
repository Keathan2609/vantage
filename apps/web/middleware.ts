import { NextResponse, type NextRequest } from "next/server";

/**
 * Content-Security-Policy for the terminal.
 *
 * Only the Content-Security-Policy lives here, because it is the one header
 * that will need a per-request nonce when the framework supports it. The
 * request-independent headers are in `next.config.mjs`, which also covers the
 * static output this matcher skips.
 *
 * ## Why script-src still allows 'unsafe-inline'
 *
 * A ZAP baseline scan reported `CSP: script-src unsafe-inline [10055]`, and it
 * is a fair finding: `'unsafe-inline'` gives away most of what a CSP is for.
 *
 * A nonce-based policy was implemented and then reverted, because it did not
 * work with this stack. Next 16's Turbopack build emits its bootstrap and
 * chunk `<script src>` tags WITHOUT the nonce, so under
 * `script-src 'nonce-…' 'strict-dynamic'` the browser blocked every chunk and
 * the terminal rendered nothing — all thirty end-to-end tests failed. Adding
 * `'unsafe-inline'` alongside a nonce does not help: a browser that honours
 * the nonce ignores `'unsafe-inline'` by specification.
 *
 * Shipping a terminal that does not load, in exchange for a scanner warning,
 * would be the wrong trade. The finding is therefore accepted and recorded in
 * `.zap/baseline.conf` with this reasoning.
 *
 * What would resolve it, in preference order:
 *
 *  1. Next propagating the nonce to Turbopack-emitted script tags. Then delete
 *     `'unsafe-inline'`, add `'nonce-…' 'strict-dynamic'`, and set the CSP on
 *     the REQUEST headers too — Next reads the nonce from there, not from a
 *     custom header.
 *  2. Serving the terminal behind a proxy that rewrites the tags.
 *
 * What is NOT a mitigation: assuming the app has no XSS. The exposure is real
 * and bounded by the fact that the session cookie is HttpOnly, so an injected
 * script cannot exfiltrate a session, and by `connect-src`, which permits
 * exactly one API origin.
 */

const isProduction = process.env.NODE_ENV === "production";

const apiOrigin = process.env.NEXT_PUBLIC_VANTAGE_API_BASE_URL ?? "http://localhost:8080";

export function middleware(request: NextRequest) {
  const csp = [
    "default-src 'self'",
    // See the note above. The development server additionally needs
    // 'unsafe-eval' for hot reloading; production does not use eval.
    `script-src 'self' 'unsafe-inline'${isProduction ? "" : " 'unsafe-eval'"}`,
    // React sets style attributes directly (meter widths, sparkline colours).
    // A style injection cannot execute code: the exposure is defacement.
    "style-src 'self' 'unsafe-inline'",
    "img-src 'self' data:",
    "font-src 'self'",
    `connect-src 'self' ${apiOrigin}`,
    "frame-ancestors 'none'",
    "form-action 'self'",
    "base-uri 'self'",
    "object-src 'none'",
    "worker-src 'self' blob:",
    "manifest-src 'self'",
  ].join("; ");

  const response = NextResponse.next({ request });
  response.headers.set("Content-Security-Policy", csp);

  // The cross-origin isolation headers are set in next.config.mjs instead, so
  // they also cover /_next/static, which this matcher skips. One owner per
  // header: two places setting the same header is how they drift apart.
  return response;
}

export const config = {
  // Everything except Next's own immutable static output.
  matcher: ["/((?!_next/static|_next/image|favicon.ico).*)"],
};
