-- SPDX-License-Identifier: AGPL-3.0-only
-- File names for the text refinement of the sniffed type (ART-5), and the files of an export
-- (LCY-2, SEC-10), which only an export delivery hands out.
ALTER TABLE files ADD COLUMN filename text;
ALTER TABLE files DROP CONSTRAINT files_origin_check;
ALTER TABLE files ADD CONSTRAINT files_origin_check CHECK (origin IN ('upload', 'produced', 'ingested', 'export'));
