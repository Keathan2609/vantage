# Observability

Three separate records, for three different questions:

| Record | Question it answers | Retention |
| --- | --- | --- |
| **Logs** | What happened in this request? | Operational |
| **Metrics** | What is the system doing right now, in aggregate? | Time series |
| **Audit** | Who did what, and can it be proven? | Permanent, append-only |

They are deliberately not merged. An audit log with debug output in it is
unusable as evidence, and a metrics store cannot answer "why was this order
refused".

## Logs

Structured JSON via `log/slog`, one line per event, with a request id
propagated through the context so every line from one request correlates.

Fields on every line: timestamp, level, message, request id, and -- where
applicable -- user id, account id, order id and instrument.

**Redaction is by key name**, applied in `internal/logging`. Passwords, tokens,
secrets, cookies and credential-shaped keys never reach an output stream. That
is a mechanism rather than a discipline: nothing relies on a developer
remembering not to log a secret.

What is logged at each level:

| Level | Used for |
| --- | --- |
| `error` | A request failed for a reason the operator must know about; an audit chain verification failure |
| `warn` | A degradation the platform handled -- a failed market-data poll, an unreachable research service, a failed calendar refresh |
| `info` | Start-up (including the execution mode, stated explicitly), order outcomes, control actions, reconciliation runs |
| `debug` | Poll-level detail, off by default |

Start-up deliberately logs the mode in full:

> starting Vantage control plane … execution_mode=paper live_trading_available=false
> note="This build executes simulated PAPER orders against a mock venue…"

An operator reading logs should never have to infer which mode the platform is
in.

## Metrics

Prometheus, on a separate internal listener -- not on the public API port, so
metrics are not reachable from wherever the terminal is.

All metrics are namespaced `vantage_<subsystem>_<name>`.

**Trading** -- subsystem `trading`

| Metric | Meaning |
| --- | --- |
| `vantage_trading_orders_accepted_total` | Orders accepted, by instrument and source |
| `vantage_trading_orders_rejected_total` | Rejections, **by reject code** |
| `vantage_trading_orders_failed_total` | Unknown outcomes -- the ones that need reconciliation |
| `vantage_trading_fills_total` | Fills recorded |
| `vantage_trading_duplicate_commands_suppressed_total` | Idempotency working |

`orders_rejected_total` by code is the most useful single trading metric: a
spike in `insufficient_margin` and a spike in `market_data_stale` are entirely
different incidents.

**Risk and account** -- subsystem `risk`

`vantage_risk_limit_utilisation_fraction` (per account, per limit),
`vantage_risk_account_equity`, `vantage_risk_account_drawdown_fraction`,
`vantage_risk_rejections_total`, `vantage_risk_kill_switch_active`.

**Market data** -- subsystem `marketdata`

`vantage_marketdata_quote_age_seconds`, `vantage_marketdata_healthy`,
`vantage_marketdata_issues_total`,
`vantage_marketdata_provider_latency_seconds`.

**Broker** -- subsystem `broker`

`vantage_broker_call_duration_seconds`, `vantage_broker_errors_total`,
`vantage_broker_up`.

**Reconciliation** -- subsystem `reconciliation`

`vantage_reconciliation_runs_total`,
`vantage_reconciliation_mismatches_total`,
`vantage_reconciliation_unresolved_discrepancies`.

**Research** -- subsystems `strategy` and `ml`

`vantage_strategy_runs_total`, `vantage_strategy_signals_total`,
`vantage_strategy_run_duration_seconds`, `vantage_ml_predictions_total`,
`vantage_ml_inference_duration_seconds`.

**HTTP and security** -- subsystems `http` and `security`

`vantage_http_requests_total`, `vantage_http_request_duration_seconds`,
`vantage_http_requests_in_flight`, `vantage_security_auth_successes_total`,
`vantage_security_auth_failures_total` (by reason),
`vantage_security_authorization_denied_total`,
`vantage_security_rate_limited_total`.

**Build**

`vantage_build_info` carries the version, commit and execution mode as labels,
so a dashboard can show which build is running and in which mode.

## Health endpoints

| Endpoint | Reports |
| --- | --- |
| `/health/live` | The process is up |
| `/health/ready` | Dependencies are usable: database, broker adapter, research service, plus the execution mode |
| `/version` | Version, commit, execution mode, simulated funds, `live_trading_available` |

Readiness is dependency-aware rather than a constant `200`. A control plane
that reports ready with an unreachable database will accept an order and then
fail to record it.

## The audit trail

Covered in detail in `docs/SECURITY.md`. In summary: append-only, hash-chained,
verified on demand by `GET /api/v1/admin/audit/verify`, which reports the first
broken link rather than a boolean. It is tamper **evidence**, not immutability.

The Activity page shows the chain hash per row and checks sequence contiguity
in the browser, so a gap -- rows removed from the tail, which a chain check
alone would not reveal -- is visible.

## What to alert on

Nothing is wired to an alerting system in this build. That is a real gap, named
in `docs/COMPLIANCE_READINESS.md`. When it is wired, these are the conditions
that warrant waking someone, in priority order:

1. **The audit chain fails verification.** The record of what happened has been
   altered. Stop trading.
2. **`vantage_reconciliation_unresolved_discrepancies > 0` for more than one cycle.**
   The position book is known to be wrong.
3. **`vantage_trading_orders_failed_total` increasing.** Outcomes are unknown, and each one
   blocks automation until resolved.
4. **`vantage_risk_kill_switch_active == 1` unexpectedly.** Someone or something stopped
   trading.
5. **`vantage_risk_account_drawdown_fraction` within 20% of its limit.** Not yet a breach;
   the last moment intervention is cheap.
6. **`vantage_marketdata_quote_age_seconds` above the stale threshold during market hours.** The
   feed is gone and automation has silently stopped.
7. **`vantage_security_auth_failures_total` rising sharply.** Credential attack.

Deliberately *not* alert-worthy: a rejected order (that is the system working),
a NO TRADE signal (the common outcome on a small account), or a single failed
market-data poll (the next one usually succeeds).

## Tracing

Not implemented. With three services and a synchronous request path, the
request id in the logs answers the same question at a fraction of the
operational cost. Distributed tracing becomes worth it when the call graph is
deep enough that reading logs stops working -- which is a reason to add it
later, not now.

## Dashboards

None are committed. The metric names above are stable and are the contract a
dashboard would build on; a Grafana JSON file in the repository would rot
faster than the metrics it displays.
