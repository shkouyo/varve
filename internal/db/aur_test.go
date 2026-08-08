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
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestMigrateAURColumns builds a database that only ran migration 007,
// seeds a row and then opens it through the store so migration 008 runs.
// It asserts the new AUR columns and their defaults on the migrated row.
func TestMigrateAURColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v7.db")

	legacy, err := sql.Open("sqlite", path+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	for _, name := range []string{"001_init.sql", "002_build_hash_ids.sql", "003_drop_task_worker_fk.sql", "004_package_metadata.sql", "005_drop_package_enabled.sql", "006_package_metadata.sql", "007_package_epoch.sql"} {
		script, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if _, err := legacy.Exec(string(script)); err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
	}
	if _, err := legacy.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
		INSERT INTO schema_migrations (version, applied_at) VALUES (1, 'x'), (2, 'x'), (3, 'x'), (4, 'x'), (5, 'x'), (6, 'x'), (7, 'x')`); err != nil {
		t.Fatalf("seed migration ledger: %v", err)
	}
	// Legacy maintainers column: a plain string list of emails.
	if _, err := legacy.Exec(`INSERT INTO packages
		(pkgbase, branch, vcs_kind, arch, current_version, pkgdesc, last_srcinfo_hash, last_upstream_ref, last_build_id, maintainers)
		VALUES ('legacy', 'main', '', 'x86_64', '', '', '', '', NULL, '["alice@example.org","bob@example.org"]')`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open after migration: %v", err)
	}
	defer s.Close()

	pkg, err := s.GetPackageByBase(testCtx, "legacy")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	// AUR columns fall back to their defaults on migrated rows.
	if pkg.AURName != "" || pkg.AURSubmit || pkg.LastAURPushAt != nil || pkg.LastAURCommit != "" || pkg.LastAURError != "" {
		t.Errorf("migrated AUR fields = %+v, want empty defaults", pkg)
	}
	// The legacy string-list maintainers decode as email-only entries.
	if len(pkg.Maintainers) != 2 || pkg.Maintainers[0] != (Maintainer{Email: "alice@example.org"}) || pkg.Maintainers[1] != (Maintainer{Email: "bob@example.org"}) {
		t.Errorf("legacy maintainers = %+v, want email-only entries", pkg.Maintainers)
	}
}

// TestRecordAURPush asserts the push outcome round trip: a success records
// the commit and an empty error, a failure records the error text, and the
// attemptedAt timestamp parses back exactly.
func TestRecordAURPush(t *testing.T) {
	s := newTestStore(t)
	_ = mustSeedPackage(t, s, "aur-pkg")

	when := at(42 * time.Second)
	if err := s.RecordAURPush(testCtx, "aur-pkg", "deadbeef", when, ""); err != nil {
		t.Fatalf("RecordAURPush success: %v", err)
	}
	got, err := s.GetPackageByBase(testCtx, "aur-pkg")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if got.LastAURCommit != "deadbeef" || got.LastAURError != "" {
		t.Errorf("success record = commit %q error %q", got.LastAURCommit, got.LastAURError)
	}
	if got.LastAURPushAt == nil || !got.LastAURPushAt.Equal(when) {
		t.Errorf("LastAURPushAt = %v, want %v", got.LastAURPushAt, when)
	}

	when2 := at(43 * time.Second)
	if err := s.RecordAURPush(testCtx, "aur-pkg", "c0ffee", when2, "git push rejected"); err != nil {
		t.Fatalf("RecordAURPush failure: %v", err)
	}
	got, err = s.GetPackageByBase(testCtx, "aur-pkg")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if got.LastAURCommit != "c0ffee" || got.LastAURError != "git push rejected" || got.LastAURPushAt == nil || !got.LastAURPushAt.Equal(when2) {
		t.Errorf("failure record = %+v", got)
	}

	if err := s.RecordAURPush(testCtx, "missing", "x", when, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("RecordAURPush for missing package = %v, want ErrNotFound", err)
	}
}

// TestUpsertPackageAUR asserts the AUR fields are refreshed by the enqueue
// upsert path: an existing row picks up the new name and submit flag.
func TestUpsertPackageAUR(t *testing.T) {
	s := newTestStore(t)
	p := Package{Pkgbase: "aur-pkg", Branch: "foo", VCSKind: "git", Arch: "x86_64", AURName: "aur-name", AURSubmit: true}
	if err := s.UpsertPackage(testCtx, &p); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	again := Package{Pkgbase: "aur-pkg", Branch: "bar", VCSKind: "", Arch: "aarch64", AURName: "renamed", AURSubmit: false}
	if err := s.UpsertPackage(testCtx, &again); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	got, err := s.GetPackageByBase(testCtx, "aur-pkg")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if got.AURName != "renamed" || got.AURSubmit {
		t.Errorf("AUR fields = %+v, want renamed with submit off", got)
	}
}

// TestMaintainerEmails asserts the notification address projection skips
// entries without an email.
func TestMaintainerEmails(t *testing.T) {
	got := MaintainerEmails([]Maintainer{
		{Name: "A", Email: "a@example.org"},
		{Name: "NoAddress"},
		{Email: "b@example.org"},
	})
	want := []string{"a@example.org", "b@example.org"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("MaintainerEmails = %v, want %v", got, want)
	}
	if emails := MaintainerEmails(nil); len(emails) != 0 {
		t.Errorf("MaintainerEmails(nil) = %v, want empty", emails)
	}
}
