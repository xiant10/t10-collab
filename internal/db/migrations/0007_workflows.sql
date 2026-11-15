-- A shared list's workflow: its items' route, such as Build, Review, Test,
-- Approve. See docs/workflows.md. stages is a JSON array of {id, name, holder,
-- on_changes, guide}, checked by the service; holders and the lead are user IDs
-- with no foreign key, as everywhere here.

CREATE TABLE collab.list_workflows (
    tenant_id  text NOT NULL,
    list_id    text NOT NULL,
    version    integer NOT NULL DEFAULT 1,
    stages     jsonb NOT NULL,
    rounds     integer NOT NULL CHECK (rounds BETWEEN 1 AND 20),
    lead_id    text NOT NULL,
    updated_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, list_id),
    FOREIGN KEY (tenant_id, list_id) REFERENCES collab.lists (tenant_id, id)
);

-- stage is null off the workflow. rounds counts the times the item went back
-- since it was started or put at a stage by a person.
ALTER TABLE collab.work_items
    ADD COLUMN stage text,
    ADD COLUMN rounds integer NOT NULL DEFAULT 0;
