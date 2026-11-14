-- A send an assistant suggests. An assistant acting for a person drafts and
-- never sends, so it records who the item should go to, and the person sends it
-- with one click. Any send of the item, or the person dismissing it, clears it.

ALTER TABLE collab.work_items
    ADD COLUMN suggested_to text,
    ADD COLUMN suggested_note text NOT NULL DEFAULT '',
    ADD COLUMN suggested_at timestamptz,
    ADD CONSTRAINT work_items_suggestion CHECK ((suggested_to IS NULL) = (suggested_at IS NULL));
