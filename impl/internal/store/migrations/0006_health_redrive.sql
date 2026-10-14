-- SPDX-License-Identifier: AGPL-3.0-only
-- Each background loop marks a tick in the store, so the tick age of each loop is visible to
-- every core copy and to P2 health (OBS-4, OBS-9); redriven deliveries link to the dead ones.

CREATE TABLE loop_ticks (
    loop       text PRIMARY KEY,
    ticked_at  timestamptz NOT NULL
);

-- The share of deliveries delivered within 10 minutes counts by creation time (OBS-9).
CREATE INDEX deliveries_created ON deliveries (created_at);
CREATE INDEX tasks_urgent ON tasks (created_at) WHERE state = 'submitted' AND priority_class <= 2;

-- A redriven delivery links to the dead one; the key system:redrive:<id>:<n> is unique (DLV-4).
ALTER TABLE deliveries ADD COLUMN redrive_of text;
ALTER TABLE deliveries ADD COLUMN redrive_key text UNIQUE;

-- The plugins a toolhost reported down in hello or heartbeat (OBS-10): no hand-out for them.
ALTER TABLE workers ADD COLUMN plugins_down jsonb NOT NULL DEFAULT '[]';
