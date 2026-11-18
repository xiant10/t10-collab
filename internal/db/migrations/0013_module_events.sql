-- Item types, step 3: the owning module's event feed. It pages by (xid, seq),
-- as notifications do, so a transaction that commits after a later one is
-- never behind a module's cursor. module is set as each event is written, from
-- its item's type, so the feed is one indexed read. Events written before this
-- have the migration's xid and no module.
ALTER TABLE collab.events
    ADD COLUMN xid    xid8 NOT NULL DEFAULT pg_current_xact_id(),
    ADD COLUMN module text;
CREATE INDEX events_module_feed ON collab.events (module, xid, seq) WHERE module IS NOT NULL;
