-- Links on a work item: where its work happens elsewhere, such as the pull
-- request an external agent teammate opened. They're data: nothing fetches
-- them. See t10-agent-control's docs/external-teammates.md.

CREATE TABLE collab.work_item_links (
    tenant_id  text NOT NULL,
    id         text NOT NULL,
    item_id    text NOT NULL,
    url        text NOT NULL CHECK (url LIKE 'https://%' AND char_length(url) <= 2000),
    title      text NOT NULL CHECK (title <> '' AND char_length(title) <= 200),
    kind       text NOT NULL CHECK (kind IN ('pull_request', 'commit', 'build', 'document', 'other')),
    added_by   text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, item_id) REFERENCES collab.work_items (tenant_id, id)
);
CREATE INDEX work_item_links_item ON collab.work_item_links (tenant_id, item_id, created_at);
