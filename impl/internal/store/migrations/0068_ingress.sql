-- SPDX-License-Identifier: AGPL-3.0-only
-- Deliveries of source channels (ING-3): one row per key system:ingress:<source>:<delivery id>,
-- so a repeated delivery gives one event; the row keeps the event it gave and the body file.
CREATE TABLE ingress_deliveries (
    tenant      text NOT NULL,
    key         text NOT NULL,
    channel     text NOT NULL,
    event       text NOT NULL,
    file_ref    text,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant, key)
);
CREATE INDEX ingress_deliveries_time ON ingress_deliveries (tenant, created_at);
