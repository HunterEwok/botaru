-- SPDX-License-Identifier: AGPL-3.0-only
-- The worker whose runtime purges the runtime_internal sessions and memory of a retired bot
-- identity (MEM-9). The recorded scope of such a request stays (tenant, bot) of
-- deletion-request; this column only says which runtime gets the one purge step, so a
-- successor bound to the same worker is not in the scope.
ALTER TABLE erasure_requests ADD COLUMN runtime_worker text;
