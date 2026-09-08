-- 0011: close the discrepancies carried over from the superseded table.
--
-- 0010 copied every historical row out of reconciliation_discrepancies into
-- reconciliation_issues so the history was not lost, and imported the
-- unresolved ones as OPEN. That was wrong, and the first run against a
-- development database showed why: 54 of 63 open issues were legacy rows, and
-- they halted the account permanently.
--
-- Two things make a migrated row unresolvable:
--
--   * Its evidence is a summary, not a snapshot. The old table stored two
--     opaque strings, so the migrated local_snapshot is
--     `{"value": "ACCEPTED"}` -- enough to know something differed once, not
--     enough for an operator to decide anything or for RECHECK to re-derive.
--   * It has no fingerprint that a fresh detection can match. Migrated rows
--     carry `migrated:<old id>` precisely so the partial unique index would
--     not reject the copy, which means a new run detecting the SAME live
--     divergence creates a separate issue and the legacy row stays open beside
--     it forever.
--
-- So the account was halted by rows whose only available resolution was an
-- operator acknowledging several dozen of them one at a time.
--
-- Closing them loses nothing. Every divergence that is still real is
-- re-detected by the next run, in the new taxonomy, with a real snapshot and a
-- stable fingerprint -- and the closed rows remain queryable as the history
-- they were preserved for.

UPDATE reconciliation_issues
SET status = 'RESOLVED',
    resolved_at = now(),
    resolution_action = 'RESOLVE_MANUALLY',
    resolution_reason =
        'Superseded by the reconciliation issue subsystem. This row was migrated from '
        || 'reconciliation_discrepancies, which recorded a summary rather than a venue '
        || 'snapshot, so it carries no evidence an operator or a re-check could act on. '
        || 'Any divergence still present is re-detected with full evidence by the next run.'
WHERE resolved_at IS NULL
  AND fingerprint LIKE 'migrated:%';

-- Clear the unknown-outcome flag on orders whose only unresolved issue was one
-- of the rows just closed.
--
-- Without this the orders stay flagged, the readiness verdict keeps counting
-- them as uncertain, and the account remains degraded for the same reason the
-- issues were closed.
UPDATE orders o
SET reconciliation_required = FALSE
WHERE o.reconciliation_required
  AND o.status <> 'FAILED'
  AND NOT EXISTS (
      SELECT 1 FROM reconciliation_issues i
      WHERE i.order_id = o.id AND i.resolved_at IS NULL
  );
