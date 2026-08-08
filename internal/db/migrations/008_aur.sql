-- Migration 008: AUR publishing records.
--
-- aur_name holds the AUR package name of the branch dotfile's [aur]
-- section (empty = the branch is not published to AUR); aur_submit mirrors
-- its submit flag and is what the ingest path checks before pushing. The
-- last_aur_* columns record the most recent push attempt: the time, the
-- commit attempted and the error text (empty on success). last_aur_push_at
-- stays NULL until the first attempt.
--
-- The maintainers column keeps its JSON representation but now stores an
-- object list [{"name": .., "email": ..}] instead of the legacy string
-- list. Existing rows are left untouched: the store decodes both shapes
-- (legacy strings map to email-only maintainers) and the next package
-- upsert rewrites the column in the new shape, so no data rewrite is
-- needed here.
--
-- All columns are additive with defaults, so a plain ALTER TABLE migrates
-- existing rows in place; no table rebuild is required.

ALTER TABLE packages ADD COLUMN aur_name TEXT NOT NULL DEFAULT '';
ALTER TABLE packages ADD COLUMN aur_submit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE packages ADD COLUMN last_aur_push_at TEXT;
ALTER TABLE packages ADD COLUMN last_aur_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE packages ADD COLUMN last_aur_error TEXT NOT NULL DEFAULT '';
