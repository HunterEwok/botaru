-- SPDX-License-Identifier: AGPL-3.0-only
-- Group chat bindings (DLV-7, DLV-15, PRM-15).

-- A group chat the operator bound (P2 chat.bind). members_unknown stops deliveries to the chat
-- after a person outside the tenant appeared there, until the operator binds it again (DLV-7).
CREATE TABLE chat_bindings (
    tenant          text NOT NULL,
    name            text NOT NULL,
    channel         text NOT NULL,
    chat            text NOT NULL,           -- the chat target id, group:<external id>
    ceiling         text NOT NULL,
    locale          text,
    model_text      text NOT NULL DEFAULT 'summary_only',
    members_unknown boolean NOT NULL DEFAULT false,
    created_by      text NOT NULL,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant, name),
    UNIQUE (tenant, channel, chat)
);

-- The group chat that gets a code text when the direct message of a split answer dies
-- (DLV-15): the person is asked to open a private chat with the bot.
ALTER TABLE deliveries ADD COLUMN fallback_chat text;
