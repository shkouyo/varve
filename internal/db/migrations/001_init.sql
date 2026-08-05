-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Copyright (C) 2026 ShinKouyo <i@0x0f.dev>
--
-- Licensed under the GNU Affero General Public License v3 or later;
-- see COPYING for the full license text.

-- Initial schema (DESIGN 3.1). "commit" is a reserved word in SQLite and
-- is therefore quoted everywhere it appears. idx_tasks_active is UNIQUE
-- per DETAIL 2.2 (enqueue dedupe): the DESIGN DDL text omits UNIQUE but
-- the ErrConflict contract requires it.

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
    last_build_id     INTEGER,
    maintainers       TEXT NOT NULL DEFAULT '[]'
);

CREATE TABLE builds (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    package_id     INTEGER NOT NULL REFERENCES packages(id),
    branch         TEXT NOT NULL,
    "commit"       TEXT NOT NULL,
    upstream_ref   TEXT NOT NULL DEFAULT '',
    srcinfo_hash   TEXT NOT NULL,
    status         TEXT NOT NULL,
    worker_id      INTEGER REFERENCES workers(id),
    log_path       TEXT NOT NULL DEFAULT '',
    started_at     TEXT,
    finished_at    TEXT,
    error          TEXT NOT NULL DEFAULT '',
    artifacts      TEXT NOT NULL DEFAULT '[]',
    resource_usage TEXT NOT NULL DEFAULT '[]'
);

CREATE INDEX idx_builds_package ON builds(package_id, id DESC);
CREATE INDEX idx_builds_status  ON builds(status);

CREATE TABLE workers (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    name           TEXT NOT NULL UNIQUE,
    role           TEXT NOT NULL,
    mode           TEXT NOT NULL,
    arch           TEXT NOT NULL DEFAULT 'x86_64',
    capacity       INTEGER NOT NULL DEFAULT 1,
    status         TEXT NOT NULL DEFAULT 'online',
    last_heartbeat TEXT,
    version        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE tasks (
    id               TEXT PRIMARY KEY,
    package_id       INTEGER NOT NULL REFERENCES packages(id),
    build_id         INTEGER NOT NULL REFERENCES builds(id),
    state            TEXT NOT NULL,
    worker_id        INTEGER REFERENCES workers(id),
    assigned_at      TEXT,
    created_at       TEXT NOT NULL,
    last_progress_at TEXT NOT NULL,
    attempts         INTEGER NOT NULL DEFAULT 0,
    claim_token      TEXT NOT NULL DEFAULT '',
    cancel_requested INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_tasks_state ON tasks(state, created_at);
CREATE UNIQUE INDEX idx_tasks_active ON tasks(package_id)
    WHERE state IN ('queued', 'assigned', 'running');
