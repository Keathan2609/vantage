package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// The deterministic result digest of a replay run.
//
// # What it proves, and what it does not
//
// It proves that two runs produced IDENTICAL CANONICAL OUTPUT from identical
// declared inputs. That is a reproducibility claim and nothing more: a digest
// says a result can be re-derived, never that the result is good. Two runs of a
// losing strategy agree on the same digest.
//
// # What is included, and why each
//
// Decisions, orders, fills, ledger entries and the final portfolio state --
// the financial facts a run is supposed to produce. If any of them differ
// between two runs of the same inputs, something is non-deterministic and the
// platform cannot be used for research.
//
// # What is excluded, and why
//
// Database ids are generated randomly, wall-clock timestamps move with the
// machine, and request ids differ per call. Including any of them would make
// the digest differ on every run and prove nothing. The rows are therefore
// ordered by BUSINESS keys -- sequence, bar time, instrument, side, quantity --
// and identified by them too.
//
// This is the same lesson the first determinism proof learned the hard way:
// ordering by uuid produced a "difference" between two identical runs.

// ResultDigest is the canonical fingerprint of one account's financial output.
type ResultDigest struct {
	// Digest is the hex SHA-256 of the canonical form.
	Digest string
	// Components are the per-section digests, so a mismatch says WHICH part of
	// the output differed rather than only that something did.
	Components map[string]string
	// Counts are the row counts behind each section, for a human reading a
	// comparison.
	Counts map[string]int
}

// digestSection is one canonicalised query.
//
// Each is ordered by business keys ONLY. A section ordered by a generated id
// would hash differently on every run for reasons that have nothing to do with
// trading.
type digestSection struct {
	name  string
	query string
}

func digestSections() []digestSection {
	return []digestSection{
		{
			// What was decided, and why it was allowed or refused. Ordered by
			// the bar it was taken on, then by the strategy and the action.
			name: "decisions",
			query: `
				SELECT coalesce(to_char(bar_time AT TIME ZONE 'UTC', 'YYYYMMDDHH24MISS'), 'none')
				       || '|' || coalesce(strategy_id::text, 'none')
				       || '|' || coalesce(strategy_version::text, '0')
				       || '|' || instrument_id
				       || '|' || signal_action
				       || '|' || coalesce(round(confidence, 6)::text, 'none')
				       || '|' || coalesce(round(requested_quantity, 10)::text, 'none')
				       || '|' || coalesce(round(approved_quantity, 10)::text, 'none')
				       || '|' || outcome
				       || '|' || coalesce(outcome_code, '')
				       || '|' || regime
				FROM decision_snapshots WHERE account_id = $1
				ORDER BY 1`,
		},
		{
			// Every order, including the refusals: a run that refused for a
			// different reason produced a different result even if no money
			// moved.
			name: "orders",
			query: `
				SELECT instrument_id || '|' || side || '|' || type
				       || '|' || round(quantity, 10)::text
				       || '|' || round(filled_quantity, 10)::text
				       || '|' || round(avg_fill_price, 10)::text
				       || '|' || status
				       || '|' || coalesce(reject_code, '')
				       || '|' || source
				       || '|' || coalesce(strategy_id::text, 'none')
				FROM orders WHERE account_id = $1
				ORDER BY 1`,
		},
		{
			// Executions, by the venue's own identity.
			name: "fills",
			query: `
				SELECT instrument_id || '|' || side
				       || '|' || round(quantity, 10)::text
				       || '|' || round(price, 10)::text
				       || '|' || round(commission, 10)::text
				       || '|' || round(slippage, 10)::text
				       || '|' || liquidity
				FROM fills WHERE account_id = $1
				ORDER BY 1`,
		},
		{
			// The ledger, in its own sequence -- which IS a business key: the
			// order of entries is part of the result.
			name: "ledger",
			query: `
				SELECT sequence::text || '|' || type
				       || '|' || round(amount, 10)::text
				       || '|' || currency
				       || '|' || round(balance_after, 10)::text
				FROM transactions WHERE account_id = $1
				ORDER BY sequence`,
		},
		{
			// Where the account ended.
			name: "positions",
			query: `
				SELECT instrument_id || '|' || side || '|' || status
				       || '|' || round(quantity, 10)::text
				       || '|' || round(avg_entry_price, 10)::text
				       || '|' || round(realized_pnl, 10)::text
				       || '|' || round(commission, 10)::text
				FROM positions WHERE account_id = $1
				ORDER BY 1`,
		},
		{
			// Attribution, which must agree too: the same money split the same
			// way. Grouped by the dimensions a research result is read in.
			name: "attribution",
			// Grouped first, concatenated second. Concatenating inside the
			// SELECT and then grouping by ordinal positions refers to columns
			// that do not exist -- the whole expression is one column -- and
			// Postgres rejects it as an aggregate in GROUP BY.
			query: `
				SELECT grouped.strategy || '|' || grouped.regime
				       || '|' || grouped.kind
				       || '|' || round(grouped.total, 10)::text
				FROM (
					SELECT coalesce(o.strategy_id::text, 'none') AS strategy,
					       coalesce(d.regime, 'none')            AS regime,
					       t.type                                AS kind,
					       sum(t.amount)                         AS total
					FROM transactions t
					LEFT JOIN orders o ON o.id = t.order_id
					LEFT JOIN decision_snapshots d ON d.id = o.decision_id
					WHERE t.account_id = $1
					GROUP BY 1, 2, 3
				) grouped
				ORDER BY 1`,
		},
		{
			// Whether the account ended safe, which is part of the outcome: a
			// run that ended halted is not the same result as one that did not.
			name: "reconciliation",
			query: `
				SELECT issue_type || '|' || severity || '|' || status
				       || '|' || repair_class
				FROM reconciliation_issues WHERE account_id = $1
				ORDER BY 1`,
		},
	}
}

// ComputeResultDigest canonicalises and hashes one account's financial output.
func (s *TradingStore) ComputeResultDigest(ctx context.Context, accountID string) (ResultDigest, error) {
	out := ResultDigest{
		Components: map[string]string{},
		Counts:     map[string]int{},
	}
	overall := sha256.New()

	for _, section := range digestSections() {
		rows, err := s.pool.Query(ctx, section.query, accountID)
		if err != nil {
			return ResultDigest{}, mapError(err)
		}
		h := sha256.New()
		n := 0
		for rows.Next() {
			var line string
			if serr := rows.Scan(&line); serr != nil {
				rows.Close()
				return ResultDigest{}, mapError(serr)
			}
			// A newline terminator, so two adjacent values cannot be confused
			// with one longer value -- the classic way a "canonical form"
			// stops being canonical.
			fmt.Fprintf(h, "%s\n", line)
			n++
		}
		rows.Close()
		if rows.Err() != nil {
			return ResultDigest{}, mapError(rows.Err())
		}

		sum := hex.EncodeToString(h.Sum(nil))
		out.Components[section.name] = sum
		out.Counts[section.name] = n
		fmt.Fprintf(overall, "%s=%s\n", section.name, sum)
	}

	out.Digest = hex.EncodeToString(overall.Sum(nil))
	return out, nil
}

// Describe renders the digest for a comparison, shortest-first so a human can
// scan it.
func (d ResultDigest) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "digest %s\n", d.Digest[:16])
	for _, section := range digestSections() {
		fmt.Fprintf(&b, "  %-15s %s  (%d rows)\n",
			section.name, d.Components[section.name][:16], d.Counts[section.name])
	}
	return b.String()
}

// DiffersFrom names the sections that differ, for a failure message that says
// WHAT diverged rather than only that something did.
func (d ResultDigest) DiffersFrom(other ResultDigest) []string {
	var out []string
	for _, section := range digestSections() {
		if d.Components[section.name] != other.Components[section.name] {
			out = append(out, fmt.Sprintf("%s (%d rows against %d)",
				section.name, d.Counts[section.name], other.Counts[section.name]))
		}
	}
	return out
}
