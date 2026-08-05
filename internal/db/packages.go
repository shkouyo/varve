// SPDX-License-Identifier: AGPL-3.0-or-later
//
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

const packageColumns = `id, pkgbase, branch, vcs_kind, arch, enabled, current_version, pkgdesc, last_srcinfo_hash, last_upstream_ref, COALESCE(last_build_id, 0), maintainers`

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
		where = ` WHERE pkgbase LIKE ? OR pkgdesc LIKE ?`
		args = append(args, "%"+q+"%", "%"+q+"%")
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

// UpdatePackageAfterBuild records the outcome of a successful build on the
// package row. It is only valid inside WithTx. ErrNotFound when pkgbase is
// unknown.
func (t *Tx) UpdatePackageAfterBuild(ctx context.Context, pkgbase, currentVersion, pkgdesc, srcinfoHash, upstreamRef string, buildID int64) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE packages SET
		current_version = ?, pkgdesc = ?, last_srcinfo_hash = ?, last_upstream_ref = ?, last_build_id = ?
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
	var enabled, lastBuildID int64
	var maintainers string
	if err := rs.Scan(&p.ID, &p.Pkgbase, &p.Branch, &p.VCSKind, &p.Arch, &enabled,
		&p.CurrentVersion, &p.Pkgdesc, &p.LastSrcinfoHash, &p.LastUpstreamRef, &lastBuildID, &maintainers); err != nil {
		return nil, err
	}
	p.Enabled = enabled != 0
	p.LastBuildID = lastBuildID
	ms, err := decodeStrings(maintainers)
	if err != nil {
		return nil, fmt.Errorf("db: decode maintainers for package %q: %w", p.Pkgbase, err)
	}
	p.Maintainers = ms
	return &p, nil
}
