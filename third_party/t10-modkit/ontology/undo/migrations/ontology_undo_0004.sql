-- The agent that applied an action through its _act tool, so the person's list
-- of what they can still undo says who did it; empty when the person did. The
-- list reads a person's open entries newest first.
ALTER TABLE {{schema}}.ontology_undo ADD COLUMN agent text NOT NULL DEFAULT '';
CREATE INDEX ontology_undo_open ON {{schema}}.ontology_undo (tenant_id, user_id, created_at DESC) WHERE undone_at IS NULL;
