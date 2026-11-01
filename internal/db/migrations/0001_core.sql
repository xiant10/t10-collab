-- Rollout step 2: items, owners, collaborators, offers, comments and history.
-- Users live in the auth database, so user IDs are plain text with no foreign
-- key. Composite keys make a link across tenants impossible, whatever the code
-- above them does.

CREATE TABLE collab.work_items (
    tenant_id   text NOT NULL,
    id          text NOT NULL,
    title       text NOT NULL CHECK (title <> '' AND char_length(title) <= 300),
    description text NOT NULL DEFAULT '' CHECK (char_length(description) <= 20000),
    status      text NOT NULL DEFAULT 'todo' CHECK (status IN ('todo', 'doing', 'done', 'cancelled')),
    priority    text NOT NULL DEFAULT 'normal' CHECK (priority IN ('low', 'normal', 'high')),
    due_at      timestamptz,
    creator_id  text NOT NULL,
    version     integer NOT NULL DEFAULT 1,
    client_ref  text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz,
    PRIMARY KEY (tenant_id, id)
);
CREATE UNIQUE INDEX work_items_client_ref ON collab.work_items (tenant_id, creator_id, client_ref)
    WHERE client_ref IS NOT NULL;
CREATE INDEX work_items_page ON collab.work_items (tenant_id, created_at DESC, id DESC);

CREATE TABLE collab.work_item_owners (
    tenant_id  text NOT NULL,
    item_id    text NOT NULL,
    user_id    text NOT NULL,
    added_by   text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, item_id, user_id),
    FOREIGN KEY (tenant_id, item_id) REFERENCES collab.work_items (tenant_id, id)
);
CREATE INDEX work_item_owners_user ON collab.work_item_owners (tenant_id, user_id);

CREATE TABLE collab.work_item_collaborators (
    tenant_id  text NOT NULL,
    item_id    text NOT NULL,
    user_id    text NOT NULL,
    added_by   text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, item_id, user_id),
    FOREIGN KEY (tenant_id, item_id) REFERENCES collab.work_items (tenant_id, id)
);
CREATE INDEX work_item_collaborators_user ON collab.work_item_collaborators (tenant_id, user_id);

-- An assignment is a row that starts as 'assigned', so one history covers every send.
CREATE TABLE collab.offers (
    tenant_id     text NOT NULL,
    id            text NOT NULL,
    item_id       text NOT NULL,
    from_user_id  text NOT NULL,
    to_user_id    text NOT NULL,
    mode          text NOT NULL CHECK (mode IN ('transfer', 'share')),
    note          text NOT NULL DEFAULT '' CHECK (char_length(note) <= 2000),
    state         text NOT NULL CHECK (state IN ('pending', 'assigned', 'accepted', 'declined', 'withdrawn', 'voided')),
    response_note text NOT NULL DEFAULT '' CHECK (char_length(response_note) <= 2000),
    responded_at  timestamptz,
    client_ref    text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, item_id) REFERENCES collab.work_items (tenant_id, id)
);
CREATE UNIQUE INDEX offers_one_per_recipient ON collab.offers (tenant_id, item_id, to_user_id) WHERE state = 'pending';
CREATE UNIQUE INDEX offers_one_transfer_per_sender ON collab.offers (tenant_id, item_id, from_user_id)
    WHERE state = 'pending' AND mode = 'transfer';
CREATE UNIQUE INDEX offers_client_ref ON collab.offers (tenant_id, from_user_id, client_ref) WHERE client_ref IS NOT NULL;
CREATE INDEX offers_inbox ON collab.offers (tenant_id, to_user_id, created_at DESC, id DESC) WHERE state = 'pending';
CREATE INDEX offers_sent ON collab.offers (tenant_id, from_user_id, created_at DESC, id DESC);

CREATE TABLE collab.comments (
    tenant_id  text NOT NULL,
    id         text NOT NULL,
    item_id    text NOT NULL,
    author_id  text NOT NULL,
    body       text NOT NULL CHECK (body <> '' AND char_length(body) <= 10000),
    created_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, item_id) REFERENCES collab.work_items (tenant_id, id)
);
CREATE INDEX comments_item ON collab.comments (tenant_id, item_id, created_at, id);

-- Item history and audit trail. item_id is null for a settings or unit link
-- change. seq orders events, because IDs are random.
CREATE TABLE collab.events (
    tenant_id      text NOT NULL,
    id             text NOT NULL,
    seq            bigint GENERATED ALWAYS AS IDENTITY,
    item_id        text,
    actor_user_id  text NOT NULL,
    actor_grant_id text,
    kind           text NOT NULL,
    detail         jsonb NOT NULL DEFAULT '{}',
    at             timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, item_id) REFERENCES collab.work_items (tenant_id, id)
);
CREATE INDEX events_item ON collab.events (tenant_id, item_id, seq);

CREATE FUNCTION collab.events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'collab.events is append-only';
END $$;
CREATE TRIGGER events_append_only BEFORE UPDATE OR DELETE ON collab.events
    FOR EACH STATEMENT EXECUTE FUNCTION collab.events_append_only();

-- The polling cursor is (xid, seq). seq alone can't be one: a transaction can
-- commit after a later one, and a client past it would miss its rows. Readers
-- stop at the oldest running transaction, so nothing can land behind a cursor.
CREATE TABLE collab.notifications (
    tenant_id text NOT NULL,
    id        text NOT NULL,
    seq       bigint GENERATED ALWAYS AS IDENTITY,
    xid       xid8 NOT NULL DEFAULT pg_current_xact_id(),
    user_id   text NOT NULL,
    event_id  text NOT NULL,
    read_at   timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, event_id) REFERENCES collab.events (tenant_id, id)
);
CREATE INDEX notifications_user ON collab.notifications (tenant_id, user_id, xid, seq);
