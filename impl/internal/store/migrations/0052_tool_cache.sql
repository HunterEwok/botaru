-- SPDX-License-Identifier: AGPL-3.0-only
-- The tool output cache (PRM-12): one entry per tenant and key (tenant, plugin, tool,
-- arguments, catalog version), with the flags of the call that filled it (AUD-5).
CREATE TABLE tool_cache (
    tenant             text NOT NULL,
    key                text NOT NULL,
    plugin             text NOT NULL,
    tool               text NOT NULL,
    output             jsonb NOT NULL,
    personal_seen      jsonb NOT NULL DEFAULT '[]',
    personal_subjects  jsonb NOT NULL DEFAULT '[]',
    source_call        text NOT NULL,
    created_at         timestamptz NOT NULL,
    expires_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant, key)
);
CREATE INDEX tool_cache_plugin ON tool_cache (tenant, plugin);

-- A call answered from the cache and its usage line carry the flag cached.
ALTER TABLE toolcalls ADD COLUMN cached boolean NOT NULL DEFAULT false;
ALTER TABLE spend ADD COLUMN cached boolean NOT NULL DEFAULT false;
