# Regulatory boundary

**This document is not legal advice, and nothing in this repository is.** It
describes what the software does and does not do, so that a qualified adviser
can assess it against South African law and any other applicable regime. An
architecture cannot decide its own regulatory status; only a regulator or a
competent legal professional can.

## What Vantage is, technically

- Software that runs on infrastructure controlled by its operator.
- It connects -- in this build, only to a **mock venue** -- through a broker
  adapter, using credentials the operator supplies for the operator's own
  account.
- It analyses market data, generates signals, applies risk limits and, within
  a scoped technical mandate, submits orders **for that same operator's own
  account**.
- It records what it did and why, in an append-only, hash-chained audit log.

## What Vantage is not

Each of these is a structural absence, not a policy statement. There is no code
path, table or configuration that could enable them without a deliberate
redesign:

- **It is not custodial.** It never holds, receives or transmits client money
  or client assets. There is no wallet, no deposit account, no payment
  integration and no table that could hold a third party's balance.
- **There is no pooled capital.** Every account row belongs to exactly one
  user. There is no omnibus account, no aggregate position across users, and no
  mechanism for one user's funds to fund another's trade.
- **There is no collective investment structure.** No units, no shares of a
  fund, no pro-rata distribution of returns, no subscription or redemption
  concept.
- **There is no Vantage-controlled wallet or treasury.** The platform has no
  account of its own.
- **It does not give advice.** It does not produce recommendations addressed to
  a person about the suitability of a financial product. Signals are the output
  of stated arithmetic on stated inputs, with the arithmetic shown.
- **It does not manage third-party money.** In this build there is one
  operator, and even multi-user support is per-user isolation, not
  discretionary management of someone else's capital.
- **It makes no performance claim.** No projected return, no accuracy figure,
  no "AI-powered" assertion appears anywhere in the product.

## Trading authority is a technical control

The `trading_authorities` table and the UI that manages it exist to *limit what
the software may do*: which instruments, which order types, which strategies,
what maximum size, what maximum daily loss, and until when.

Every API response carrying an authority includes this notice verbatim, and the
terminal displays it:

> Trading authority is a technical control that limits what Vantage may do on
> this account. It is not a legal or regulatory authorisation.

The distinction matters. An authority record is a configuration constraint
enforced by code. It is **not** a mandate, a power of attorney, a discretionary
management agreement, a client instruction under FAIS, or evidence of consent
for any regulatory purpose. If a deployment ever needs such an instrument, it
is a legal document produced by a professional and stored outside this system,
and this table is not it.

The vocabulary is deliberately restricted throughout the code and interface:
"authority" and "technical control" are used; "permission", "consent",
"mandate", "authorised" and "compliant" are not, because each has a specific
meaning in financial regulation that this software has no standing to invoke.

## South African context, as questions rather than conclusions

The operator is in South Africa. The following regimes are the ones a qualified
adviser would most likely need to consider. Each is stated as a question this
software cannot answer about itself:

| Regime | The question | What the software provides |
| --- | --- | --- |
| **FAIS** (Financial Advisory and Intermediary Services Act) | Does any activity constitute "advice" or an "intermediary service" to another person? | Signals are self-directed analysis on the operator's own account; no output is addressed to a third party |
| **FSCA licensing** | Does the activity require an FSP licence, and under which category? | Non-custodial, self-directed, single-account by construction |
| **CISCA** (Collective Investment Schemes Control Act) | Is anything a collective investment scheme? | No pooling, no units, no third-party capital -- structurally |
| **FICA** (Financial Intelligence Centre Act) | Do KYC/AML obligations attach? | No funds are received or transmitted; identity data is limited to an operator login |
| **POPIA** (Protection of Personal Information Act) | Is personal information processed lawfully and minimally? | See below |
| **Exchange control** | Do cross-border flows arise? | No funds move through this software at all |
| **Tax** | How are gains treated, and what records are needed? | A complete, append-only per-account ledger with a gapless sequence |

Two of these have concrete engineering answers, so they are worth stating.

**POPIA.** The personal information held is deliberately minimal: an email
address, a display name, a role, session metadata (IP address, user agent) and
audit records of actions taken. No identity documents, no financial account
numbers, no biometric data, no marketing data. Data is stored on
infrastructure the operator controls. Sessions expire and can be revoked
individually. Audit and ledger records are append-only *by design*, which means
a POPIA erasure request against them conflicts with the integrity property --
that tension is real, and a deployment serving other people must resolve it in
policy (for example by pseudonymising the actor reference rather than deleting
the event). It is named here rather than left to be discovered.

**Record-keeping.** Every order, fill, refusal, limit change, authority change
and control action is recorded with its actor, timestamp, reason and outcome.
The ledger is per-account, gapless and append-only. Whatever retention period
an adviser specifies, the data exists to satisfy it; what is missing is an
external immutable sink, covered in `docs/COMPLIANCE_READINESS.md`.

## What would change the analysis

Any of the following would materially alter the regulatory picture and must not
be treated as an incremental feature:

1. **Managing another person's account or capital.** This is the single largest
   change. It converts self-directed software into a service provided to a
   client.
2. **Receiving funds**, in any form, for any purpose.
3. **Publishing signals or performance** to anyone other than the operator.
4. **Charging for access**, particularly where the output could be read as
   advice.
5. **Enabling live execution.** This build cannot; see the four independent
   mechanisms in the README.
6. **Aggregating orders across accounts**, which introduces best-execution and
   allocation questions that do not currently exist.

Each of these should be treated as a decision requiring legal review *before*
implementation, not a change to be made and reviewed afterwards.

## Statements this project will not make

For completeness, because the temptation is real and the cost of getting it
wrong is not:

- Vantage is **not** described as "compliant", "FSCA-approved", "licensed",
  "regulated" or "legally cleared", anywhere.
- The architecture is described as **compliance-supporting** -- it produces the
  records and controls that an obligation would need -- and that is the
  strongest available claim.
- No document in this repository concludes that a licence is or is not
  required. That conclusion is not ours to draw.
