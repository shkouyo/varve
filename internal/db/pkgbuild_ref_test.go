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

import "testing"

// TestMigratePkgbuildRef asserts migration 009 adds the pkgbuild_ref
// column to both packages and builds with an empty default, so a branch
// without a pkgbuild_source record keeps the empty value.
func TestMigratePkgbuildRef(t *testing.T) {
	s := newFileTestStore(t)
	for _, tbl := range []string{"packages", "builds"} {
		var cols int
		if err := s.read.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('` + tbl + `') WHERE name = 'pkgbuild_ref'`).Scan(&cols); err != nil {
			t.Fatalf("check pkgbuild_ref on %s: %v", tbl, err)
		}
		if cols != 1 {
			t.Errorf("pkgbuild_ref column missing on %s after migration", tbl)
		}
	}
}

// TestBuildPkgbuildRefRoundTrip asserts the dispatched external head rides
// the build row: CreateTask stores it and GetBuild / LatestBuildForPackage
// decode it back.
func TestBuildPkgbuildRefRoundTrip(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "pb")
	task, build := newTaskBuild("pb-task", "queued", pkg, at(0))
	build.PkgbuildRef = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := s.CreateTask(testCtx, task, build); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got, err := s.GetBuild(testCtx, task.BuildID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if got.PkgbuildRef != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("build pkgbuild_ref = %q, want the dispatched external head", got.PkgbuildRef)
	}
	latest, err := s.LatestBuildForPackage(testCtx, pkg.ID)
	if err != nil {
		t.Fatalf("LatestBuildForPackage: %v", err)
	}
	if latest.PkgbuildRef != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("latest build pkgbuild_ref = %q, want the dispatched external head", latest.PkgbuildRef)
	}
}

// TestUpdatePackageAfterBuildPkgbuildRef asserts the success path copies
// the built external head onto the package row, and that the plain
// upsert path never touches it (it only advances on success).
func TestUpdatePackageAfterBuildPkgbuildRef(t *testing.T) {
	s := newTestStore(t)
	mustSeedPackage(t, s, "pb-upd")

	if err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.UpdatePackageAfterBuild(testCtx, "pb-upd", PackageUpdate{
			CurrentVersion: "1.0-1", Pkgdesc: "d", SrcinfoHash: "h",
			UpstreamRef: "r", PkgbuildRef: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			BuildID: "0000000000000001", Commit: "deadbeef",
		})
	}); err != nil {
		t.Fatalf("UpdatePackageAfterBuild: %v", err)
	}
	got, err := s.GetPackageByBase(testCtx, "pb-upd")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if got.PkgbuildRef != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("package pkgbuild_ref = %q, want the built external head", got.PkgbuildRef)
	}
	if got.LastCommit != "deadbeef" {
		t.Errorf("last_commit = %q, want the branch commit deadbeef", got.LastCommit)
	}

	// UpsertPackage refreshes detection metadata but must leave the
	// success-only pkgbuild_ref record alone.
	p := Package{
		Pkgbase: "pb-upd", Branch: "pb-upd", VCSKind: "", Arch: "x86_64",
		Maintainers: []Maintainer{{Email: "a@example.org"}},
	}
	if err := s.UpsertPackage(testCtx, &p); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	got, err = s.GetPackageByBase(testCtx, "pb-upd")
	if err != nil {
		t.Fatalf("GetPackageByBase after upsert: %v", err)
	}
	if got.PkgbuildRef != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("upsert clobbered pkgbuild_ref = %q, want it preserved", got.PkgbuildRef)
	}
}
