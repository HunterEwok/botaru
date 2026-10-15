-- SPDX-License-Identifier: AGPL-3.0-only
-- The retention loop moves expired files to purged (RET-2) and finds them by this.
CREATE INDEX files_expired ON files (file_ref) WHERE state = 'expired';
