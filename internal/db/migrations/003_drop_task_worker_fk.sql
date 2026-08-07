-- Migration 003: drop the tasks.worker_id foreign key.
--
-- Deleting a worker must never be blocked by task history: builds already
-- carry the executing node as plain text (worker_name, migration 002) and
-- tasks keep worker_id only as a nullable provenance hint without a
-- constraint, matching the builds contract. Deregistering a worker that
-- executed builds is a plain delete of the workers row.
--
-- SQLite cannot drop a constraint in place, so the tasks table is rebuilt
-- through the RENAME dance with every column and index preserved.

ALTER TABLE tasks RENAME TO tasks_legacy;

DROP INDEX idx_tasks_state;
DROP INDEX idx_tasks_active;

CREATE TABLE tasks (
    id               TEXT PRIMARY KEY,
    package_id       INTEGER NOT NULL REFERENCES packages(id),
    build_id         TEXT NOT NULL REFERENCES builds(id),
    state            TEXT NOT NULL,
    worker_id        INTEGER,
    assigned_at      TEXT,
    created_at       TEXT NOT NULL,
    last_progress_at TEXT NOT NULL,
    attempts         INTEGER NOT NULL DEFAULT 0,
    claim_token      TEXT NOT NULL DEFAULT '',
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    fail_count       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_tasks_state ON tasks(state, created_at);
CREATE UNIQUE INDEX idx_tasks_active ON tasks(package_id)
    WHERE state IN ('queued', 'assigned', 'running');

INSERT INTO tasks
    (id, package_id, build_id, state, worker_id, assigned_at, created_at,
     last_progress_at, attempts, claim_token, cancel_requested, fail_count)
    SELECT id, package_id, build_id, state, worker_id, assigned_at, created_at,
           last_progress_at, attempts, claim_token, cancel_requested, fail_count
    FROM tasks_legacy;

DROP TABLE tasks_legacy;
