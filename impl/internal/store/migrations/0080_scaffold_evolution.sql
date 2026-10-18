-- SPDX-License-Identifier: AGPL-3.0-only
-- The variant archive of the scaffold (EVO-4): one immutable row per target object of every
-- manifest change a bot proposed, with its lineage, the fields it sets and its outcome. The
-- identity, the fields and the hashes never change; the outcome, the gate result, the live flag
-- and the counters of the children do.
CREATE TABLE scaffold_variants (
    variant_id                text PRIMARY KEY,
    tenant                    text NOT NULL,
    target_kind               text NOT NULL,
    target_name               text NOT NULL,
    parent_variant_id         text REFERENCES scaffold_variants (variant_id),
    base_commit               text,
    base_sha256               text,
    proposal_id               text NOT NULL,
    author_bot_id             text,
    author_role               text NOT NULL,
    manifest_sha256           text NOT NULL,
    patch_json                jsonb NOT NULL,
    revert_patch_json         jsonb NOT NULL,
    diversity_signature       text NOT NULL,
    gate_metrics              jsonb,
    trial_outcome             text NOT NULL DEFAULT 'pending',
    reason_code               text,
    live                      boolean NOT NULL DEFAULT false,
    children_count            integer NOT NULL DEFAULT 0,
    successful_children_count integer NOT NULL DEFAULT 0,
    created_at                timestamptz NOT NULL,
    updated_at                timestamptz NOT NULL
);
CREATE INDEX scaffold_variants_target ON scaffold_variants (tenant, target_kind, target_name, created_at);
CREATE INDEX scaffold_variants_role ON scaffold_variants (tenant, author_role, created_at);
CREATE INDEX scaffold_variants_proposal ON scaffold_variants (proposal_id);

-- The review rules (EVO-3): a lesson of a refused, reverted or worse manifest change of a role,
-- kept until a person drops it. A later proposal of the role for the same problem and target
-- whose approach contains the rule's signature is refused at creation.
CREATE TABLE review_rules (
    id            bigserial PRIMARY KEY,
    tenant        text NOT NULL,
    role          text NOT NULL,
    category      text NOT NULL,
    target_kind   text NOT NULL,
    target_name   text NOT NULL,
    problem_code  text NOT NULL,
    signature     text NOT NULL,
    outcome       text NOT NULL,
    reason_code   text NOT NULL,
    count         integer NOT NULL DEFAULT 1,
    proposal_ids  jsonb NOT NULL,
    first_at      timestamptz NOT NULL,
    last_at       timestamptz NOT NULL,
    dropped_at    timestamptz,
    dropped_by    text
);
CREATE UNIQUE INDEX review_rules_live ON review_rules (tenant, role, target_kind, target_name, problem_code, signature)
    WHERE dropped_at IS NULL;
