-- Applied actions their people can undo, and idempotent actions' answers, through
-- t10-modkit/ontology. {{schema}} is the module's own schema.
CREATE TABLE {{schema}}.ontology_undo (
	tenant_id  text NOT NULL,
	id         text NOT NULL,
	user_id    text NOT NULL,
	action     text NOT NULL,
	object     text NOT NULL DEFAULT '',
	payload    jsonb NOT NULL DEFAULT 'null',
	created_at timestamptz NOT NULL,
	expires_at timestamptz NOT NULL,
	undone_at  timestamptz,
	PRIMARY KEY (tenant_id, id)
);

CREATE TABLE {{schema}}.ontology_replays (
	tenant_id  text NOT NULL,
	user_id    text NOT NULL,
	action     text NOT NULL,
	key        text NOT NULL,
	hash       text NOT NULL,
	answer     jsonb NOT NULL,
	created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
	PRIMARY KEY (tenant_id, user_id, action, key)
);
