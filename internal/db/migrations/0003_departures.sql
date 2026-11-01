-- Rollout step 4: departures. There's no departures table. Each sweep works out
-- what an item needs from its owners and offers, so a retry or a second departure
-- needs nothing special. The queue columns hold only what the org can't tell a
-- request: that the sweep found an item or list nobody can act on.

ALTER TABLE collab.offers DROP CONSTRAINT offers_state_check;
ALTER TABLE collab.offers ADD CONSTRAINT offers_state_check
    CHECK (state IN ('pending', 'assigned', 'accepted', 'declined', 'withdrawn', 'voided', 'expired'));
-- Set only on an offer holding a departed owner's item. Ordinary offers don't expire.
ALTER TABLE collab.offers ADD COLUMN expires_at timestamptz;
CREATE INDEX offers_expiring ON collab.offers (tenant_id, expires_at) WHERE state = 'pending' AND expires_at IS NOT NULL;

ALTER TABLE collab.work_items
    ADD COLUMN queued_at timestamptz,
    ADD COLUMN queue_reason text CHECK (queue_reason IN ('no_successor', 'owner_suspended')),
    ADD CONSTRAINT work_items_queue CHECK ((queued_at IS NULL) = (queue_reason IS NULL));
CREATE INDEX work_items_queued ON collab.work_items (tenant_id, queued_at, id) WHERE queued_at IS NOT NULL;

ALTER TABLE collab.lists
    ADD COLUMN queued_at timestamptz,
    ADD COLUMN queue_reason text CHECK (queue_reason IN ('no_active_owner', 'no_active_members')),
    ADD CONSTRAINT lists_queue CHECK ((queued_at IS NULL) = (queue_reason IS NULL));
CREATE INDEX lists_queued ON collab.lists (tenant_id, queued_at, id) WHERE queued_at IS NOT NULL;
