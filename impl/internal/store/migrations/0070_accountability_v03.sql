-- SPDX-License-Identifier: AGPL-3.0-only
-- Completion of accountability from v0.3 (GOV-40): the candidates of links to rejected proposals
-- of one incident share one rejection_link_card, carried by the first of them.

ALTER TABLE attributions ADD COLUMN card_of text;
CREATE INDEX attributions_card_of ON attributions (card_of) WHERE card_of IS NOT NULL;

-- Links found before this migration keep a card of their own.
UPDATE attributions SET card_of = id WHERE kind = 'rejected_proposal';
