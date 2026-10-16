-- SPDX-License-Identifier: AGPL-3.0-only
-- The bound group chats where a person was seen writing, and the chats an offboarded person
-- may still be a member of (IDN-13, IDN-14, REP-7). The core does not see group membership,
-- so a person seen in a bound chat counts as a member until the operator binds the chat again.
CREATE TABLE chat_seen (
    tenant      text NOT NULL,
    binding     text NOT NULL,
    principal   text NOT NULL,
    last_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant, binding, principal)
);
CREATE INDEX chat_seen_principal ON chat_seen (tenant, principal);

CREATE TABLE chat_leftovers (
    tenant          text NOT NULL,
    binding         text NOT NULL,
    principal       text NOT NULL,
    since           timestamptz NOT NULL,
    next_notice_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant, binding, principal)
);
CREATE INDEX chat_leftovers_notice ON chat_leftovers (next_notice_at);
