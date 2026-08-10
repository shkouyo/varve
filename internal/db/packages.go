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
	"time"
)

const packageColumns = `id, pkgbase, branch, vcs_kind, arch, current_version, pkgdesc, url, licenses, conflicts, provides, pkgname, source, pkgver, pkgrel, epoch, last_commit, last_srcinfo_hash, last_upstream_ref, pkgbuild_ref, last_failed_at, COALESCE(last_build_id, ''), maintainers, aur_name, aur_submit, last_aur_push_at, last_aur_commit, last_aur_error`

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
// maintainers and the .SRCINFO-derived url/licenses/conflicts/provides/
// pkgname/source/pkgver/pkgrel) of an existing row. It never touches the
// build-derived records (current_version, pkgdesc, hashes, last_build_id),
// which are updated only after a successful build. Fills p.ID.
// Enqueueing a change whose pkgbase appears in the source for the first
// time needs a creation path; the maintainers snapshot is refreshed here
// at enqueue time.
//
// Empty incoming values never clobber an existing row: the metadata
// columns fall back to the stored value when the incoming one is empty
// ("" or "null" for the JSON string arrays, 0 for epoch). A rebuild or
// re-check path that misses a field therefore cannot wipe already
// recorded metadata; only non-empty detection output advances the row.
// vcs_kind and aur_submit are exempt: "" and false are meaningful
// transitions of their own.
func (s *Store) UpsertPackage(ctx context.Context, p *Package) error {
	if p == nil || p.Pkgbase == "" {
		return errors.New("db: UpsertPackage requires a package with a pkgbase")
	}
	maintainers, err := encodeJSON(p.Maintainers)
	if err != nil {
		return fmt.Errorf("db: encode maintainers for package %q: %w", p.Pkgbase, err)
	}
	licenses, err := encodeJSON(p.Licenses)
	if err != nil {
		return fmt.Errorf("db: encode licenses for package %q: %w", p.Pkgbase, err)
	}
	conflicts, err := encodeJSON(p.Conflicts)
	if err != nil {
		return fmt.Errorf("db: encode conflicts for package %q: %w", p.Pkgbase, err)
	}
	provides, err := encodeJSON(p.Provides)
	if err != nil {
		return fmt.Errorf("db: encode provides for package %q: %w", p.Pkgbase, err)
	}
	pkgname, err := encodeJSON(p.Pkgname)
	if err != nil {
		return fmt.Errorf("db: encode pkgname for package %q: %w", p.Pkgbase, err)
	}
	source, err := encodeJSON(p.Source)
	if err != nil {
		return fmt.Errorf("db: encode source for package %q: %w", p.Pkgbase, err)
	}
	var id int64
	err = s.write.QueryRowContext(ctx, `INSERT INTO packages
		(pkgbase, branch, vcs_kind, arch, maintainers, aur_name, aur_submit, url, licenses, conflicts, provides,
		 pkgname, source, pkgver, pkgrel, epoch)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(pkgbase) DO UPDATE SET
			branch = CASE WHEN excluded.branch = '' THEN packages.branch ELSE excluded.branch END,
			vcs_kind = excluded.vcs_kind,
			arch = CASE WHEN excluded.arch = '' THEN packages.arch ELSE excluded.arch END,
			maintainers = CASE WHEN excluded.maintainers IN ('', 'null') THEN packages.maintainers ELSE excluded.maintainers END,
			aur_name = CASE WHEN excluded.aur_name = '' THEN packages.aur_name ELSE excluded.aur_name END,
			aur_submit = excluded.aur_submit,
			url = CASE WHEN excluded.url = '' THEN packages.url ELSE excluded.url END,
			arch = CASE WHEN excluded.arch = '' THEN packages.arch ELSE excluded.arch END,
			maintainers = CASE WHEN excluded.maintainers IN ('', 'null') THEN packages.maintainers ELSE excluded.maintainers END,
			aur_name = CASE WHEN excluded.aur_name = '' THEN packages.aur_name ELSE excluded.aur_name END,
			aur_submit = excluded.aur_submit,
			url = CASE WHEN excluded.url = '' THEN packages.url ELSE excluded.url END,
			licenses = CASE WHEN excluded.licenses IN ('', 'null') THEN packages.licenses ELSE excluded.licenses END,
			conflicts = CASE WHEN excluded.conflicts IN ('', 'null') THEN packages.conflicts ELSE excluded.conflicts END,
			provides = CASE WHEN excluded.provides IN ('', 'null') THEN packages.provides ELSE excluded.provides END,
			pkgname = CASE WHEN excluded.pkgname IN ('', 'null') THEN packages.pkgname ELSE excluded.pkgname END,
			source = CASE WHEN excluded.source IN ('', 'null') THEN packages.source ELSE excluded.source END,
			pkgver = CASE WHEN excluded.pkgver = '' THEN packages.pkgver ELSE excluded.pkgver END,
			pkgrel = CASE WHEN excluded.pkgrel = '' THEN packages.pkgrel ELSE excluded.pkgrel END,
			epoch = CASE WHEN excluded.epoch = 0 THEN packages.epoch ELSE excluded.epoch END
		RETURNING id`,
		p.Pkgbase, p.Branch, p.VCSKind, p.Arch, maintainers, p.AURName, p.AURSubmit, p.URL, licenses, conflicts, provides,
		pkgname, source, p.Pkgver, p.Pkgrel, p.Epoch).Scan(&id)
	if err != nil {
		return fmt.Errorf("db: upsert package %q: %w", p.Pkgbase, err)
	}
	p.ID = id
	return nil
}

// UpdatePackageAfterBuild records the outcome of a successful build on the
// package row: the version/description/hash records advance, the .SRCINFO
// metadata is refreshed and the last_failed_at rebuild-cooldown marker is
// cleared (a success proves the package builds again). It is only valid
// inside WithTx. ErrNotFound when pkgbase is unknown.
func (t *Tx) UpdatePackageAfterBuild(ctx context.Context, pkgbase string, u PackageUpdate) error {
	licenses, err := encodeJSON(u.Licenses)
	if err != nil {
		return fmt.Errorf("db: encode licenses for package %q: %w", pkgbase, err)
	}
	conflicts, err := encodeJSON(u.Conflicts)
	if err != nil {
		return fmt.Errorf("db: encode conflicts for package %q: %w", pkgbase, err)
	}
	provides, err := encodeJSON(u.Provides)
	if err != nil {
		return fmt.Errorf("db: encode provides for package %q: %w", pkgbase, err)
	}
	pkgname, err := encodeJSON(u.Pkgname)
	if err != nil {
		return fmt.Errorf("db: encode pkgname for package %q: %w", pkgbase, err)
	}
	source, err := encodeJSON(u.Source)
	if err != nil {
		return fmt.Errorf("db: encode source for package %q: %w", pkgbase, err)
	}
	res, err := t.tx.ExecContext(ctx, `UPDATE packages SET
		current_version = ?, pkgdesc = ?, url = ?, licenses = ?, conflicts = ?, provides = ?,
		pkgname = ?, source = ?, pkgver = ?, pkgrel = ?, epoch = ?,
		last_commit = ?, last_srcinfo_hash = ?, last_upstream_ref = ?, pkgbuild_ref = ?, last_build_id = ?, last_failed_at = NULL
		WHERE pkgbase = ?`,
		u.CurrentVersion, u.Pkgdesc, u.URL, licenses, conflicts, provides,
		pkgname, source, u.Pkgver, u.Pkgrel, u.Epoch,
		u.Commit, u.SrcinfoHash, u.UpstreamRef, u.PkgbuildRef, u.BuildID, pkgbase)
	if err != nil {
		return fmt.Errorf("db: update package %q after build: %w", pkgbase, err)
	}
	return requireAffected(res, fmt.Sprintf("update package %q after build", pkgbase))
}

// RecordAURPush records the outcome of one AUR push attempt on the package
// row: the attempted commit, the attempt time and the error text (empty on
// success). Both successes and failures are recorded so the package page
// can show the last publish state; a push outcome never affects the build
// records themselves.
func (s *Store) RecordAURPush(ctx context.Context, pkgbase, commit string, attemptedAt time.Time, errMsg string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE packages SET
		last_aur_push_at = ?, last_aur_commit = ?, last_aur_error = ?
		WHERE pkgbase = ?`,
		formatTime(attemptedAt), commit, errMsg, pkgbase)
	if err != nil {
		return fmt.Errorf("db: record AUR push for package %q: %w", pkgbase, err)
	}
	return requireAffected(res, fmt.Sprintf("record AUR push for package %q", pkgbase))
}

// scanPackage decodes one packages row.
func scanPackage(rs rowScanner) (*Package, error) {
	var p Package
	var maintainers, licenses, conflicts, provides, pkgname, source string
	var lastFailedAt, lastAURPushAt sql.NullString
	var aurSubmit int
	if err := rs.Scan(&p.ID, &p.Pkgbase, &p.Branch, &p.VCSKind, &p.Arch,
		&p.CurrentVersion, &p.Pkgdesc, &p.URL, &licenses, &conflicts, &provides,
		&pkgname, &source, &p.Pkgver, &p.Pkgrel, &p.Epoch,
		&p.LastCommit, &p.LastSrcinfoHash, &p.LastUpstreamRef, &p.PkgbuildRef, &lastFailedAt, &p.LastBuildID,
		&maintainers, &p.AURName, &aurSubmit, &lastAURPushAt, &p.LastAURCommit, &p.LastAURError); err != nil {
		return nil, err
	}
	p.AURSubmit = aurSubmit != 0
	if lastFailedAt.Valid {
		at, err := parseTime(lastFailedAt.String)
		if err != nil {
			return nil, fmt.Errorf("db: decode last_failed_at for package %q: %w", p.Pkgbase, err)
		}
		p.LastFailedAt = &at
	}
	if lastAURPushAt.Valid {
		at, err := parseTime(lastAURPushAt.String)
		if err != nil {
			return nil, fmt.Errorf("db: decode last_aur_push_at for package %q: %w", p.Pkgbase, err)
		}
		p.LastAURPushAt = &at
	}
	ms, err := decodeMaintainers(maintainers)
	if err != nil {
		return nil, fmt.Errorf("db: decode maintainers for package %q: %w", p.Pkgbase, err)
	}
	p.Maintainers = ms
	ls, err := decodeStrings(licenses)
	if err != nil {
		return nil, fmt.Errorf("db: decode licenses for package %q: %w", p.Pkgbase, err)
	}
	p.Licenses = ls
	cs, err := decodeStrings(conflicts)
	if err != nil {
		return nil, fmt.Errorf("db: decode conflicts for package %q: %w", p.Pkgbase, err)
	}
	p.Conflicts = cs
	ps, err := decodeStrings(provides)
	if err != nil {
		return nil, fmt.Errorf("db: decode provides for package %q: %w", p.Pkgbase, err)
	}
	p.Provides = ps
	pns, err := decodeStrings(pkgname)
	if err != nil {
		return nil, fmt.Errorf("db: decode pkgname for package %q: %w", p.Pkgbase, err)
	}
	p.Pkgname = pns
	scs, err := decodeStrings(source)
	if err != nil {
		return nil, fmt.Errorf("db: decode source for package %q: %w", p.Pkgbase, err)
	}
	p.Source = scs
	return &p, nil
}

// escapeLike escapes the LIKE metacharacters of a user-supplied search
// term so the query matches them literally instead of as wildcards.
func escapeLike(q string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_")
	return replacer.Replace(q)
}
