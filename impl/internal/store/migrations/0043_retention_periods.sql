-- SPDX-License-Identifier: AGPL-3.0-only
-- Deletion by period in every copy (RET-9, RET-10, D-108), the 93-day anonymization of a
-- person's usage lines (BUD-16, SEC-14) and the whole-month deletion of audit (AUD-3).

-- The metadata period a tenant set and since when: a shorter period applies only to records
-- written after the apply that set it (D-108 (1)). No row: the class default.
CREATE TABLE retention_metadata_periods (
    tenant  text NOT NULL,
    seconds bigint NOT NULL CHECK (seconds > 0),
    since   timestamptz NOT NULL,
    PRIMARY KEY (tenant, since)
);

-- A usage line of a person past 93 days keeps only the kind of its subject.
ALTER TABLE spend ADD COLUMN subject_kind text CHECK (subject_kind IN ('human'));
CREATE INDEX spend_person_age ON spend (time) WHERE subject LIKE 'human:%';

-- The retention loop deletes audit only as a prefix of a chain in whole months: it first moves
-- the cut of the chain (the seq and hash of the last record deleted), and the trigger lets a
-- delete through only for records at or below the cut. The cut never moves back, so audit.verify
-- starts from it and every record after it still chains (AUD-3, AUD-6). Updates stay refused.
CREATE TABLE audit_cuts (
    tenant      text NOT NULL,
    epoch       bigint NOT NULL,
    seq         bigint NOT NULL,
    hash        text NOT NULL,
    updated_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant, epoch)
);
CREATE OR REPLACE FUNCTION audit_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND OLD.seq <= (SELECT seq FROM audit_cuts c WHERE c.tenant = OLD.tenant AND c.epoch = OLD.epoch) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit is append-only';
END
$$;
-- A cut names a record that exists with that hash and is older than the shortest metadata
-- period a tenant may set (93 days, RET-9), so no cut, made by the loop or written by hand, lets
-- a delete through for recent audit. A cut only moves forward and is never removed.
CREATE FUNCTION audit_cut_forward() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'an audit cut is never removed';
    END IF;
    IF TG_OP = 'UPDATE' AND (NEW.seq < OLD.seq OR NEW.tenant <> OLD.tenant OR NEW.epoch <> OLD.epoch) THEN
        RAISE EXCEPTION 'an audit cut only moves forward';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM audit a WHERE a.tenant = NEW.tenant AND a.epoch = NEW.epoch AND a.seq = NEW.seq AND a.hash = NEW.hash
        AND a.at < botaru_now() - interval '93 days') THEN
        RAISE EXCEPTION 'an audit cut names a stored record older than 93 days';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER audit_cuts_forward BEFORE INSERT OR UPDATE OR DELETE ON audit_cuts
    FOR EACH ROW EXECUTE FUNCTION audit_cut_forward();

-- The memory class with a tenant period reaches facts by age.
CREATE INDEX facts_age ON facts (created_at);
CREATE INDEX memory_items_age ON memory_items (updated_at);
CREATE INDEX events_tenant_time ON events (tenant, time);
