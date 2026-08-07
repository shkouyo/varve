-- Migration 002: builds.id becomes an opaque 16-hex hash primary key so a
-- build id can never be confused with a sequence number and survives
-- index rebuilds and restores. The packages/tasks tables are rebuilt in
-- the same transaction because their references to builds change type.
--
-- Every table is rebuilt through the RENAME dance (old tables keep their
-- names with a _legacy suffix until the fresh ones are populated, then are
-- dropped child-first) so foreign-key enforcement stays on throughout and
-- a partially applied migration rolls back atomically.
--
-- Legacy mapping: an old autoincrement id N becomes printf('%016x', N), a
-- deterministic zero-padded hash that keeps old rows reproducible and is
-- collision-safe against the random ids generated at runtime. The old
-- integer is preserved in builds.seq, a monotonically increasing ordering
-- column: with hash ids a lexicographic ORDER BY id is meaningless, so the
-- "newest first" listing order now comes from seq. log_path is rewritten
-- to logs/<hash>.log, which intentionally points at a new on-disk name.
--
-- worker_id is kept on builds but its REFERENCES workers constraint is
-- dropped: worker ownership for display now lives in the plain-text
-- worker_name column, so deregistering (deleting) a worker can never be
-- blocked by historical build rows.
--
-- New failure bookkeeping for the retry policy:
--   tasks.fail_count        per-task failed-attempt counter (retry budget)
--   packages.last_failed_at failed-package rebuild cooldown marker
-- Both are written by the failure path in a later change; started_at keeps
-- its MarkRunning-only semantics.

ALTER TABLE packages RENAME TO packages_legacy;
ALTER TABLE builds RENAME TO builds_legacy;
ALTER TABLE tasks RENAME TO tasks_legacy;

DROP INDEX idx_builds_package;
DROP INDEX idx_builds_status;
DROP INDEX idx_tasks_state;
DROP INDEX idx_tasks_active;

CREATE TABLE packages (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    pkgbase           TEXT NOT NULL UNIQUE,
    branch            TEXT NOT NULL,
    vcs_kind          TEXT NOT NULL DEFAULT '',
    arch              TEXT NOT NULL DEFAULT 'x86_64',
    enabled           INTEGER NOT NULL DEFAULT 1,
    current_version   TEXT NOT NULL DEFAULT '',
    pkgdesc           TEXT NOT NULL DEFAULT '',
    last_srcinfo_hash TEXT NOT NULL DEFAULT '',
    last_upstream_ref TEXT NOT NULL DEFAULT '',
    last_failed_at    TEXT,
    last_build_id     TEXT,
    maintainers       TEXT NOT NULL DEFAULT '[]'
);

CREATE TABLE builds (
    id             TEXT PRIMARY KEY,
    seq            INTEGER NOT NULL,
    package_id     INTEGER NOT NULL REFERENCES packages(id),
    branch         TEXT NOT NULL,
    "commit"       TEXT NOT NULL,
    upstream_ref   TEXT NOT NULL DEFAULT '',
    srcinfo_hash   TEXT NOT NULL,
    status         TEXT NOT NULL,
    worker_id      INTEGER,
    worker_name    TEXT NOT NULL DEFAULT '',
    log_path       TEXT NOT NULL DEFAULT '',
    started_at     TEXT,
    finished_at    TEXT,
    error          TEXT NOT NULL DEFAULT '',
    artifacts      TEXT NOT NULL DEFAULT '[]',
    resource_usage TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX idx_builds_package ON builds(package_id, seq DESC);
CREATE INDEX idx_builds_status  ON builds(status);

CREATE TABLE tasks (
    id               TEXT PRIMARY KEY,
    package_id       INTEGER NOT NULL REFERENCES packages(id),
    build_id         TEXT NOT NULL REFERENCES builds(id),
    state            TEXT NOT NULL,
    worker_id        INTEGER REFERENCES workers(id),
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

INSERT INTO packages
    (id, pkgbase, branch, vcs_kind, arch, enabled, current_version, pkgdesc,
     last_srcinfo_hash, last_upstream_ref, last_failed_at, last_build_id, maintainers)
    SELECT id, pkgbase, branch, vcs_kind, arch, enabled, current_version, pkgdesc,
           last_srcinfo_hash, last_upstream_ref, NULL,
           CASE WHEN last_build_id IS NULL THEN NULL
                ELSE printf('%016x', last_build_id) END, maintainers
    FROM packages_legacy;

INSERT INTO builds
    (id, seq, package_id, branch, "commit", upstream_ref, srcinfo_hash, status,
     worker_id, worker_name, log_path, started_at, finished_at, error,
     artifacts, resource_usage)
    SELECT printf('%016x', id), id, package_id, branch, "commit", upstream_ref,
           srcinfo_hash, status, worker_id, '', 'logs/' || printf('%016x', id) || '.log',
           started_at, finished_at, error, artifacts, resource_usage
    FROM builds_legacy;

INSERT INTO tasks
    (id, package_id, build_id, state, worker_id, assigned_at, created_at,
     last_progress_at, attempts, claim_token, cancel_requested, fail_count)
    SELECT id, package_id, printf('%016x', build_id), state, worker_id, assigned_at,
           created_at, last_progress_at, attempts, claim_token, cancel_requested, 0
    FROM tasks_legacy;

DROP TABLE tasks_legacy;
DROP TABLE builds_legacy;
DROP TABLE packages_legacy;
