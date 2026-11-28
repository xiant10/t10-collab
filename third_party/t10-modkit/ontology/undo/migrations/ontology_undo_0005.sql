-- What agents applied for a person through their _act tool, undoable or not,
-- so the person can see what was done for them (GET /v1/done). undo_id names
-- the undo entry of an undo action. Rows older than 30 days are swept.
CREATE TABLE {{schema}}.ontology_done (
	tenant_id  text NOT NULL,
	id         text NOT NULL,
	user_id    text NOT NULL,
	action     text NOT NULL,
	object     text NOT NULL DEFAULT '',
	agent      text NOT NULL DEFAULT '',
	undo_id    text,
	created_at timestamptz NOT NULL,
	PRIMARY KEY (tenant_id, id)
);
CREATE INDEX ontology_done_person ON {{schema}}.ontology_done (tenant_id, user_id, created_at DESC);
