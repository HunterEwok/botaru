-- SPDX-License-Identifier: AGPL-3.0-only
-- The parts of a tool cache key fixed when a call is checked (PRM-12): the catalog version and
-- the data mark of the task, so an entry stored at completion has the key of the call.
ALTER TABLE toolcalls ADD COLUMN cache_catalog text;
ALTER TABLE toolcalls ADD COLUMN cache_mark text;
