# Security

This document describes the controls that exist in the code today. Where a
control is partial or deferred, it says so. Nothing here is aspirational.

## Principles

1. **The database is the last line, not the only one.** Every invariant that
   matters is enforced in the domain, in SQL, and by grants — so bypassing the
   application does not bypass the rule.
2. **No custom cryptography.** `internal/crypto` composes AES-256-GCM and
   HMAC-SHA-256 from the standard library. No primitive is implemented here,
   and none should be.
3. **Fail closed.** A missing FX rate, a stale quote, an unreachable research
   service or an unresolved reconciliation discrepancy all produce a refusal,
   not a guess.
4. **Secrets have no field to leak through.** Response types do not contain
   credentials. That is stronger than remembering to redact them.

## Authentication

**Passwords.** Argon2id via `golang.org/x/crypto/argon2`, 64 MiB, 3 passes, 4
lanes, 32-byte output, 16-byte random salt, PHC-encoded so the parameters
travel with the hash and can be raised later without invalidating existing
passwords. Verification is constant-time. Password policy rejects passwords
containing the user's email or display name, and the usual weak forms.

**Sessions.** A 256-bit random token is issued to the browser in a cookie. The
server stores only `sha256(token)`, so a database dump does not yield usable
session tokens. Cookies are:

| Attribute | Value | Why |
| --- | --- | --- |
| `HttpOnly` | true (session) | An XSS bug cannot read the session |
| `Secure` | true under TLS | No plaintext transmission |
| `SameSite` | `Lax` | Blocks the common cross-site POST |
| `__Host-` prefix | under TLS | Binds the cookie to the exact host, no subdomain injection |
| `Path` | `/` | — |

**Multi-factor.** TOTP (RFC 6238) with a ±1 step window. The secret is
generated server-side, encrypted with AES-256-GCM under a versioned key, and
bound to the user id via the AEAD associated data — a ciphertext copied to
another user's row will not decrypt. Ten recovery codes are issued once; only
their HMAC-SHA-256 hashes are stored, and consumption is atomic so a code
cannot be redeemed twice.

Enrolment is two-phase: the secret is stored with `mfa_enabled = false` and only
becomes active when a code verifies. An abandoned enrolment therefore cannot
lock anyone out.

A session that has passed the password step but not the second factor is
*partially* authenticated: it can reach exactly two endpoints, `mfa/verify` and
`logout`. It can read nothing.

**Brute force.** Five consecutive failures on an account start an exponential
backoff from 30 seconds, capped at 15 minutes. This is keyed to the *account*,
which is what stops password guessing. The per-IP login rate limit (burst 10,
2 per minute) is a coarse anti-spray control on top; it is documented as such
in `internal/ratelimit` so nobody mistakes it for the primary defence.

## Authorisation

Three roles, checked by middleware before a handler runs:

| Role | Can | Cannot |
| --- | --- | --- |
| `viewer` | read | change anything |
| `trader` | read, place and cancel orders, run research, change risk limits and authority | administer users |
| `admin` | administer users, verify the audit chain, resolve reconciliation issues | **place an order** |

Administration and trading are deliberately disjoint. Compromising the
administrative role does not directly move a position.

Beyond roles, every account-scoped handler resolves the account **through the
authenticated user** (`AccountForUser`), so a valid session for user A cannot
read or act on user B's account by passing its id. This is verified in
`tests/smoke/smoke.py`.

### Why reconciliation repair is admin, not trader

An operator action such as `IMPORT_BROKER_FILL` writes to the ledger. That is
strictly more power than placing an order: an order is checked by nineteen risk
gates and executed by a venue, while a repair asserts that something already
happened. Giving it to `trader` would mean the role that can lose money could
also rewrite the record of having lost it.

The separation costs something honest: an admin cannot place the order whose
recovery they are approving, so a single-operator installation signs in twice.
That is the intended friction.

The scope resolution is deliberately different here, and it was a defect
before it was a decision. Reconciliation handlers use `accountForOperations`
rather than `accountForRequest`, because an admin owns no trading account and
every repair call therefore returned "account not found" — the endpoint was
unreachable by the only role permitted to use it. `accountForOperations`
widens to any account **for ADMIN only**; every other role still resolves
through ownership. `TestEveryReconciliationHandlerUsesTheOperationsScope` in
`internal/arch` fails the build if a reconciliation handler uses the other
one, because the failure mode is silent: the route answers, plausibly, with
the wrong thing.

Forged and cross-account issue ids are not a separate check. The account is in
the `WHERE` clause of the issue lookup, so an issue id belonging to another
account is simply not found — a 404, with no signal that the id exists.

## CSRF

Every state-changing request must echo the value of the readable `vantage_csrf`
cookie in an `X-Vantage-CSRF` header, compared in constant time against the
value bound to the session. A cross-site request can cause the cookie to be
sent but cannot read it to set the header.

SameSite is not relied on alone: its behaviour varies by browser and version,
and the double-submit pair does not.

## CORS

Exactly one configured origin is allowed, with credentials. There is no
wildcard and **no origin reflection** — reflecting `Origin` alongside
`Allow-Credentials` would let any site drive the API with the user's session,
which is precisely the attack CSRF defence exists for.

## Response headers

The API returns JSON only, so its CSP is maximally restrictive:

```
default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'
```

plus `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`,
`Referrer-Policy: no-referrer`, `Cross-Origin-Resource-Policy: same-site`,
`Cross-Origin-Opener-Policy: same-origin`, a `Permissions-Policy` that denies
geolocation, microphone, camera and payment, and
`Cache-Control: no-store, no-cache, must-revalidate, private` because financial
responses must never sit in an intermediary cache. HSTS is set in production
only, so a local HTTP setup is not pinned to HTTPS.

The web application sets its own policy in `apps/web/next.config.mjs`. It loads
no third-party script, font, style or frame. `'unsafe-eval'` appears in the
development policy only, because the Next.js dev server needs it for hot
reloading; the production build does not use `eval` and does not receive the
allowance.

## Secrets

- No secret is committed. `.env` is git-ignored; `.env.example` documents every
  variable with instructions and no values.
- The data-encryption key and session signing key are 32-byte values supplied
  by the environment. In a real deployment they belong in a KMS, and the code
  is written so that swapping the source changes one constructor.
- Keys are **versioned**. Every ciphertext carries a 5-byte header with the
  format byte and the key version, so rotation is incremental: old data stays
  readable under retired keys while new data is written under the active one.
- Broker credentials are not present in this build (the venue is a mock). The
  storage path they will use is the same versioned AEAD, and the response types
  have no field capable of carrying them.

**Never exposed through:** API responses, browser state, logs, error messages,
decision snapshots, audit metadata, metrics labels or screenshots. Structured
logging redacts by key name, and the snapshot writer stores decision inputs
only.

## Database privilege

Three roles, created by `infra/docker/postgres-init/00-roles.sql`:

| Role | Purpose | Privilege |
| --- | --- | --- |
| `vantage_owner` | owns the schema, runs migrations | DDL |
| `vantage_app` | the control plane | DML, minus UPDATE/DELETE on eight append-only tables, and no CREATE on `public` |
| `vantage_research` | the quant service | `SELECT` on market and research tables only |

The application is **not** a superuser and cannot alter the schema. The
research role is granted nothing at all on `users`, `sessions`, `accounts`,
`orders`, `fills`, `transactions`, `trading_authorities`, `kill_switches` or
`broker_connections`: research has no business reading credentials or moving
money, and the database is where that boundary is actually enforced.

Append-only tables (`audit_events`, `transactions`, `fills`,
`order_state_transitions`, `decision_snapshots`, `risk_limit_history`,
`trading_authority_history`, `strategy_lifecycle_history`) are protected twice
— by triggers that refuse UPDATE and DELETE, and by revoked grants so the
attempt fails before reaching a trigger an attacker might try to disable.

Both role boundaries are verified by `tests/smoke/smoke.py`, which attempts
twelve writes that must fail and confirms Postgres refuses each one.

## Audit

`audit_events` is append-only and hash-chained: each row's hash covers its
canonical content and the previous row's hash. Editing, deleting or reordering
a row breaks every subsequent hash, and `GET /api/v1/admin/audit/verify`
recomputes the chain and reports the first broken link.

This is tamper **evidence**, not immutability. A party with database write
access can rewrite history; what they cannot do is rewrite it without breaking
the chain. Achieving immutability would require an external append-only sink,
which is named as a gap in `docs/COMPLIANCE_READINESS.md` rather than implied
here.

Two implementation details make verification actually work: timestamps are
truncated to microseconds before hashing (Postgres cannot store nanoseconds, so
a nanosecond-precision hash would never re-verify), and metadata is stored in a
`json` column rather than `jsonb` so key order and formatting survive the round
trip.

## Operator actions on financial state

There is no endpoint that sets an order's status, and there will not be one. A
generic "set status" route is a remote-code-execution equivalent for
accounting: whatever validation the OMS performs, that route bypasses. `SET_ORDER_STATUS` is asserted **invalid** by
`TestOnlyRecognisedActionsAreValid`.

What exists instead is seven named actions, each of which knows what it means:
`ACKNOWLEDGE`, `RECHECK`, `IMPORT_BROKER_FILL`, `MARK_BROKER_REJECTED`,
`MARK_NOT_EXECUTED`, `LINK_BROKER_ORDER`, `RESOLVE_MANUALLY`. Each requires
ADMIN, requires the issue to be open, requires the action to be one the issue's
own policy permits, and requires a reason of at least ten characters — not as
bureaucracy but because the reason is the only record of *why* a human
overrode the system, and "ok" is not one.

Three properties matter more than the list:

- **The caller supplies a decision, never data.** `IMPORT_BROKER_FILL` takes no
  quantity, price, side or instrument. It reconstructs the execution from the
  evidence captured in the issue at detection time. A caller who could pass a
  price could book any trade at any price and call it recovery.
- **Failed attempts are audited too.** An illegal transition, a wrong role, a
  closed issue: the rejection is written with the attempted action and actor.
  An audit log that only records successes cannot show an attack that failed.
- **Repeat resolution is a conflict, not a second write.** The resolving
  update is conditional on the issue still being open, so two concurrent
  approvals produce one resolution and one 409.

## Input handling

- Request bodies are capped at 256 KiB and decoded with unknown-field
  rejection, so a typo in a field name is an error rather than a silently
  ignored parameter.
- Every SQL statement is parameterised through pgx. There is no string
  concatenation of user input into SQL anywhere in `internal/store`.
- Decimal inputs are parsed with `decimal.NewFromString`, never through a
  float. `NaN` and infinities are rejected.
- UUID path parameters are parsed before use, so an invalid id is a 400 rather
  than a database error.
- Errors returned to the client carry a stable machine-readable code and a
  human sentence. They never carry a SQL fragment, a stack trace or an internal
  identifier; the request id links a response to the server log that has the
  detail.

## Rate limits

Per operation, not global, because logging in and reading quotes have nothing
in common:

| Operation | Burst | Sustained |
| --- | --- | --- |
| Login | 10 | 2/min |
| MFA verify | 10 | 2/min |
| Password change | 3 | 3/10 min |
| Place order | 20 | 2/s |
| Cancel order | 30 | 3/s |
| Kill switch | 30 | 2/s |
| Control change | 10 | 0.5/s |
| Research (backtest, train, run) | 10 | 0.2/s |
| Reads | 300 | 30/s |

The kill switch is deliberately generous: an operator must never be throttled
out of stopping trading.

## Dependencies and supply chain

- Go modules are pinned with checksums in `go.sum`; `go mod verify` is part of
  CI.
- Node dependencies are installed with `npm ci` from a committed lockfile. The
  web application has four runtime dependencies (Next, React, React DOM) and
  hand-draws its charts rather than pulling a charting library.
- Python dependencies are pinned in `pyproject.toml`.
- CI runs Gitleaks (secrets), Semgrep (static analysis), Trivy (filesystem and
  images), OSV-Scanner (known vulnerabilities), `govulncheck`, `pip-audit` and
  `npm audit`. See `.github/workflows/security.yml`.

## What is not covered

Stated plainly, because a security document that lists only strengths is not
useful:

- **No penetration test has been performed** against a deployed instance. The
  OWASP checks that exist are automated scans plus targeted tests in the smoke
  suite.
- **No external immutable audit sink.** The chain detects tampering; it does
  not prevent it.
- **No hardware-backed key storage.** Keys come from the environment in this
  build.
- **Single-tenant assumptions in places.** The data model is multi-user and
  ownership is enforced per request, but the platform has been exercised with
  one operator.
- **The mock venue is not an adversary.** A real broker adapter will need
  hostile-input handling that a cooperative in-process mock does not exercise.

## Reporting

This is a single-operator system with no public deployment. If that changes, a
`SECURITY.md` contact and a disclosure window belong here before anything is
exposed.
