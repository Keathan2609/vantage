# Threat model

Method: assets first, then who would want them, then the surfaces that reach
them, then what stops each attack — and what does not. Every control named here
exists in the code; residual risks are listed rather than absorbed into
optimistic wording.

## Scope

In scope: the control plane, the research service, the terminal, the database,
the local deployment, and the boundary where a real broker adapter would
eventually attach.

Out of scope, and **never to be tested**: Exness, MetaQuotes, Trading
Economics, any third-party provider, and any system not owned or explicitly
authorised. No scanning, probing or credential testing has been performed
against any external party, and none may be.

## Assets

| Asset | Why an attacker wants it | Where it lives |
| --- | --- | --- |
| Broker credentials | Direct access to a real trading account | Not present in this build; designed for versioned AEAD storage |
| Session tokens | Full control of the terminal as the operator | Browser cookie; only `sha256` stored server-side |
| Password hashes | Offline cracking, credential reuse | `users.password_hash`, Argon2id |
| MFA secrets | Defeat the second factor | `users.mfa_secret_cipher`, AES-256-GCM, bound to user id |
| The ability to place an order | Move a position; with real money, theft | Only through `internal/oms` |
| The ability to widen a limit | Turn a small loss into a large one | `PUT /risk/limits`, capped by DB constraint at 10% |
| The audit log | Hide what was done | Append-only, hash-chained |
| Strategy and model logic | Commercial value | Repository, research service |
| Market and P&L history | Position inference | `market_bars`, `transactions` |

The highest-value asset is not data. It is **the capability to execute**, which
is why the architecture spends most of its complexity restricting one code
path.

## Adversaries

| Adversary | Capability | Motivation |
| --- | --- | --- |
| Opportunistic internet scanner | Unauthenticated HTTP | Any exposed system |
| Credential attacker | Password lists, stolen reuse | Account takeover |
| Malicious website the operator visits | Cross-site requests, browser APIs | Drive the terminal with the operator's session |
| Dependency compromise | Code execution inside a service | Anything the process can reach |
| Malicious or careless strategy code | Runs inside the research plane | Execute trades, read secrets |
| Someone with database access | Read and write SQL | Read secrets, rewrite history |
| The operator, mistaken | Full legitimate authority | Not malicious, but the most likely cause of loss |

The last row is not a joke. On a R500 account with a 100-ounce gold contract,
the realistic path to ruin is a fat-fingered size or a widened limit, not an
attacker.

## Surface: unauthenticated HTTP

**Attacks.** Endpoint enumeration, login brute force, JSON parser abuse, body
flooding, error-message reconnaissance.

**Controls.** Three unauthenticated endpoints only (`/health/live`,
`/health/ready`, `/version`), and none of them expose account data. Login is
rate-limited per IP and lockout-protected per account. Bodies are capped at
256 KiB with a 30-second request timeout. Errors carry a code and a sentence,
never a stack trace, SQL fragment or internal id; the request id is the link to
the detailed server-side log.

**Residual.** `/version` reveals the build and execution mode. That is a
deliberate trade: an operator being able to confirm "paper" from outside the
UI is worth more than the trivia it leaks.

## Surface: authenticated API

**Attacks.** Horizontal privilege escalation (act on another user's account),
vertical escalation (viewer acting as trader), mass assignment, IDOR on order
and position ids, parameter tampering on sizes and limits.

**Controls.** Role middleware before the handler; account resolution *through*
the authenticated user rather than from the request; unknown JSON fields
rejected so mass assignment fails loudly; every id parsed and then re-resolved
under ownership; risk limits capped by a database CHECK regardless of what the
API accepts.

**Verified.** `tests/smoke/smoke.py` asserts that a cross-tenant read is
refused, that an admin cannot place an order, and that a viewer cannot mutate.

**Residual.** Authorisation is checked at the handler boundary. A future
handler that forgets `accountForRequest` would not be caught by a test that
does not exist yet; the mitigation is that there is exactly one helper and it
is used uniformly today.

## Surface: the browser

**Attacks.** XSS to steal the session, CSRF to place an order, clickjacking,
cache-mining a shared machine, dependency-delivered script injection.

**Controls.** Session in an `HttpOnly` cookie, so script cannot read it. CSRF
double-submit with a constant-time comparison. `X-Frame-Options: DENY` and
`frame-ancestors 'none'`. `Cache-Control: no-store` on API responses. A CSP on
the web app that permits no third-party origin at all, and no charting library
to compromise — the candles are hand-drawn SVG.

**Residual.** `'unsafe-inline'` remains in the web app's `script-src` because
the App Router injects inline bootstrap scripts. A nonce-based policy would
remove it and is the obvious next hardening step. The API itself allows
nothing.

## Surface: the research plane

This is the surface the architecture is most opinionated about.

**Attacks.** A strategy tries to place an order directly; a compromised Python
dependency reaches for broker credentials; research code widens a risk limit;
model inference is manipulated into an unfiltered signal.

**Controls.**

1. The research service holds **no broker client**. There is no adapter in the
   process to call.
2. It has **no credential** pointing back at the control plane. The trust flows
   one way: control plane → research service.
3. Its database role has `SELECT` on market and research tables and **nothing
   on** `users`, `sessions`, `accounts`, `orders`, `fills`, `transactions`,
   `trading_authorities`, `kill_switches` or `broker_connections`.
4. A strategy returns a *signal*: an action, a confidence and an explanation.
   It cannot express a size that risk must honour — the orchestrator sizes from
   the account's budget and ignores any suggestion.
5. Inference failure is fail-closed: `FeatureUnavailableError` produces NO
   TRADE, so "the model is broken" never becomes "trade without the filter".

**Verified.** The role boundary is exercised by the smoke suite, which attempts
research-role writes and confirms Postgres refuses them.

**Residual.** The research service can influence *whether* a trade is
considered, and a strategy that consistently produced bad-but-permitted signals
would still lose money within the risk budget. Risk limits bound the damage;
they cannot make a bad strategy good.

## Surface: the database

**Attacks.** SQL injection; privilege escalation to superuser; direct
modification of balances; deletion of audit rows.

**Controls.** Every statement parameterised through pgx — no string
concatenation of user input into SQL anywhere in `internal/store`. The
application role is not a superuser and cannot execute DDL. Append-only tables
have UPDATE and DELETE revoked *and* trigger enforcement. Financial invariants
are CHECK constraints and unique indexes, so a bug in application code cannot
write an impossible row.

**Residual.** A party with `vantage_owner` or superuser access can do anything,
including rewriting audit rows — the chain makes that detectable, not
impossible. An external append-only sink is the mitigation and is not built.

## Surface: order execution

**Attacks.** Duplicate submission (double-click, retry storm, replay); replaying
a captured request with a modified payload; racing two orders to breach a
position limit; an order placed while a kill switch is active.

**Controls.** Idempotency is a primary key on `(account_id, idempotency_key)`
claimed inside the same transaction as the order write, so a concurrent
duplicate loses the claim rather than being deduplicated after the fact. Same
key with a *different* payload is rejected as
`idempotency_key_payload_mismatch` rather than silently replaying the first.
Position uniqueness is a partial unique index, so two racing opens cannot
create parallel positions. The kill switch is checked inside the pipeline, not
in the UI.

**Verified.** Eight concurrent identical submissions produce exactly one order
(`tests/smoke/smoke.py`).

**Residual.** A broker call that times out leaves the outcome genuinely
unknown. Vantage records `FAILED`, never retries, and blocks automation until
reconciliation resolves it. That is the correct behaviour and it is still an
operational burden.

## Surface: dependencies and build

**Attacks.** Typosquatted package; compromised release of a real package;
malicious transitive dependency; poisoned container base image.

**Controls.** Pinned versions with checksums (`go.sum`, `package-lock.json`,
`pyproject.toml`); CI runs Gitleaks, Semgrep, Trivy, OSV-Scanner,
`govulncheck`, `pip-audit` and `npm audit`; container images run as a
non-root user with a read-only root filesystem and no shell in the final
stage; the web app pulls no runtime dependency beyond React and Next.

**Residual.** Scanning finds *known* vulnerabilities. It does not find a
malicious package that has not yet been reported.

## Surface: operator error

**Controls.** A mandatory review step before submission, with the notional,
margin, risk amount and every check shown. A stop-loss requirement enforced by
risk, not by discipline. Risk-per-trade capped at 10% by the database. Kill
switch separated from Flatten, so "stop" never surprises anyone by
liquidating. Every refusal explains itself in a sentence. Confirmation required
for closing a position.

**Residual.** An operator with the `trader` role can widen limits up to the
database ceiling and take a 10%-per-trade risk. That is their decision to make;
the platform makes it visible, audited and reversible rather than impossible.

## What paper mode removes from this model

Because no real venue is reachable, several otherwise-critical threats do not
currently apply: theft of broker credentials (none exist), unauthorised
real-money execution (no adapter), and market-abuse exposure (no real orders).
They are listed here so that enabling a real adapter is understood as
re-opening this model, not as a configuration change.

The controls that would need to be in place first are named in
`docs/BROKER_ADAPTERS.md` and `docs/COMPLIANCE_READINESS.md`.
