-- The E5 export of the monitor profile (MON-3, MON-11, MON-12).
-- origin: code, or monitor for an alarm raised by a monitoring problem (MON-5).
ALTER TABLE alarm_episodes ADD COLUMN origin text NOT NULL DEFAULT 'code';

-- One row per tenant with E5 deliveries: the last successful export, the start of the current
-- failure, the last monitor_export_stalled message and the last planned audit head.
CREATE TABLE monitor_exports (
    tenant text PRIMARY KEY,
    last_ok_at timestamptz,
    failing_since timestamptz,
    stalled_notice_at timestamptz,
    head_planned_at timestamptz,
    head_exported_at timestamptz
);

CREATE INDEX deliveries_monitor_open ON deliveries (tenant, created_at) WHERE exec_kind = 'monitor' AND state IN ('planned', 'sending');

-- The public key of each audit-head key id, written when it first signs and never replaced, so
-- heads stay verifiable after the token key rotates away (MON-3).
CREATE TABLE audit_head_keys (
    key_id text PRIMARY KEY,
    public_key bytea NOT NULL,
    created_at timestamptz NOT NULL
);

CREATE FUNCTION audit_head_keys_frozen() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_head_keys is insert-only';
END $$;
CREATE TRIGGER audit_head_keys_no_change BEFORE UPDATE OR DELETE ON audit_head_keys
    FOR EACH ROW EXECUTE FUNCTION audit_head_keys_frozen();
