-- SPDX-License-Identifier: AGPL-3.0-only
-- Schedules (TIM-2, TIM-3).

-- The last slot a schedule handled and the expression it was computed by: a change of cron or
-- time zone takes effect from the next slot after the apply (TIM-3).
CREATE TABLE schedule_state (
    tenant      text NOT NULL,
    name        text NOT NULL,
    cron        text NOT NULL,
    timezone    text NOT NULL,
    last_slot   timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant, name)
);
