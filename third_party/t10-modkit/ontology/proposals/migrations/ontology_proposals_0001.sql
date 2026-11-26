-- Drafts agents made for people, through t10-modkit/ontology. {{schema}} is the module's own schema.
CREATE TABLE {{schema}}.ontology_proposals (
	tenant_id  text NOT NULL,
	id         text NOT NULL,
	user_id    text NOT NULL,
	agent      text NOT NULL DEFAULT '',
	action     text NOT NULL,
	object     text NOT NULL DEFAULT '',
	params     jsonb NOT NULL DEFAULT '{}',
	state      text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'confirmed', 'dismissed')),
	created_at timestamptz NOT NULL,
	decided_at timestamptz,
	expires_at timestamptz NOT NULL,
	PRIMARY KEY (tenant_id, id)
);

CREATE INDEX ontology_proposals_open ON {{schema}}.ontology_proposals (tenant_id, user_id, created_at DESC) WHERE state = 'open';
