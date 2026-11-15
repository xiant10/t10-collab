-- The grant chain of the run that caused an event, root first, so t10-automations
-- can find which of its firings ran it: the firing's grant is in the chain, though
-- the harness's own child grant is what reached here.
ALTER TABLE collab.outbox ADD COLUMN cause_chain text[] NOT NULL DEFAULT '{}';
