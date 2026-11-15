-- A person who is a plain member of a list agrees to hold stages of its
-- workflow before moves give them its items, as only the reporting line may
-- give someone work without asking. holds_at is when they agreed; owners and
-- agent accounts need none. See docs/workflows.md.

ALTER TABLE collab.list_members
    ADD COLUMN holds_at timestamptz;
