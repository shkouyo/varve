-- Migration 006: packages gains the commit trigger record and the
-- build-verified package metadata.
--
-- last_commit stores the branch tip commit of the last successful build;
-- detection triggers on any branch commit change instead of .SRCINFO hash
-- changes. last_srcinfo_hash is kept as the historical hash record and
-- continues to be written after builds.
--
-- pkgname and source are JSON string arrays using the same convention as
-- maintainers; pkgver and pkgrel split the merged current_version
-- ("1.0-1") into its parts, which is stored unchanged.
--
-- All columns are additive with defaults, so a plain ALTER TABLE migrates
-- existing rows in place; no table rebuild is required.

ALTER TABLE packages ADD COLUMN last_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE packages ADD COLUMN pkgname TEXT NOT NULL DEFAULT '[]';
ALTER TABLE packages ADD COLUMN source TEXT NOT NULL DEFAULT '[]';
ALTER TABLE packages ADD COLUMN pkgver TEXT NOT NULL DEFAULT '';
ALTER TABLE packages ADD COLUMN pkgrel TEXT NOT NULL DEFAULT '';
