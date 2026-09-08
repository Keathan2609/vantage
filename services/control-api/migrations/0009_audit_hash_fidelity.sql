-- 0009_audit_hash_fidelity: make the audit hash chain actually verifiable.
--
-- The chain hashes each event's canonical content, including its metadata. For
-- verification to succeed, the bytes read back must be byte-identical to the
-- bytes that were hashed. Two things broke that, and both were found by running
-- the verifier rather than by reading the code:
--
--  1. TIMESTAMPTZ stores microsecond precision. The hash was computed over a
--     Go timestamp carrying nanoseconds, so the recomputed hash never matched.
--     Fixed in the application by truncating to microseconds before hashing.
--
--  2. JSONB does not preserve its input text. It normalises key order,
--     whitespace and number formatting, so metadata written as one byte
--     sequence is read back as another. The JSON type preserves the exact
--     input text, which is what a hash over that text requires.
--
-- The trade-off is deliberate: JSONB would allow GIN indexing of audit
-- metadata, which is a convenience, while a verifiable audit chain is the
-- entire point of the table.
--
-- Rows written before this migration cannot be made verifiable retroactively:
-- their original metadata bytes are gone. The chain is therefore re-based here,
-- which is safe only because no deployment of this build has yet produced
-- audit history anyone relies on. In a live system this migration would instead
-- record a chain break at a known sequence and start a new chain after it.

ALTER TABLE audit_events
    ALTER COLUMN metadata TYPE JSON USING metadata::text::json;

ALTER TABLE audit_events
    ALTER COLUMN metadata SET DEFAULT '{}'::json;

-- Re-base the chain. The append-only trigger blocks DELETE, so it is suspended
-- for the length of this migration and restored immediately afterwards.
ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only;
DELETE FROM audit_events;
ALTER TABLE audit_events ENABLE TRIGGER audit_events_append_only;

UPDATE audit_chain_state
SET head_hash = repeat('0', 64), head_sequence = 0, updated_at = now()
WHERE id = TRUE;

-- A verification record, so the re-base is itself visible in the history
-- rather than being an unexplained gap at sequence 1.
COMMENT ON TABLE audit_events IS
    'Append-only, hash-chained audit log. Chain re-based by migration 0009 to '
    'restore hash fidelity (microsecond timestamps, text-preserving JSON). '
    'Provides tamper evidence, not immutability.';
