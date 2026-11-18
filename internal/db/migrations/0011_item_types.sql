-- Item types, step 1: an item can be of a type a module declares, with fields
-- checked against it, the record it's about, and who may read it. Untyped items
-- are the built-in collab.task and behave as before. See
-- ../proposals/collab-item-types.md in the workspace and docs/types.md.

-- A type's default is tenant ''. Tenant versions come with a later step.
CREATE TABLE collab.types (
    tenant_id  text NOT NULL DEFAULT '',
    id         text NOT NULL,
    module     text NOT NULL,
    version    integer NOT NULL CHECK (version >= 1),
    definition jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

-- visibility and departure are copied from the type, which can never change
-- them, so the read filters need no join.
ALTER TABLE collab.work_items
    ADD COLUMN type         text NOT NULL DEFAULT 'collab.task',
    ADD COLUMN type_version integer NOT NULL DEFAULT 1,
    ADD COLUMN fields       jsonb NOT NULL DEFAULT '{}' CHECK (pg_column_size(fields) <= 32768),
    ADD COLUMN subject_ref  text CHECK (char_length(subject_ref) <= 300),
    ADD COLUMN visibility   text NOT NULL DEFAULT 'team' CHECK (visibility IN ('team', 'participants')),
    ADD COLUMN departure    text NOT NULL DEFAULT 'reassign' CHECK (departure IN ('reassign', 'creator', 'cancel')),
    ADD CONSTRAINT work_items_participants_off_lists CHECK (visibility <> 'participants' OR list_id IS NULL);
CREATE INDEX work_items_subject ON collab.work_items (tenant_id, subject_ref) WHERE subject_ref IS NOT NULL;
CREATE INDEX work_items_type ON collab.work_items (tenant_id, type, status);
