-- Rollout steps 3 and 4: shared lists, team links and tenant settings.
-- org_unit_id is plain text, because units live in the auth database.

CREATE TABLE collab.lists (
    tenant_id   text NOT NULL,
    id          text NOT NULL,
    name        text NOT NULL CHECK (name <> '' AND char_length(name) <= 200),
    org_unit_id text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    archived_at timestamptz,
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX lists_linked ON collab.lists (tenant_id, id) WHERE org_unit_id IS NOT NULL;

CREATE TABLE collab.list_members (
    tenant_id  text NOT NULL,
    list_id    text NOT NULL,
    user_id    text NOT NULL,
    role       text NOT NULL CHECK (role IN ('owner', 'member')),
    source     text NOT NULL DEFAULT 'manual' CHECK (source IN ('manual', 'org')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, list_id, user_id),
    FOREIGN KEY (tenant_id, list_id) REFERENCES collab.lists (tenant_id, id),
    -- The sweep only touches org members, so an owner must never be one.
    CHECK (role = 'member' OR source = 'manual')
);
CREATE INDEX list_members_user ON collab.list_members (tenant_id, user_id);

ALTER TABLE collab.work_items
    ADD COLUMN list_id text,
    ADD CONSTRAINT work_items_list FOREIGN KEY (tenant_id, list_id) REFERENCES collab.lists (tenant_id, id);
CREATE INDEX work_items_list_id ON collab.work_items (tenant_id, list_id);

-- No row means 'none'.
CREATE TABLE collab.tenant_settings (
    tenant_id          text PRIMARY KEY,
    manager_visibility text NOT NULL CHECK (manager_visibility IN ('none', 'direct_reports', 'all_reports')),
    updated_by         text NOT NULL,
    updated_at         timestamptz NOT NULL DEFAULT now()
);
