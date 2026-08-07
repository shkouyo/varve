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
	"strings"
)

const packageColumns = `id, pkgbase, branch, vcs_kind, arch, enabled, current_version, pkgdesc, last_srcinfo_hash, last_upstream_ref, last_failed_at, COALESCE(last_build_id, ''), maintainers`

// GetPackageByBase returns one package by its pkgbase with maintainers
// decoded. ErrNotFound when the package does not exist.
func (s *Store) GetPackageByBase(ctx context.Context, pkgbase string) (*Package, error) {
	p, err := scanPackage(s.read.QueryRowContext(ctx,
		`SELECT `+packageColumns+` FROM packages WHERE pkgbase = ?`, pkgbase))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("db: get package %q: %w", pkgbase, err)
	}
	return p, nil
}

// ListPackages returns the requested page of packages matching q (a
// substring of pkgbase or pkgdesc; empty q matches everything) together
// with the total count. Rows are ordered by pkgbase for stable pagination.
func (s *Store) ListPackages(ctx context.Context, q string, page, perPage int) ([]Package, int, error) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 1
	}
	where := ""
	var args []any
	if q != "" {
		where = ` WHERE pkgbase LIKE ? ESCAPE '\' OR pkgdesc LIKE ? ESCAPE '\'`
		pattern := "%" + escapeLike(q) + "%"
		args = append(args, pattern, pattern)
	}
	var total int
	if err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM packages`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("db: count packages: %w", err)
	}
	query := `SELECT ` + packageColumns + ` FROM packages` + where + ` ORDER BY pkgbase LIMIT ? OFFSET ?`
	args = append(args, perPage, (page-1)*perPage)
	rows, err := s.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("db: list packages: %w", err)
	}
	defer rows.Close()
	out := []Package{}
	for rows.Next() {
		p, err := scanPackage(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("db: list packages: %w", err)
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("db: list packages: %w", err)
	}
	return out, total, nil
}

// GetPackageByID returns one package by its primary key with maintainers
// decoded. ErrNotFound when the package does not exist. It is used by task
// detail assembly and failure notifications, which resolve the package row
// through task.package_id.
func (s *Store) GetPackageByID(ctx context.Context, id int64) (*Package, error) {
	p, err := scanPackage(s.read.QueryRowContext(ctx,
		`SELECT `+packageColumns+` FROM packages WHERE id = ?`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("db: get package %d: %w", id, err)
	}
	return p, nil
}

// UpsertPackage inserts a new package row for an unknown pkgbase, or
// refreshes the mutable detection metadata (branch, vcs_kind, arch,
// maintainers) of an existing row. It never touches the build-derived
// records (current_version, pkgdesc, hashes, last_build_id), which are
// updated only after a successful build. Fills p.ID. Enqueueing a change
// whose pkgbase appears in the source for the first time needs a creation
// path; the maintainers snapshot is refreshed here at enqueue time.
func (s *Store) UpsertPackage(ctx context.Context, p *Package) error {
	if p == nil || p.Pkgbase == "" {
		return errors.New("db: UpsertPackage requires a package with a pkgbase")
	}
	maintainers, err := encodeJSON(p.Maintainers)
	if err != nil {
		return fmt.Errorf("db: encode maintainers for package %q: %w", p.Pkgbase, err)
	}
	var id int64
	err = s.write.QueryRowContext(ctx, `INSERT INTO packages
		(pkgbase, branch, vcs_kind, arch, enabled, maintainers)
		VALUES (?, ?, ?, ?, 1, ?)
		ON CONFLICT(pkgbase) DO UPDATE SET
			branch = excluded.branch,
			vcs_kind = excluded.vcs_kind,
			arch = excluded.arch,
			maintainers = excluded.maintainers
		RETURNING id`,
		p.Pkgbase, p.Branch, p.VCSKind, p.Arch, maintainers).Scan(&id)
	if err != nil {
		return fmt.Errorf("db: upsert package %q: %w", p.Pkgbase, err)
	}
	p.ID = id
	return nil
}

// UpdatePackageAfterBuild records the outcome of a successful build on the
// package row: the version/description/hash records advance and the
// last_failed_at rebuild-cooldown marker is cleared (a success proves the
// package builds again). It is only valid inside WithTx. ErrNotFound when
// pkgbase is unknown.
func (t *Tx) UpdatePackageAfterBuild(ctx context.Context, pkgbase, currentVersion, pkgdesc, srcinfoHash, upstreamRef string, buildID string) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE packages SET
		current_version = ?, pkgdesc = ?, last_srcinfo_hash = ?, last_upstream_ref = ?, last_build_id = ?, last_failed_at = NULL
		WHERE pkgbase = ?`,
		currentVersion, pkgdesc, srcinfoHash, upstreamRef, buildID, pkgbase)
	if err != nil {
		return fmt.Errorf("db: update package %q after build: %w", pkgbase, err)
	}
	return requireAffected(res, fmt.Sprintf("update package %q after build", pkgbase))
}

// scanPackage decodes one packages row.
func scanPackage(rs rowScanner) (*Package, error) {
	var p Package
	var enabled int64
	var maintainers string
	var lastFailedAt sql.NullString
	if err := rs.Scan(&p.ID, &p.Pkgbase, &p.Branch, &p.VCSKind, &p.Arch, &enabled,
		&p.CurrentVersion, &p.Pkgdesc, &p.LastSrcinfoHash, &p.LastUpstreamRef, &lastFailedAt, &p.LastBuildID, &maintainers); err != nil {
		return nil, err
	}
	p.Enabled = enabled != 0
	if lastFailedAt.Valid {
		at, err := parseTime(lastFailedAt.String)
		if err != nil {
			return nil, fmt.Errorf("db: decode last_failed_at for package %q: %w", p.Pkgbase, err)
		}
		p.LastFailedAt = &at
	}
	ms, err := decodeStrings(maintainers)
	if err != nil {
		return nil, fmt.Errorf("db: decode maintainers for package %q: %w", p.Pkgbase, err)
	}
	p.Maintainers = ms
	return &p, nil
}

// escapeLike escapes the LIKE metacharacters of a user-supplied search
// term so the query matches them literally instead of as wildcards.
func escapeLike(q string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_")
	return replacer.Replace(q)
}
