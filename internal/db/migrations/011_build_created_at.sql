-- Migration 011: builds.created_at.
--
-- The enqueue moment previously lived only on the mirrored tasks row
-- (tasks.created_at), and every build has exactly one task, so the web
-- UI could not show queue wait times without a join. This migration
-- adds builds.created_at as a nullable column and backfills it from the
-- mirrored task; builds without a task (not created in normal
-- operation) keep NULL.

ALTER TABLE builds ADD COLUMN created_at TEXT;

UPDATE builds SET created_at = (
    SELECT t.created_at FROM tasks t WHERE t.build_id = builds.id
) WHERE created_at IS NULL;
