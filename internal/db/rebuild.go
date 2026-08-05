// SPDX-License-Identifier: AGPL-3.0-or-later

// Copyright (C) 2026 ShinKouyo <i@0x0f.dev>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RebuildPackage is one authoritative package record fed to RebuildIndex.
// cmd/varve derives it from the storage side file of the package plus the
// previous packages row (DETAIL §13.3): the side file carries the branch,
// the VCS kind, the latest build metadata and the artifact manifest, while
// pkgdesc and maintainers — detection metadata absent from the side file —
// are preserved from the row being replaced.
type RebuildPackage struct {
	Pkgbase         string
	Branch          string
	VCSKind         string
	Arch            string
	CurrentVersion  string
	Pkgdesc         string
	LastSrcinfoHash string
	LastUpstreamRef string
	Maintainers     []string

	// Build metadata of the single (latest) build row to create.
	WorkerID    int64 // 0 = unknown worker (builds.worker_id NULL)
	Commit      string
	UpstreamRef string
	SrcinfoHash string
	BuiltAt     time.Time // ingest timestamp (side file [build].time), builds.finished_at
	Artifacts   []Artifact
}

// RebuildIndex clears the task queue and rebuilds the packages and builds
// tables from the authoritative side-file records in one write transaction
// (DETAIL §13.3, decision D5): tasks are emptied (in-flight and queued work
// is voided and naturally re-enqueued by the next detection round, A16),
// the packages table is recreated from the records, exactly one build row
// per record is created (the latest build per package, decision A20) and
// the workers table is left untouched. Any package not represented in the
// records is dropped together with its build history.
//
// The records must carry distinct, non-empty Pkgbases; the caller (the
// rebuild-index subcommand) is responsible for deduplicating side files.
// An empty record list still clears the index: a repository without side
// files must not retain stale packages. Added by the M12 cmd/varve module.
func (s *Store) RebuildIndex(ctx context.Context, pkgs []RebuildPackage) error {
	seen := make(map[string]bool, len(pkgs))
	for i := range pkgs {
		p := &pkgs[i]
		if p.Pkgbase == "" {
			return errors.New("db: RebuildIndex: record with empty pkgbase")
		}
		if seen[p.Pkgbase] {
			return fmt.Errorf("db: RebuildIndex: duplicate pkgbase %q", p.Pkgbase)
		}
		seen[p.Pkgbase] = true
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		// Order matters for the foreign keys: tasks reference builds and
		// packages, builds reference packages (and workers, untouched).
		if _, err := tx.ExecContext(ctx, `DELETE FROM tasks`); err != nil {
			return fmt.Errorf("db: rebuild index: clear tasks: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM builds`); err != nil {
			return fmt.Errorf("db: rebuild index: clear builds: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM packages`); err != nil {
			return fmt.Errorf("db: rebuild index: clear packages: %w", err)
		}
		for i := range pkgs {
			p := &pkgs[i]
			id, err := s.insertRebuiltPackage(ctx, tx, p)
			if err != nil {
				return err
			}
			buildID, err := s.insertRebuiltBuild(ctx, tx, p, id)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE packages SET last_build_id = ? WHERE id = ?`, buildID, id); err != nil {
				return fmt.Errorf("db: rebuild index: set last build for package %q: %w", p.Pkgbase, err)
			}
		}
		return nil
	})
}

// insertRebuiltPackage inserts one packages row from the record and returns
// its id. pkgbase is UNIQUE, so the caller's dedupe is the backstop.
func (s *Store) insertRebuiltPackage(ctx context.Context, tx *sql.Tx, p *RebuildPackage) (int64, error) {
	maintainers, err := encodeJSON(p.Maintainers)
	if err != nil {
		return 0, fmt.Errorf("db: rebuild index: encode maintainers for package %q: %w", p.Pkgbase, err)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO packages
		(pkgbase, branch, vcs_kind, arch, enabled, current_version, pkgdesc,
		 last_srcinfo_hash, last_upstream_ref, last_build_id, maintainers)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, NULL, ?)`,
		p.Pkgbase, p.Branch, p.VCSKind, p.Arch, p.CurrentVersion, p.Pkgdesc,
		p.LastSrcinfoHash, p.LastUpstreamRef, maintainers)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("db: rebuild index: duplicate pkgbase %q", p.Pkgbase)
		}
		return 0, fmt.Errorf("db: rebuild index: insert package %q: %w", p.Pkgbase, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("db: rebuild index: package %q id: %w", p.Pkgbase, err)
	}
	return id, nil
}

// insertRebuiltBuild inserts the single latest build row of a package and
// returns its id. The status is "succeeded" (a side file only exists after
// a successful ingest, DESIGN §3.2) and the log path is the conventional
// "logs/<build-id>.log" (DETAIL §2.4); the new id cannot point at the old
// on-disk log, which is an accepted consequence of A20.
func (s *Store) insertRebuiltBuild(ctx context.Context, tx *sql.Tx, p *RebuildPackage, packageID int64) (int64, error) {
	artifacts, err := encodeJSON(p.Artifacts)
	if err != nil {
		return 0, fmt.Errorf("db: rebuild index: encode artifacts for package %q: %w", p.Pkgbase, err)
	}
	var finishedAt any
	if !p.BuiltAt.IsZero() {
		finishedAt = formatTime(p.BuiltAt)
	}
	var workerID any
	if p.WorkerID != 0 {
		workerID = p.WorkerID
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO builds
		(package_id, branch, "commit", upstream_ref, srcinfo_hash, status,
		 worker_id, log_path, started_at, finished_at, error, artifacts, resource_usage)
		VALUES (?, ?, ?, ?, ?, 'succeeded', ?, '', NULL, ?, '', ?, '[]')`,
		packageID, p.Branch, p.Commit, p.UpstreamRef, p.SrcinfoHash,
		workerID, finishedAt, artifacts)
	if err != nil {
		return 0, fmt.Errorf("db: rebuild index: insert build for package %q: %w", p.Pkgbase, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("db: rebuild index: build id for package %q: %w", p.Pkgbase, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE builds SET log_path = ? WHERE id = ?`,
		fmt.Sprintf("logs/%d.log", id), id); err != nil {
		return 0, fmt.Errorf("db: rebuild index: set log path for package %q: %w", p.Pkgbase, err)
	}
	return id, nil
}
