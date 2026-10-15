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
