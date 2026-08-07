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
)

const buildColumns = `id, package_id, branch, "commit", upstream_ref, srcinfo_hash, status, COALESCE(worker_id, 0), worker_name, log_path, started_at, finished_at, error, artifacts, resource_usage`

// GetBuild returns one build by id with artifacts and resource usage
// decoded. ErrNotFound when the build does not exist.
func (s *Store) GetBuild(ctx context.Context, id string) (*Build, error) {
	b, err := scanBuild(s.read.QueryRowContext(ctx,
		`SELECT `+buildColumns+` FROM builds WHERE id = ?`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("db: get build %s: %w", id, err)
	}
	return b, nil
}

// LatestBuildForPackage returns the newest build row of a package (by
// seq, the insertion order), regardless of status. ErrNotFound when the
// package has no build yet. detect uses it for the rebuild cooldown: a
// package whose last build failed is compared against the failed build's
// snapshot to tell a stale difference from a fresh source change.
func (s *Store) LatestBuildForPackage(ctx context.Context, packageID int64) (*Build, error) {
	b, err := scanBuild(s.read.QueryRowContext(ctx,
		`SELECT `+buildColumns+` FROM builds WHERE package_id = ? ORDER BY seq DESC LIMIT 1`, packageID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("db: latest build for package %d: %w", packageID, err)
	}
	return b, nil
}

// ListBuilds returns the requested page of builds, newest first, with the
// total count. failedOnly restricts the result to failed builds.
func (s *Store) ListBuilds(ctx context.Context, page, perPage int, failedOnly bool) ([]Build, int, error) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 1
	}
	where := ""
	if failedOnly {
		where = ` WHERE status = 'failed'`
	}
	var total int
	if err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM builds`+where).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("db: count builds: %w", err)
	}
	query := `SELECT ` + buildColumns + ` FROM builds` + where + ` ORDER BY seq DESC LIMIT ? OFFSET ?`
	rows, err := s.read.QueryContext(ctx, query, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("db: list builds: %w", err)
	}
	defer rows.Close()
	out := []Build{}
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("db: list builds: %w", err)
		}
		out = append(out, *b)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("db: list builds: %w", err)
	}
	return out, total, nil
}

// ListBuildsByPackage returns the requested page of one package's builds,
// newest first, with the total count. The package detail page uses it so
// long build histories paginate instead of being truncated.
func (s *Store) ListBuildsByPackage(ctx context.Context, packageID int64, page, perPage int) ([]Build, int, error) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 1
	}
	var total int
	if err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM builds WHERE package_id = ?`, packageID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("db: count package builds: %w", err)
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+buildColumns+` FROM builds WHERE package_id = ? ORDER BY seq DESC LIMIT ? OFFSET ?`,
		packageID, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("db: list package builds: %w", err)
	}
	defer rows.Close()
	out := []Build{}
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("db: list package builds: %w", err)
		}
		out = append(out, *b)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("db: list package builds: %w", err)
	}
	return out, total, nil
}

// scanBuild decodes one builds row.
func scanBuild(rs rowScanner) (*Build, error) {
	var b Build
	var commit, artifacts, resourceUsage string
	var startedAtNS, finishedAtNS sql.NullString
	if err := rs.Scan(&b.ID, &b.PackageID, &b.Branch, &commit, &b.UpstreamRef, &b.SrcinfoHash,
		&b.Status, &b.WorkerID, &b.WorkerName, &b.LogPath, &startedAtNS, &finishedAtNS, &b.Error, &artifacts, &resourceUsage); err != nil {
		return nil, err
	}
	b.Commit = commit
	if startedAtNS.Valid {
		t, err := parseTime(startedAtNS.String)
		if err != nil {
			return nil, fmt.Errorf("db: decode started_at for build %s: %w", b.ID, err)
		}
		b.StartedAt = &t
	}
	if finishedAtNS.Valid {
		t, err := parseTime(finishedAtNS.String)
		if err != nil {
			return nil, fmt.Errorf("db: decode finished_at for build %s: %w", b.ID, err)
		}
		b.FinishedAt = &t
	}
	arts, err := decodeArtifacts(artifacts)
	if err != nil {
		return nil, fmt.Errorf("db: decode artifacts for build %s: %w", b.ID, err)
	}
	b.Artifacts = arts
	samples, err := decodeSamples(resourceUsage)
	if err != nil {
		return nil, fmt.Errorf("db: decode resource_usage for build %s: %w", b.ID, err)
	}
	// Keep only the most recent maxSamples entries so the render path
	// (web performance table) never sees unbounded history.
	b.ResourceUsage = capSamples(samples)
	return &b, nil
}
