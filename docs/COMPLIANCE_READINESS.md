# Compliance readiness

This document maps the platform's **technical capabilities** to the kinds of
obligation a financial-services operation typically has to satisfy, and states
what is missing.

It reaches no legal conclusion. Read `docs/REGULATORY_BOUNDARY.md` first: the
architecture is *compliance-supporting*, meaning it produces the records and
controls an obligation would need. Whether any obligation applies, and whether
these capabilities satisfy it, is a question for a qualified adviser.

## Reading the tables

| Marker | Meaning |
| --- | --- |
| **Built** | Implemented and exercised by tests |
| **Partial** | Implemented, with a stated limitation |
| **Not built** | Absent; named so it is not assumed |

## Record-keeping and auditability

| Capability | State | Detail |
| --- | --- | --- |
| Every order recorded with actor, time, inputs, outcome | Built | `orders`, `order_state_transitions`, `decision_snapshots` |
| Every refusal recorded with its reason | Built | `risk_events`, `decision_snapshots` -- a rejected order is a first-class record, not a gap |
| Complete per-account ledger | Built | `transactions`, gapless sequence enforced by `transactions_account_sequence_uniq` |
| Append-only enforcement | Built | Triggers plus revoked UPDATE/DELETE grants on eight tables |
| Tamper detection | Built | SHA-256 hash chain over `audit_events`; `GET /admin/audit/verify` reports the first broken link |
| Tamper *prevention* | **Not built** | The chain detects rewriting; it cannot stop a party with database write access. An external append-only sink (object storage with retention lock, or a WORM log) is required for prevention |
| Retention policy and enforced retention period | **Not built** | Data is retained indefinitely by default; no policy engine exists |
| Time synchronisation guarantee | Partial | Timestamps come from one injected clock and are stored in UTC; no NTP attestation or trusted timestamping |
| Export of records for an examiner | Partial | Everything is queryable SQL and available through the API; no packaged export or attested report |

## Decision reconstruction

Financial supervision commonly asks *why* a trade happened. This is the
capability the platform invests in most heavily.

| Capability | State | Detail |
| --- | --- | --- |
| Signal inputs preserved | Built | Indicator readings at decision time stored in the snapshot |
| Quote used for the decision preserved | Built | Bid, ask and provider recorded on the snapshot |
| Risk verdict with every check | Built | All checks stored, not only the failing one |
| Account state at decision time | Built | Equity, exposure, margin, open positions |
| Strategy version and code identity | Built | `strategy_versions` with a version number; model versions carry a dataset hash and a random seed |
| Model reproducibility | Partial | Seed, algorithm, dataset hash and window boundaries are recorded, so a run is repeatable given the same data; the trained artefact itself is not committed, by policy |
| No secrets in snapshots | Built | Snapshot writer stores decision inputs only; no credential field exists |

## Risk management

| Capability | State | Detail |
| --- | --- | --- |
| Pre-trade risk checks | Built | 26 deterministic checks, all evaluated, on every order |
| Hard ceilings independent of application code | Built | CHECK constraints, including risk-per-trade ≤ 10% |
| Mandatory stop-loss policy | Built | Per-account flag, enforced in the engine |
| Kill switch | Built | Global, account and strategy scope; separate from position closing |
| Trading authority scoping | Built | Instruments, order types, strategies, size, notional, exposure, leverage, daily loss, expiry |
| Limit-change audit with previous values | Built | `risk_limit_history` |
| Four-eyes approval for limit changes | **Not built** | Single-operator design; a second-approver workflow would need a second role and a pending-change table |
| Automated post-trade surveillance | **Not built** | Reconciliation compares state; it does not look for abusive patterns |

## Segregation and access control

| Capability | State | Detail |
| --- | --- | --- |
| Role separation | Built | `viewer` / `trader` / `admin`, with admin structurally unable to trade |
| Per-user account isolation | Built | Accounts resolved through the authenticated user; verified by test |
| Research plane cannot execute | Built | No adapter, no credential, read-only DB role |
| Least-privilege database roles | Built | Three roles; application is not superuser and cannot run DDL |
| Multi-factor authentication | Built | TOTP with encrypted secrets and hashed recovery codes |
| MFA enforced (not merely available) | **Not built** | Enrolment is the user's choice; no policy forces it |
| Session management and revocation | Built | Per-session revocation, expiry, revoke-all on password change |
| Privileged-access logging | Built | Every admin action audited |

## Client-asset obligations

| Obligation | Applicability here |
| --- | --- |
| Client money segregation | **No client money exists.** No wallet, no deposit path, no third-party balance table |
| Reconciliation of client assets | Not applicable for the same reason; venue reconciliation exists for positions and orders |
| Custody controls | Not applicable -- non-custodial by construction |

If any of these ever *became* applicable, it would be because the boundary in
`docs/REGULATORY_BOUNDARY.md` had been crossed, which is a legal decision
before it is an engineering one.

## Operational resilience

| Capability | State | Detail |
| --- | --- | --- |
| Health and readiness endpoints | Built | Dependency-aware readiness |
| Metrics | Built | Prometheus: orders, rejections, risk utilisation, quote age, reconciliation mismatches, auth failures |
| Structured logs with redaction | Built | `slog`, JSON, request-id correlated |
| Reconciliation blocking automation on discrepancy | Built | Critical mismatch blocks automated trading |
| Backup and restore procedure | Partial | Documented in `docs/DISASTER_RECOVERY.md`; not automated, and restore has been reasoned through rather than rehearsed |
| Disaster-recovery test | **Not built** | No restore drill has been performed |
| Business-continuity plan | **Not built** | Out of scope for a single-operator local deployment |
| Incident response runbook | Partial | `docs/DISASTER_RECOVERY.md` covers failure modes and first actions; there is no on-call rota to escalate to |

## Data protection (POPIA-relevant capabilities)

| Capability | State | Detail |
| --- | --- | --- |
| Data minimisation | Built | Email, display name, role, session metadata, action audit -- nothing more |
| Purpose limitation | Built | No analytics, no marketing, no third-party telemetry, no external calls beyond providers |
| Encryption of sensitive fields at rest | Partial | MFA secrets encrypted under a versioned key; whole-database encryption is a deployment concern |
| Encryption in transit | Partial | HSTS and secure cookies in production; local development is HTTP by necessity |
| Access logging | Built | Authentication and authorisation outcomes audited |
| Right to erasure | **Conflicted, by design** | Audit and ledger rows are append-only. A deployment serving other people must resolve this in policy -- pseudonymising the actor reference is the usual answer -- and the conflict is documented rather than hidden |
| Data-subject export | Partial | All data is queryable; no packaged subject-access export |
| Breach detection | Partial | Auth failure metrics and audit chain verification; no alerting pipeline configured |

## The honest summary

What this platform genuinely provides is **evidence**: a complete, ordered,
tamper-evident record of what it did, what it refused, and why -- with the
inputs preserved well enough to reconstruct a decision months later. That is
the expensive part of most record-keeping obligations, and it is built.

What it does not provide is anything that requires an organisation rather than
software: approval workflows, retention policy enforcement, rehearsed recovery,
surveillance, and an immutable off-system record. Those are named above so
that no one reading this mistakes a well-instrumented single-operator system
for a compliance programme.
