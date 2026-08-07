-- Migration 004: packages gains the .SRCINFO metadata rendered on the
-- package page: the upstream url (scalar) plus licenses, conflicts and
-- provides, each stored as a JSON string array with the same convention
-- as maintainers.
--
-- All four columns are additive with defaults, so a plain ALTER TABLE
-- migrates existing rows in place; no table rebuild is required.

ALTER TABLE packages ADD COLUMN url TEXT NOT NULL DEFAULT '';
ALTER TABLE packages ADD COLUMN licenses TEXT NOT NULL DEFAULT '[]';
ALTER TABLE packages ADD COLUMN conflicts TEXT NOT NULL DEFAULT '[]';
ALTER TABLE packages ADD COLUMN provides TEXT NOT NULL DEFAULT '[]';
