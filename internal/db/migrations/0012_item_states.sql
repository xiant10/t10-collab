-- Item types, step 2: a type with states gives its items a lifecycle. status
-- stays as the state's phase, so every query and index on it keeps working.
-- Null for an item whose type has no states, such as collab.task.
ALTER TABLE collab.work_items ADD COLUMN state text CHECK (char_length(state) <= 63);
