-- An idempotent action claims its key before it runs, with no answer until it
-- has one, so two requests with one key never both apply it.
ALTER TABLE {{schema}}.ontology_replays ALTER COLUMN answer DROP NOT NULL;
