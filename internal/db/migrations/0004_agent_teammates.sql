-- Agent teammates: tenant agent accounts as members. See t10-agent-control's
-- docs/tenant-agents.md, Part 2. An agent account's ID starts with acc_, as
-- the auth service issues them.

-- Who may offer work to an agent account. Its reporting line can always assign
-- it work; anyone else needs to be listed here. No row means nobody.
CREATE TABLE collab.agent_intake (
    tenant_id  text NOT NULL,
    user_id    text NOT NULL CHECK (user_id LIKE 'acc\_%'),
    everyone   boolean NOT NULL DEFAULT false,
    units      text[] NOT NULL DEFAULT '{}',
    lists      text[] NOT NULL DEFAULT '{}',
    updated_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id)
);

-- Events for t10-automations, one per agent account an item's change concerns,
-- written in the change's transaction and posted at least once. cause_grant_id
-- is the grant of the run that caused it, when an agent did, and hops counts
-- the agent sends on the item since a person last acted on it, or the agent
-- comments if there are more.
CREATE TABLE collab.outbox (
    tenant_id      text NOT NULL,
    id             text NOT NULL,
    seq            bigint GENERATED ALWAYS AS IDENTITY,
    event_id       text NOT NULL,
    kind           text NOT NULL,
    item_id        text,
    for_user_id    text NOT NULL,
    actor_user_id  text NOT NULL,
    cause_grant_id text,
    hops           integer NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    attempts       integer NOT NULL DEFAULT 0,
    next_at        timestamptz NOT NULL DEFAULT now(),
    sent_at        timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, event_id) REFERENCES collab.events (tenant_id, id)
);
CREATE INDEX outbox_due ON collab.outbox (next_at, seq) WHERE sent_at IS NULL;
