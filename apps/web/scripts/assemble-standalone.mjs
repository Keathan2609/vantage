/**
 * Assemble Next's standalone output into something that can actually be run.
 *
 * # Why this script exists
 *
 * `output: "standalone"` emits a server and the modules it needs, but NOT the
 * static assets. Running `node .next/standalone/server.js` against a bare
 * standalone tree therefore answers 200 for the HTML and 404 for every chunk,
 * serving them as `text/plain`. The browser refuses to execute them, the app
 * never hydrates, and the page sits on "Connecting to the control plane..."
 * forever.
 *
 * That failure is expensive because it does not look like a missing file. It
 * looks like a broken API: around twenty Playwright tests fail on missing
 * selectors, and the obvious suspects are CORS, the session cookie and the
 * control plane. It cost real time in this repository before it was understood.
 *
 * The Dockerfile has always copied `.next/static` in. The gap was that a local
 * or scripted production run had to remember to do the same by hand, and an
 * operator step that is required but undocumented is a defect. This runs as a
 * `postbuild`, so a build always produces a runnable tree.
 *
 * Deliberately dependency-free and synchronous: it is a build step, and a copy
 * that fails must fail the build loudly rather than leave a half-assembled
 * directory that starts and then misbehaves.
 */

import { cpSync, existsSync, rmSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const webRoot = join(dirname(fileURLToPath(import.meta.url)), "..");
const next = join(webRoot, ".next");
const standalone = join(next, "standalone");

if (!existsSync(standalone)) {
  // Not an error: a build with `output` unset legitimately has no standalone
  // directory, and this script should not dictate the build mode.
  console.log("[assemble-standalone] no .next/standalone directory; nothing to assemble");
  process.exit(0);
}

/**
 * Replace rather than merge. A stale chunk from a previous build that is no
 * longer referenced is harmless, but one that IS still referenced under the
 * same name and has different content is the worst kind of bug to chase.
 */
function replace(from, to, label) {
  if (!existsSync(from)) {
    console.log(`[assemble-standalone] no ${label} to copy`);
    return false;
  }
  rmSync(to, { recursive: true, force: true });
  mkdirSync(dirname(to), { recursive: true });
  cpSync(from, to, { recursive: true });
  console.log(`[assemble-standalone] copied ${label}`);
  return true;
}

const copiedStatic = replace(
  join(next, "static"),
  join(standalone, ".next", "static"),
  ".next/static",
);
replace(join(webRoot, "public"), join(standalone, "public"), "public");

if (!copiedStatic) {
  // A standalone build with no static output means the build did not finish,
  // and serving it would reproduce exactly the failure this script exists to
  // prevent.
  console.error(
    "[assemble-standalone] .next/standalone exists but .next/static does not. " +
      "The build did not complete; refusing to leave a standalone tree that " +
      "would serve 404s for every chunk.",
  );
  process.exit(1);
}

console.log("[assemble-standalone] standalone output is runnable: node .next/standalone/server.js");
