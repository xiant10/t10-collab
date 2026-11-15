-- A workflow may start each new item on its list at the first stage. Stages
-- gain holders (several, the item going to one of them) and skip_when in their
-- JSON, so they need no column. See docs/workflows.md.

ALTER TABLE collab.list_workflows
    ADD COLUMN auto_start boolean NOT NULL DEFAULT false;
