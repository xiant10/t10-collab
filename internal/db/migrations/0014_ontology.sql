-- Collab serves the ontology. A send an assistant drafts is now an ontology
-- proposal (collab.ontology_proposals, from t10-modkit's ontology/proposals),
-- so the item's own suggestion goes. type_revision counts changes to item
-- types and their tenant versions: it's the ontology's tenant-layer revision.

ALTER TABLE collab.work_items
    DROP CONSTRAINT work_items_suggestion,
    DROP COLUMN suggested_to,
    DROP COLUMN suggested_note,
    DROP COLUMN suggested_at;

CREATE TABLE collab.type_revision (
    one      boolean PRIMARY KEY DEFAULT true CHECK (one),
    revision bigint NOT NULL
);
INSERT INTO collab.type_revision (revision) VALUES (1);
