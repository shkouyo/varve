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
	"strings"
	"testing"
)

// TestMigrateFresh asserts that an empty database is migrated to the
// latest schema with every table and index present and WAL journaling.
func TestMigrateFresh(t *testing.T) {
	s := newFileTestStore(t) // file-backed so WAL is real

	tables := []string{"packages", "builds", "workers", "tasks", "schema_migrations"}
	for _, tbl := range tables {
		var n int
		if err := s.read.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, tbl).Scan(&n); err != nil {
			t.Fatalf("check table %s: %v", tbl, err)
		}
		if n != 1 {
			t.Errorf("table %s missing after migrate", tbl)
		}
	}
	indexes := []string{"idx_builds_package", "idx_builds_status", "idx_tasks_state", "idx_tasks_active"}
	for _, idx := range indexes {
		var n int
		if err := s.read.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&n); err != nil {
			t.Fatalf("check index %s: %v", idx, err)
		}
		if n != 1 {
			t.Errorf("index %s missing after migrate", idx)
		}
	}

	// Partial index predicate must be active (queued duplicates rejected).
	pkg := mustSeedPackage(t, s, "pkg-a")
	createTask(t, s, "t1", "queued", pkg, at(0))
	task2, build2 := newTaskBuild("t2", "queued", pkg, at(0))
	if err := s.CreateTask(testCtx, task2, build2); err != ErrConflict {
		t.Fatalf("second queued task for same package: got %v, want ErrConflict", err)
	}

	// schema_migrations records every migration exactly once.
	var versions []int
	rows, err := s.read.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	if len(versions) != 4 || versions[0] != 1 || versions[1] != 2 || versions[2] != 3 || versions[3] != 4 {
		t.Errorf("schema_migrations = %v, want [1 2 3 4]", versions)
	}

	// WAL journal mode.
	var mode string
	if err := s.read.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

// TestMigrateIdempotent asserts that reopening the same database does not
// re-apply migrations and keeps existing data.
func TestMigrateIdempotent(t *testing.T) {
	path := t.TempDir() + "/test.db"
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	pkg := seedPackage(t, s1, Package{Pkgbase: "keepme", Branch: "main", Arch: "x86_64", Enabled: true})
	createTask(t, s1, "keep-task", "queued", pkg, at(0))
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()
	if _, err := s2.GetPackageByBase(testCtx, "keepme"); err != nil {
		t.Fatalf("package lost across reopen: %v", err)
	}
	if _, err := s2.GetTask(testCtx, "keep-task"); err != nil {
		t.Fatalf("task lost across reopen: %v", err)
	}
	var n int
	if err := s2.read.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if n != 4 {
		t.Errorf("schema_migrations count = %d after reopen, want 4", n)
	}
}

// TestMigrateFromFixture simulates a database with an existing migration
// history: it opens the real database, records an extra future version in
// the ledger, and asserts that a fresh Open recognizes the history, skips
// the applied migration and stays fully usable.
func TestMigrateFromFixture(t *testing.T) {
	path := t.TempDir() + "/test.db"
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := s1.write.Exec(
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, 999, formatTime(at(0))); err != nil {
		t.Fatalf("record fixture version: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen with fixture history: %v", err)
	}
	defer s2.Close()

	// Migrations 1-3 were already applied and must not run again; the
	// store is usable and the ledger unchanged.
	pkg := mustSeedPackage(t, s2, "pkg-fixture")
	createTask(t, s2, "fixture-task", "queued", pkg, at(0))
	var versions []int
	rows, err := s2.read.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	if len(versions) != 5 || versions[0] != 1 || versions[1] != 2 || versions[2] != 3 || versions[3] != 4 || versions[4] != 999 {
		t.Errorf("schema_migrations = %v, want [1 2 3 4 999]", versions)
	}
}

// TestMigrationVersion exercises the filename version parser.
func TestMigrationVersion(t *testing.T) {
	cases := []struct {
		name    string
		want    int
		wantErr bool
	}{
		{"migrations/001_init.sql", 1, false},
		{"migrations/012_add_foo.sql", 12, false},
		{"migrations/0_leading.sql", 0, false},
		{"migrations/noname.sql", 0, true},
	}
	for _, tc := range cases {
		got, err := migrationVersion(tc.name)
		if (err != nil) != tc.wantErr {
			t.Errorf("migrationVersion(%q) error = %v, wantErr %v", tc.name, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("migrationVersion(%q) = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestOpenEmptyPath guards the Open precondition.
func TestOpenEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("Open(\"\") error = %v, want empty-path error", err)
	}
}

// TestSchemaRawSQL ensures the reserved column name "commit" is queryable
// through raw SQL, guarding the migration DDL quoting.
func TestSchemaRawSQL(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "pkg-commit")
	_, b := createTask(t, s, "task-commit", "queued", pkg, at(0))
	var commit string
	if err := s.read.QueryRow(`SELECT "commit" FROM builds WHERE id = ?`, b.ID).Scan(&commit); err != nil {
		t.Fatalf("select commit: %v", err)
	}
	if commit != "deadbeef" {
		t.Errorf("commit = %q, want deadbeef", commit)
	}
}

// TestStoreConcurrency guards that concurrent writes through the single
// write connection do not corrupt the database.
func TestStoreConcurrency(t *testing.T) {
	s := newTestStore(t)
	const n = 8
	done := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			pkg := Package{Pkgbase: "conc-" + string(rune('a'+i)), Branch: "main", Arch: "x86_64", Enabled: true}
			done <- seedPackageErr(s, pkg)
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent seed: %v", err)
		}
	}
	workers, err := s.ListWorkers(testCtx)
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	if len(workers) != 0 {
		t.Errorf("workers = %d, want 0", len(workers))
	}
}

func seedPackageErr(s *Store, p Package) error {
	m, err := encodeJSON(p.Maintainers)
	if err != nil {
		return err
	}
	_, err = s.write.Exec(`INSERT INTO packages
		(pkgbase, branch, vcs_kind, arch, enabled, current_version, pkgdesc, last_srcinfo_hash, last_upstream_ref, last_build_id, maintainers)
		VALUES (?, ?, ?, ?, 1, '', '', '', '', NULL, ?)`,
		p.Pkgbase, p.Branch, p.VCSKind, p.Arch, m)
	return err
}
