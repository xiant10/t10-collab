-- An undo is marked started (undone_at) before its compensating change, and
-- reverted (reverted_at) after it, so one that was interrupted can run again.
ALTER TABLE {{schema}}.ontology_undo ADD COLUMN reverted_at timestamptz;
UPDATE {{schema}}.ontology_undo SET reverted_at = undone_at WHERE undone_at IS NOT NULL;
