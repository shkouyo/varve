-- Migration 005: drop the packages.enabled dead column.
--
-- The flag was never toggled by any code path: package rows are only
-- written by UpsertPackage and the index rebuild, both of which hardcode
-- the value to 1, and no reader used it as a decision input. Keeping it
-- around only widened the row and the package render. SQLite drops the
-- column in place, so the migration is a single ALTER statement.
ALTER TABLE packages DROP COLUMN enabled;
