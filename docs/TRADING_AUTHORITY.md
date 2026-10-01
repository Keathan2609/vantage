# Trading authority

An authority is the scoped technical mandate that says what Vantage may do on
an account. Without an active one, every order is refused -- automated and
manual alike.

> Trading authority is a technical control that limits what Vantage may do on
> this account. It is not a legal or regulatory authorisation.

That notice is returned with every authority in the API payload itself
(`authority.notice`), not merely written in documentation, so no interface can
present an authority as a legal permission. See
`docs/REGULATORY_BOUNDARY.md` for why the vocabulary is restricted this way.

## What an authority constrains

| Field | Constrains |
| --- | --- |
| `mode` | Execution mode; `paper` is the only value this build accepts |
| `active` | Whether it is in force at all |
| `automation_enabled` | Whether *automated* orders may be placed under it |
| `allowed_instruments` | Which instruments; an empty list is refused, not stored |
| `allowed_order_types` | Which order types; an empty list is refused |
| `allowed_strategy_ids` | Which strategies; empty means any enabled PAPER strategy |
| `max_order_quantity` | Largest single order, in lots |
| `max_order_notional` | Largest single order, in account currency |
| `max_position_exposure` | Largest exposure per position |
| `max_leverage` | Leverage ceiling |
| `max_daily_loss` | Daily loss ceiling |
| `valid_from` / `valid_until` | The window in which it applies |

An empty allow-list is rejected rather than stored. An authority that permits
nothing is confusing rather than safe: the operator would see an active
authority and a stream of refusals with no obvious connection.

## Authority and risk limits are different things

They are checked at different gates and they answer different questions.

| | Trading authority | Risk limits |
| --- | --- | --- |
| Question | *May* Vantage do this at all? | *Should* it, given the account's state? |
| Scope | Instruments, order types, strategies, hard ceilings | Exposure, drawdown, concentration, margin, spread, event risk |
| Changes | Granted and revoked as a unit, with history | Edited field by field, with history |
| Failure | `no_trading_authority`, `..._not_permitted_by_authority` | `risk_limit_breached` and specific codes |

Both apply, and **whichever is smaller wins**. An authority permitting 0.10
lots does not override a risk engine that sizes 0.02, and a generous risk limit
does not let an order exceed the authority's ceiling.

## Lifecycle

```
grant ──▶ active ──┬──▶ narrowed (PATCH, version-checked)
                   ├──▶ expired (valid_until passes)
                   └──▶ revoked (explicit, with a reason)
```

- **Grant** (`POST /authority`) validates every instrument and order type
  against the catalogue, requires positive numeric ceilings, and requires
  `valid_until` to be in the future if given. Only one authority can be active
  per account.
- **Narrow** (`PATCH /authority/{id}`) can toggle automation and change the
  instrument and strategy lists. It carries an optimistic version so two
  editors cannot silently overwrite each other; a stale version is a 409, not a
  last-write-wins.
- **Revoke** (`POST /authority/{id}/revoke`) takes a reason, defaulting to
  "revoked by user" rather than an empty string, and the response says plainly
  that new orders will now be refused until a new authority is granted.
- **Expiry** is a timestamp comparison at check time, not a background job, so
  an expired authority stops working immediately whether or not any scheduler
  is running.

Every change is written to `trading_authority_history` (append-only) and
audited with the actor and the outcome.

## Three independent stops

An order can be stopped by three different mechanisms, and they are
deliberately *not* combined into one flag:

| Mechanism | Scope | Typical use |
| --- | --- | --- |
| Account `trading_enabled` | One account | "This account is under review" |
| Authority `active` / `automation_enabled` | The mandate | "Stop the robots, I will trade by hand" |
| Kill switch | Global, account or strategy | "Something is wrong, stop now" |

Collapsing them would force an operator to revoke a mandate they want back in
ten minutes just to pause one account for an afternoon. Separately, each has a
different audit meaning, which matters when reconstructing an incident.

## What automation cannot do

- It cannot create or widen an authority. Those endpoints require the `trader`
  or `admin` role and a session; the scheduler has neither.
- It cannot exceed the ceilings. The OMS checks the authority on every order,
  including orders it generated itself.
- It cannot act while `automation_enabled` is false: the check produces
  `automation_disabled` and the strategy run records a refusal.
- It cannot act outside `allowed_instruments`, even for an instrument its
  strategy declares -- the authority is the narrower of the two.

## Seeding, and one thing worth knowing

The development seed creates an authority for the paper account. It will
**widen an existing authority only through an explicit, audited reconcile
step** -- early on it silently left an existing narrow authority in place, which
made a freshly seeded environment refuse trades for reasons that were correct
but invisible. The fix was to make the widening explicit and audited rather
than to make the seed overwrite quietly.

## Multi-user

Authorities are per user *and* per account (`user_id`, `account_id`), and every
authority endpoint resolves the account through the authenticated user. A valid
session for one user cannot grant, narrow or revoke an authority on another
user's account, and the smoke suite verifies the cross-tenant refusal.
