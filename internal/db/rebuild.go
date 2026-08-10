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
// previous packages row: the side file carries the branch, the VCS kind, the
// latest build metadata and the artifact manifest, while the detection
// metadata absent from the side file (pkgdesc, maintainers, url, licenses,
// conflicts, provides, pkgname, source, pkgver, pkgrel, epoch, pkgbuild_ref
// and the AUR record) is preserved from the row being replaced.
type RebuildPackage struct {
	Pkgbase         string
	Branch          string
	VCSKind         string
	Arch            string
	CurrentVersion  string
	Pkgdesc         string
	URL             string
	Licenses        []string
	Conflicts       []string
	Provides        []string
	Pkgname         []string
	Source          []string
	Pkgver          string
	Pkgrel          string
	Epoch           int
	PkgbuildRef     string
	LastFailedAt    *time.Time // rebuild-cooldown marker, preserved
	LastSrcinfoHash string
	LastUpstreamRef string
	Maintainers     []Maintainer

	// AUR publishing record (see Package); preserved across the rebuild
	// so the package page keeps the last publish state.
	AURName       string
	AURSubmit     bool
	LastAURPushAt *time.Time
	LastAURCommit string
	LastAURError  string

	// Build metadata of the single (latest) build row to create.
	WorkerID    int64  // 0 = unknown worker (builds.worker_id NULL)
	WorkerName  string // plain-text executing machine name (builds.worker_name)
	Commit      string
	UpstreamRef string
	SrcinfoHash string
	BuiltAt     time.Time // ingest timestamp (side file [build].time), builds.finished_at
	Artifacts   []Artifact
}

// RebuildIndex clears the task queue and rebuilds the packages and builds
// tables from the authoritative side-file records in one write transaction:
// tasks are emptied (in-flight and queued work is voided and naturally
// re-enqueued by the next detection round), the packages table is recreated
// from the records, exactly one build row per record is created (the latest
// build per package) and the workers table is left untouched. Any package not
// represented in the records is dropped together with its build history.
//
// The returned list holds the build ids that were removed (every build row
// that existed before the rebuild, including the ones of packages that
// remain); the caller uses it to clean up on-disk artifacts such as the
// orphaned log files. An empty record list still clears the index and
// returns every removed build id.
//
// The records must carry distinct, non-empty Pkgbases; the caller (the
// rebuild-index subcommand) is responsible for deduplicating side files. An
// empty record list still clears the index: a repository without side files
// must not retain stale packages.
func (s *Store) RebuildIndex(ctx context.Context, pkgs []RebuildPackage) ([]string, error) {
	seen := make(map[string]bool, len(pkgs))
	for i := range pkgs {
		p := &pkgs[i]
		if p.Pkgbase == "" {
			return nil, errors.New("db: RebuildIndex: record with empty pkgbase")
		}
		if seen[p.Pkgbase] {
			return nil, fmt.Errorf("db: RebuildIndex: duplicate pkgbase %q", p.Pkgbase)
		}
		seen[p.Pkgbase] = true
	}
	var removed []string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		// Collect every build id before the clears so the caller can
		// remove the on-disk logs of dropped builds.
		rows, err := tx.QueryContext(ctx, `SELECT id FROM builds`)
		if err != nil {
			return fmt.Errorf("db: rebuild index: collect build ids: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("db: rebuild index: scan build id: %w", err)
			}
			removed = append(removed, id)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("db: rebuild index: collect build ids: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("db: rebuild index: collect build ids: %w", err)
		}
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
	return removed, err
}

// insertRebuiltPackage inserts one packages row from the record and returns
// its id. pkgbase is UNIQUE, so the caller's dedupe is the backstop. The
// last_commit record is the record's latest build commit, so a branch tip
// that still matches it does not re-trigger detection after the rebuild.
func (s *Store) insertRebuiltPackage(ctx context.Context, tx *sql.Tx, p *RebuildPackage) (int64, error) {
	maintainers, err := encodeJSON(p.Maintainers)
	if err != nil {
		return 0, fmt.Errorf("db: rebuild index: encode maintainers for package %q: %w", p.Pkgbase, err)
	}
	licenses, err := encodeJSON(p.Licenses)
	if err != nil {
		return 0, fmt.Errorf("db: rebuild index: encode licenses for package %q: %w", p.Pkgbase, err)
	}
	conflicts, err := encodeJSON(p.Conflicts)
	if err != nil {
		return 0, fmt.Errorf("db: rebuild index: encode conflicts for package %q: %w", p.Pkgbase, err)
	}
	provides, err := encodeJSON(p.Provides)
	if err != nil {
		return 0, fmt.Errorf("db: rebuild index: encode provides for package %q: %w", p.Pkgbase, err)
	}
	pkgname, err := encodeJSON(p.Pkgname)
	if err != nil {
		return 0, fmt.Errorf("db: rebuild index: encode pkgname for package %q: %w", p.Pkgbase, err)
	}
	source, err := encodeJSON(p.Source)
	if err != nil {
		return 0, fmt.Errorf("db: rebuild index: encode source for package %q: %w", p.Pkgbase, err)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO packages
		(pkgbase, branch, vcs_kind, arch, current_version, pkgdesc, url, licenses, conflicts, provides,
		 pkgname, source, pkgver, pkgrel, epoch, pkgbuild_ref, last_failed_at,
		 last_commit, last_srcinfo_hash, last_upstream_ref, last_build_id, maintainers,
		 aur_name, aur_submit, last_aur_push_at, last_aur_commit, last_aur_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?)`,
		p.Pkgbase, p.Branch, p.VCSKind, p.Arch, p.CurrentVersion, p.Pkgdesc, p.URL, licenses, conflicts, provides,
		pkgname, source, p.Pkgver, p.Pkgrel, p.Epoch, p.PkgbuildRef, formatNullableTime(p.LastFailedAt),
		p.Commit, p.LastSrcinfoHash, p.LastUpstreamRef, maintainers,
		p.AURName, p.AURSubmit, formatNullableTime(p.LastAURPushAt), p.LastAURCommit, p.LastAURError)
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
// a successful ingest) and the log path is the conventional
// "logs/<build-id>.log"; the new id cannot point at the old on-disk log,
// which is an accepted consequence of the rebuild.
func (s *Store) insertRebuiltBuild(ctx context.Context, tx *sql.Tx, p *RebuildPackage, packageID int64) (string, error) {
	artifacts, err := encodeJSON(p.Artifacts)
	if err != nil {
		return "", fmt.Errorf("db: rebuild index: encode artifacts for package %q: %w", p.Pkgbase, err)
	}
	var finishedAt any
	if !p.BuiltAt.IsZero() {
		finishedAt = formatTime(p.BuiltAt)
	}
	var workerID any
	if p.WorkerID != 0 {
		workerID = p.WorkerID
	}
	id, err := newBuildID(ctx, tx)
	if err != nil {
		return "", fmt.Errorf("db: rebuild index: build id for package %q: %w", p.Pkgbase, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO builds
		(id, seq, package_id, branch, "commit", upstream_ref, srcinfo_hash, status,
		 worker_id, worker_name, log_path, started_at, finished_at, error, artifacts, resource_usage)
		VALUES (?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM builds), ?, ?, ?, ?, ?, 'succeeded', ?, ?, '', NULL, ?, '', ?, '[]')`,
		id, packageID, p.Branch, p.Commit, p.UpstreamRef, p.SrcinfoHash,
		workerID, p.WorkerName, finishedAt, artifacts); err != nil {
		return "", fmt.Errorf("db: rebuild index: insert build for package %q: %w", p.Pkgbase, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE builds SET log_path = ? WHERE id = ?`,
		fmt.Sprintf("logs/%s.log", id), id); err != nil {
		return "", fmt.Errorf("db: rebuild index: set log path for package %q: %w", p.Pkgbase, err)
	}
	return id, nil
}
