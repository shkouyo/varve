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
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

// TestMigrateUpgradeFromEachVersion upgrades a database from every
// intermediate version (1..10) and asserts the store opens cleanly with a
// complete ledger and usable data. Only the v1 upgrade carries a data-level
// assertion (TestMigrateUpgradeFromV1); the intermediate starts exercise
// every migration's "apply on top of the previous version" path.
func TestMigrateUpgradeFromEachVersion(t *testing.T) {
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	sort.Strings(names)
	for i := 1; i < len(names); i++ {
		name := fmt.Sprintf("from v%d", i)
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "upgrade.db")
			legacy, err := sql.Open("sqlite", writeDSN(path))
			if err != nil {
				t.Fatalf("open legacy db: %v", err)
			}
			if _, err := legacy.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
				t.Fatalf("create ledger table: %v", err)
			}
			for _, m := range names[:i] {
				script, err := fs.ReadFile(migrationsFS, m)
				if err != nil {
					t.Fatalf("read migration %s: %v", m, err)
				}
				if _, err := legacy.Exec(string(script)); err != nil {
					t.Fatalf("apply migration %s: %v", m, err)
				}
				v, err := migrationVersion(m)
				if err != nil {
					t.Fatalf("version of %s: %v", m, err)
				}
				if _, err := legacy.Exec(
					`INSERT INTO schema_migrations (version, applied_at) VALUES (?, 'x')`, v); err != nil {
					t.Fatalf("seed ledger for %s: %v", m, err)
				}
			}
			if err := legacy.Close(); err != nil {
				t.Fatalf("close legacy db: %v", err)
			}

			s, err := Open(path)
			if err != nil {
				t.Fatalf("Open after upgrade from v%d: %v", i, err)
			}
			defer s.Close()

			var versions []int
			rows, err := s.read.Query(`SELECT version FROM schema_migrations ORDER BY version`)
			if err != nil {
				t.Fatalf("query ledger: %v", err)
			}
			for rows.Next() {
				var v int
				if err := rows.Scan(&v); err != nil {
					t.Fatal(err)
				}
				versions = append(versions, v)
			}
			rows.Close()
			assertMigrationLedger(t, versions)

			// The migrated store is usable for writes and reads.
			pkg := mustSeedPackage(t, s, fmt.Sprintf("pkg-v%d", i))
			if pkg.ID == 0 {
				t.Error("seeded package has no id")
			}
			if got, err := s.GetPackageByBase(testCtx, pkg.Pkgbase); err != nil || got.ID != pkg.ID {
				t.Errorf("GetPackageByBase(%q) = %+v, %v; want the seeded row", pkg.Pkgbase, got, err)
			}
		})
	}
}

// TestMigrateFailureRollsBack asserts a failing migration script leaves no
// partial schema and no ledger row behind (each migration is one
// transaction), and that the database remains openable afterwards. The
// failing script is injected through the migrationsFS seam.
func TestMigrateFailureRollsBack(t *testing.T) {
	orig := migrationsFS
	t.Cleanup(func() { migrationsFS = orig })
	wantCommitted := len(migrationVersions(t))

	files := make(map[string]*fstest.MapFile)
	if err := fs.WalkDir(orig, "migrations", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(orig, path)
		if err != nil {
			return err
		}
		files[path] = &fstest.MapFile{Data: data}
		return nil
	}); err != nil {
		t.Fatalf("walk embedded migrations: %v", err)
	}
	files["migrations/099_boom.sql"] = &fstest.MapFile{
		Data: []byte("CREATE TABLE boom (x INTEGER);\nTHIS IS NOT SQL;\n"),
	}
	migrationsFS = fstest.MapFS(files)

	path := filepath.Join(t.TempDir(), "boom.db")
	s, err := Open(path)
	if err == nil {
		s.Close()
		t.Fatal("Open with an injected failing migration must fail")
	}
	if !strings.Contains(err.Error(), "099_boom") {
		t.Errorf("error = %q, want the failing migration named", err)
	}

	// The failing transaction rolled back: no boom table, no 099 ledger
	// row; the earlier migrations each committed in their own transaction.
	probe, err := sql.Open("sqlite", writeDSN(path))
	if err != nil {
		t.Fatalf("open probe connection: %v", err)
	}
	defer probe.Close()
	var n int
	if err := probe.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'boom'`).Scan(&n); err != nil {
		t.Fatalf("probe boom table: %v", err)
	}
	if n != 0 {
		t.Error("boom table survived the rolled-back migration")
	}
	if err := probe.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 99`).Scan(&n); err != nil {
		t.Fatalf("probe ledger: %v", err)
	}
	if n != 0 {
		t.Error("version 99 was recorded despite the failure")
	}
	if err := probe.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("probe ledger count: %v", err)
	}
	if n != wantCommitted {
		t.Errorf("ledger rows = %d, want %d (earlier migrations committed)", n, wantCommitted)
	}

	// The database is not corrupted: a normal Open succeeds.
	migrationsFS = orig
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open after failed migration: %v", err)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestMigrateConcurrentOpen opens the same new database from two
// goroutines at once: at most one side may fail (migration races serialize
// on the single write connection) and the surviving database must be
// complete and usable.
func TestMigrateConcurrentOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conc.db")
	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			s, err := Open(path)
			if err == nil {
				s.Close()
				return
			}
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	failures := 0
	for _, err := range errs {
		if err != nil {
			failures++
			t.Logf("concurrent open error: %v", err)
		}
	}
	if failures > 1 {
		t.Fatalf("both concurrent Opens failed: %v and %v", errs[0], errs[1])
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after concurrent Opens: %v", err)
	}
	defer s.Close()
	var versions []int
	rows, err := s.read.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query ledger: %v", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	rows.Close()
	assertMigrationLedger(t, versions)
	mustSeedPackage(t, s, "conc-pkg")
}

// TestOpenCorruptedFile asserts Open fails with a readable wrapped error on
// a file that is not a sqlite database.
func TestOpenCorruptedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.db")
	if err := os.WriteFile(path, []byte("garbage, not sqlite"), 0o644); err != nil {
		t.Fatalf("write garbage db: %v", err)
	}
	s, err := Open(path)
	if err == nil {
		s.Close()
		t.Fatal("Open of a non-sqlite file must fail")
	}
	if !strings.Contains(err.Error(), "db:") {
		t.Errorf("error = %q, want the db: prefix", err)
	}
}
