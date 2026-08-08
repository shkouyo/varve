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
	"errors"
	"reflect"
	"testing"
	"time"
)

// TestGetPackageByBase covers hit, miss and decoded maintainers.
func TestGetPackageByBase(t *testing.T) {
	s := newTestStore(t)
	seedPackage(t, s, Package{
		Pkgbase:         "foo",
		Branch:          "foo",
		VCSKind:         "git",
		Arch:            "x86_64",
		CurrentVersion:  "1.2.3-1",
		Pkgdesc:         "a foo package",
		LastSrcinfoHash: "abc",
		LastUpstreamRef: "refs/heads/main",
		Maintainers:     []Maintainer{{Email: "alice@example.com"}, {Email: "bob@example.com"}},
	})

	got, err := s.GetPackageByBase(testCtx, "foo")
	if err != nil {
		t.Fatalf("GetPackageByBase(foo): %v", err)
	}
	if got.Pkgbase != "foo" || got.Branch != "foo" || got.VCSKind != "git" || got.Arch != "x86_64" {
		t.Errorf("scalar fields mismatch: %+v", got)
	}
	if got.CurrentVersion != "1.2.3-1" || got.Pkgdesc != "a foo package" {
		t.Errorf("version/desc mismatch: %+v", got)
	}
	if got.LastSrcinfoHash != "abc" || got.LastUpstreamRef != "refs/heads/main" {
		t.Errorf("hash/ref mismatch: %+v", got)
	}
	if len(got.Maintainers) != 2 || got.Maintainers[0].Email != "alice@example.com" || got.Maintainers[1].Email != "bob@example.com" {
		t.Errorf("Maintainers = %v", got.Maintainers)
	}
	if got.ID == 0 {
		t.Error("ID = 0, want autoincrement id")
	}

	if _, err := s.GetPackageByBase(testCtx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetPackageByBase(missing) = %v, want ErrNotFound", err)
	}
}

// TestListPackages covers pagination totals, page bounds, and substring
// search on pkgbase and pkgdesc.
func TestListPackages(t *testing.T) {
	s := newTestStore(t)
	pkgs := []Package{
		{Pkgbase: "alpha", Branch: "main", Arch: "x86_64", Pkgdesc: "letters first"},
		{Pkgbase: "beta", Branch: "main", Arch: "x86_64", Pkgdesc: "letters second"},
		{Pkgbase: "gamma", Branch: "main", Arch: "x86_64", Pkgdesc: "greek letter"},
		{Pkgbase: "delta", Branch: "main", Arch: "x86_64", Pkgdesc: "river delta"},
	}
	for _, p := range pkgs {
		seedPackage(t, s, p)
	}

	t.Run("full list", func(t *testing.T) {
		rows, total, err := s.ListPackages(testCtx, "", 1, 100)
		if err != nil {
			t.Fatalf("ListPackages: %v", err)
		}
		if total != 4 || len(rows) != 4 {
			t.Errorf("rows=%d total=%d, want 4/4", len(rows), total)
		}
		for i, got := range rows {
			want := []string{"alpha", "beta", "delta", "gamma"}[i]
			if got.Pkgbase != want {
				t.Errorf("rows[%d].Pkgbase = %q, want %q (order by pkgbase)", i, got.Pkgbase, want)
			}
		}
	})

	t.Run("pagination", func(t *testing.T) {
		page1, total, err := s.ListPackages(testCtx, "", 1, 2)
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		if total != 4 || len(page1) != 2 || page1[0].Pkgbase != "alpha" || page1[1].Pkgbase != "beta" {
			t.Errorf("page 1 = %v (total %d)", page1, total)
		}
		page2, _, err := s.ListPackages(testCtx, "", 2, 2)
		if err != nil {
			t.Fatalf("page 2: %v", err)
		}
		if len(page2) != 2 || page2[0].Pkgbase != "delta" || page2[1].Pkgbase != "gamma" {
			t.Errorf("page 2 = %v", page2)
		}
		page3, _, err := s.ListPackages(testCtx, "", 3, 2)
		if err != nil {
			t.Fatalf("page 3: %v", err)
		}
		if len(page3) != 0 {
			t.Errorf("page 3 = %v, want empty", page3)
		}
	})

	t.Run("boundary page and perPage", func(t *testing.T) {
		rows, total, err := s.ListPackages(testCtx, "", 0, 0) // clamped to 1/1
		if err != nil {
			t.Fatalf("ListPackages(0,0): %v", err)
		}
		if total != 4 || len(rows) != 1 || rows[0].Pkgbase != "alpha" {
			t.Errorf("clamped call = %v (total %d)", rows, total)
		}
	})

	t.Run("search pkgbase substring", func(t *testing.T) {
		rows, total, err := s.ListPackages(testCtx, "gam", 1, 100)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if total != 1 || len(rows) != 1 || rows[0].Pkgbase != "gamma" {
			t.Errorf("rows=%v total=%d, want gamma/1", rows, total)
		}
	})

	t.Run("search pkgdesc substring", func(t *testing.T) {
		rows, total, err := s.ListPackages(testCtx, "second", 1, 100)
		if err != nil {
			t.Fatalf("search desc: %v", err)
		}
		if total != 1 || len(rows) != 1 || rows[0].Pkgbase != "beta" {
			t.Errorf("rows=%v total=%d, want beta/1", rows, total)
		}
	})

	t.Run("search across both columns", func(t *testing.T) {
		rows, total, err := s.ListPackages(testCtx, "et", 1, 100)
		if err != nil {
			t.Fatalf("search both: %v", err)
		}
		// "et" appears in pkgbase (beta, delta) and pkgdesc (letters, letter).
		if total != 3 || len(rows) != 3 {
			t.Errorf("rows=%d total=%d, want 3/3", len(rows), total)
		}
	})

	t.Run("search greek desc", func(t *testing.T) {
		rows, total, err := s.ListPackages(testCtx, "greek", 1, 100)
		if err != nil {
			t.Fatalf("search desc: %v", err)
		}
		if total != 1 || len(rows) != 1 || rows[0].Pkgbase != "gamma" {
			t.Errorf("rows=%v total=%d, want gamma/1", rows, total)
		}
	})

	t.Run("search no match", func(t *testing.T) {
		rows, total, err := s.ListPackages(testCtx, "zzz", 1, 100)
		if err != nil {
			t.Fatalf("search miss: %v", err)
		}
		if total != 0 || len(rows) != 0 {
			t.Errorf("rows=%d total=%d, want 0/0", len(rows), total)
		}
	})
}

// TestListPackagesLikeEscape asserts that LIKE metacharacters in the
// search term match literally instead of acting as wildcards.
func TestListPackagesLikeEscape(t *testing.T) {
	s := newTestStore(t)
	seedPackage(t, s, Package{Pkgbase: "lib-100%", Branch: "main", Arch: "x86_64", Pkgdesc: "percent package"})
	seedPackage(t, s, Package{Pkgbase: "lib-100x", Branch: "main", Arch: "x86_64", Pkgdesc: "plain package"})

	rows, total, err := s.ListPackages(testCtx, "100%", 1, 100)
	if err != nil {
		t.Fatalf("search literal percent: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].Pkgbase != "lib-100%" {
		t.Errorf("rows=%v total=%d, want only lib-100%% (literal %% match)", rows, total)
	}

	// A bare underscore matches only a literal underscore, not any char.
	seedPackage(t, s, Package{Pkgbase: "lib_1", Branch: "main", Arch: "x86_64"})
	seedPackage(t, s, Package{Pkgbase: "libx1", Branch: "main", Arch: "x86_64"})
	rows, total, err = s.ListPackages(testCtx, "lib_1", 1, 100)
	if err != nil {
		t.Fatalf("search literal underscore: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].Pkgbase != "lib_1" {
		t.Errorf("rows=%v total=%d, want only lib_1 (literal _ match)", rows, total)
	}
}

// TestUpdatePackageAfterBuild asserts the update semantics inside WithTx.
func TestUpdatePackageAfterBuild(t *testing.T) {
	s := newTestStore(t)
	mustSeedPackage(t, s, "upd")

	upd := PackageUpdate{
		CurrentVersion: "2:2.0.0-1", Pkgdesc: "new desc", SrcinfoHash: "hash2", UpstreamRef: "ref2",
		BuildID: "000000000000002a", URL: "https://example.org/upd",
		Licenses: []string{"GPL", "MIT"}, Conflicts: []string{"oldpkg"}, Provides: []string{"upd-lib"},
		Pkgname: []string{"upd", "upd-lib"}, Source: []string{"https://example.org/upd.tar.gz"},
		Pkgver: "2.0.0", Pkgrel: "1", Epoch: 2, Commit: "deadbeef",
	}
	err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.UpdatePackageAfterBuild(testCtx, "upd", upd)
	})
	if err != nil {
		t.Fatalf("UpdatePackageAfterBuild: %v", err)
	}
	got, err := s.GetPackageByBase(testCtx, "upd")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if got.CurrentVersion != "2:2.0.0-1" || got.Pkgdesc != "new desc" ||
		got.LastSrcinfoHash != "hash2" || got.LastUpstreamRef != "ref2" || got.LastBuildID != "000000000000002a" {
		t.Errorf("updated fields mismatch: %+v", got)
	}
	if got.URL != "https://example.org/upd" {
		t.Errorf("URL = %q, want https://example.org/upd", got.URL)
	}
	if !reflect.DeepEqual(got.Licenses, []string{"GPL", "MIT"}) ||
		!reflect.DeepEqual(got.Conflicts, []string{"oldpkg"}) ||
		!reflect.DeepEqual(got.Provides, []string{"upd-lib"}) {
		t.Errorf("metadata = licenses %v conflicts %v provides %v, want GPL/MIT oldpkg upd-lib",
			got.Licenses, got.Conflicts, got.Provides)
	}
	if !reflect.DeepEqual(got.Pkgname, []string{"upd", "upd-lib"}) ||
		!reflect.DeepEqual(got.Source, []string{"https://example.org/upd.tar.gz"}) {
		t.Errorf("pkgname/source = %v/%v, want upd/upd-lib and the source list", got.Pkgname, got.Source)
	}
	if got.Pkgver != "2.0.0" || got.Pkgrel != "1" {
		t.Errorf("pkgver/pkgrel = %q/%q, want 2.0.0/1", got.Pkgver, got.Pkgrel)
	}
	if got.Epoch != 2 {
		t.Errorf("epoch = %d, want 2", got.Epoch)
	}
	if got.CurrentVersion != "2:2.0.0-1" {
		t.Errorf("current_version = %q, want the epoch-prefixed 2:2.0.0-1", got.CurrentVersion)
	}
	if got.LastCommit != "deadbeef" {
		t.Errorf("last_commit = %q, want deadbeef", got.LastCommit)
	}

	// Unknown pkgbase -> ErrNotFound (transaction still commits cleanly).
	err = s.WithTx(testCtx, func(tx *Tx) error {
		return tx.UpdatePackageAfterBuild(testCtx, "nope", PackageUpdate{CurrentVersion: "1", Pkgdesc: "d", SrcinfoHash: "h", UpstreamRef: "r", BuildID: "0000000000000001"})
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdatePackageAfterBuild(missing) = %v, want ErrNotFound", err)
	}
}

// TestUpdatePackageAfterBuildClearsCooldown asserts the success side of
// the rebuild cooldown: a successful build clears the package's
// last_failed_at marker.
func TestUpdatePackageAfterBuildClearsCooldown(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "rec")
	createTask(t, s, "rec-1", "assigned", pkg, at(0))
	if err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeFailed(testCtx, "rec-1", "boom", at(time.Minute), nil, nil)
	}); err != nil {
		t.Fatalf("FinalizeFailed: %v", err)
	}
	if err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.UpdatePackageAfterBuild(testCtx, "rec", PackageUpdate{CurrentVersion: "1.0-1", Pkgdesc: "d", SrcinfoHash: "hash", UpstreamRef: "ref", BuildID: "000000000000002a"})
	}); err != nil {
		t.Fatalf("UpdatePackageAfterBuild: %v", err)
	}
	got, err := s.GetPackageByBase(testCtx, "rec")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if got.LastFailedAt != nil {
		t.Errorf("last_failed_at = %v, want nil after a successful build", got.LastFailedAt)
	}
}

// TestGetPackageByBaseCorruptMaintainers guards the no-panic contract.
func TestGetPackageByBaseCorruptMaintainers(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "corrupt")
	if _, err := s.write.Exec(`UPDATE packages SET maintainers = '{"not a list' WHERE id = ?`, pkg.ID); err != nil {
		t.Fatalf("corrupt maintainers: %v", err)
	}
	_, err := s.GetPackageByBase(testCtx, "corrupt")
	if err == nil {
		t.Fatal("GetPackageByBase on corrupt JSON: want error, got nil")
	}
}

// TestGetPackageByBaseCorruptMetadata guards the same no-panic contract
// for the .SRCINFO metadata JSON columns.
func TestGetPackageByBaseCorruptMetadata(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "corrupt-meta")
	for _, col := range []string{"licenses", "conflicts", "provides", "pkgname", "source"} {
		if _, err := s.write.Exec(`UPDATE packages SET `+col+` = 'not json' WHERE id = ?`, pkg.ID); err != nil {
			t.Fatalf("corrupt %s: %v", col, err)
		}
		if _, err := s.GetPackageByBase(testCtx, "corrupt-meta"); err == nil {
			t.Fatalf("GetPackageByBase with corrupt %s: want error, got nil", col)
		}
	}
}

// TestUpsertPackageMetadata asserts the metadata survives the upsert
// round trip and is refreshed on conflict.
func TestUpsertPackageMetadata(t *testing.T) {
	s := newTestStore(t)
	p := &Package{
		Pkgbase: "meta", Branch: "main", Arch: "x86_64",
		URL: "https://example.org/meta", Licenses: []string{"MIT"}, Provides: []string{"meta-lib"},
		Pkgname: []string{"meta"}, Source: []string{"https://example.org/meta.tar.gz"}, Pkgver: "1.0", Pkgrel: "1", Epoch: 3,
	}
	if err := s.UpsertPackage(testCtx, p); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	got, err := s.GetPackageByBase(testCtx, "meta")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if got.URL != "https://example.org/meta" || !reflect.DeepEqual(got.Licenses, []string{"MIT"}) ||
		!reflect.DeepEqual(got.Provides, []string{"meta-lib"}) || len(got.Conflicts) != 0 {
		t.Errorf("metadata round trip = url %q licenses %v provides %v conflicts %v", got.URL, got.Licenses, got.Provides, got.Conflicts)
	}
	if !reflect.DeepEqual(got.Pkgname, []string{"meta"}) ||
		!reflect.DeepEqual(got.Source, []string{"https://example.org/meta.tar.gz"}) ||
		got.Pkgver != "1.0" || got.Pkgrel != "1" || got.Epoch != 3 {
		t.Errorf("pkgname/source/pkgver/pkgrel/epoch round trip = %v/%v/%q/%q/%d", got.Pkgname, got.Source, got.Pkgver, got.Pkgrel, got.Epoch)
	}

	// An upsert on the same pkgbase refreshes the metadata.
	p2 := &Package{Pkgbase: "meta", Branch: "main", Arch: "x86_64", URL: "https://new.example.org/meta"}
	if err := s.UpsertPackage(testCtx, p2); err != nil {
		t.Fatalf("UpsertPackage refresh: %v", err)
	}
	got, err = s.GetPackageByBase(testCtx, "meta")
	if err != nil {
		t.Fatalf("GetPackageByBase after refresh: %v", err)
	}
	if got.URL != "https://new.example.org/meta" || len(got.Licenses) != 0 || len(got.Provides) != 0 {
		t.Errorf("refreshed metadata = url %q licenses %v provides %v, want empty refresh", got.URL, got.Licenses, got.Provides)
	}
}
