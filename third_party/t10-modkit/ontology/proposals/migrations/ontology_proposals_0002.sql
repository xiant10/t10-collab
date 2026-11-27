-- A draft keeps the revision of the object it was made on, so a confirm after the
-- object changed is refused with stale_revision. 0 for a create, and for drafts
-- made before this column.
ALTER TABLE {{schema}}.ontology_proposals ADD COLUMN revision bigint NOT NULL DEFAULT 0;
