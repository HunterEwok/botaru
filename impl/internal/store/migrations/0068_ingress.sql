-- SPDX-License-Identifier: AGPL-3.0-only
-- Deliveries of source channels (ING-3): one row per key system:ingress:<source>:<delivery id>,
-- so a repeated delivery gives one event; the row keeps the event it gave, the first task its
-- routes enqueued, the body file, and how long the key is kept against repeats.
CREATE TABLE ingress_deliveries (
    tenant      text NOT NULL,
    key         text NOT NULL,
    channel     text NOT NULL,
    event       text NOT NULL,
    first_task  text,
    file_ref    text,
    created_at  timestamptz NOT NULL,
    keep_until  timestamptz NOT NULL,
    PRIMARY KEY (tenant, key)
);
CREATE INDEX ingress_deliveries_keep ON ingress_deliveries (keep_until);
CREATE INDEX ingress_deliveries_file ON ingress_deliveries (tenant, file_ref);
