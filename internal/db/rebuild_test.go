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
	"reflect"
	"testing"
	"time"
)

// seededMetaPackage inserts a package row carrying the full detection
// metadata a live pipeline would have recorded, one succeeded build, a
// failed task that stamped the last_failed_at cooldown marker and the AUR
// push record.
func seededMetaPackage(t *testing.T, s *Store, pkgbase string) (Package, *Build) {
	t.Helper()
	pkg := seedPackage(t, s, Package{
		Pkgbase:    pkgbase,
		Branch:     "main",
		VCSKind:    "git",
		Arch:       "x86_64",
		Pkgdesc:    "meta desc",
		URL:        "https://example.org/" + pkgbase,
		Licenses:   []string{"MIT"},
		Conflicts:  []string{pkgbase + "-legacy"},
		Provides:   []string{pkgbase + "-lib"},
		Pkgname:    []string{pkgbase},
		Source:     []string{"https://example.org/" + pkgbase + ".tar.gz"},
		Pkgver:     "1.0",
		Pkgrel:     "2",
		Epoch:      1,
		LastCommit: "old-c",
		Maintainers: []Maintainer{
			{Name: "Alice", Email: "alice@example.org"},
		},
	})
	// seedPackage does not write the full metadata column set; fill the
	// remaining detection columns the live upsert path would carry.
	if _, err := s.write.Exec(`UPDATE packages SET
		url = ?, licenses = ?, conflicts = ?, provides = ?, epoch = ?, pkgbuild_ref = ?, last_failed_at = ?,
		aur_name = ?, aur_submit = ?, last_aur_push_at = ?, last_aur_commit = ?, last_aur_error = ?
		WHERE pkgbase = ?`,
		pkg.URL, encodeStrings(pkg.Licenses), encodeStrings(pkg.Conflicts), encodeStrings(pkg.Provides),
		pkg.Epoch, "ext-ref", nil, "", false, nil, "", "", pkgbase); err != nil {
		t.Fatalf("fill metadata for %s: %v", pkgbase, err)
	}
	task, build := createTask(t, s, "meta-"+pkgbase, "running", pkg, at(0))
	if err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeTask(testCtx, task.ID, "succeeded", "", at(time.Minute),
			[]Artifact{{File: pkgbase + "-1.0-2-x86_64.pkg.tar.zst", Kind: "package", Pkgname: pkgbase, Version: "1.0-2", Arch: "x86_64"}},
			nil)
	}); err != nil {
		t.Fatalf("finalize %s: %v", pkgbase, err)
	}
	if err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.UpdatePackageAfterBuild(testCtx, pkgbase, PackageUpdate{
			CurrentVersion: "1.0-2", Pkgdesc: "meta desc", SrcinfoHash: "new-h", UpstreamRef: "new-ref", BuildID: build.ID,
			URL: "https://example.org/" + pkgbase, Licenses: []string{"MIT"}, Conflicts: []string{pkgbase + "-legacy"},
			Provides: []string{pkgbase + "-lib"}, Pkgname: []string{pkgbase},
			Source: []string{"https://example.org/" + pkgbase + ".tar.gz"},
			Pkgver: "1.0", Pkgrel: "2", Epoch: 1, PkgbuildRef: "ext-ref",
			Commit: "new-c",
		})
	}); err != nil {
		t.Fatalf("update %s after build: %v", pkgbase, err)
	}
	// A failed task stamps the last_failed_at rebuild-cooldown marker.
	failedAt := at(2 * time.Minute)
	failedTask, _ := createTask(t, s, "meta-"+pkgbase+"-2", "running", pkg, at(30*time.Second))
	if err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeFailed(testCtx, failedTask.ID, "test: boom", failedAt, nil, nil)
	}); err != nil {
		t.Fatalf("finalize failed: %v", err)
	}
	// The AUR publish record mirrors what publishAUR leaves behind.
	if err := s.RecordAURPush(testCtx, pkgbase, "c0ffee", at(3*time.Minute), "git push rejected"); err != nil {
		t.Fatalf("RecordAURPush: %v", err)
	}
	got, err := s.GetPackageByBase(testCtx, pkgbase)
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if got.LastFailedAt == nil || !got.LastFailedAt.Equal(failedAt) {
		t.Fatalf("seeded cooldown marker = %v, want %v", got.LastFailedAt, failedAt)
	}
	pkg = *got
	return pkg, build
}

// encodeStrings marshals a string slice for a JSON column in test seeds.
func encodeStrings(v []string) string {
	b, err := encodeJSON(v)
	if err != nil {
		panic(err)
	}
	return b
}

// recordFromPackage mirrors what cmd/varve's toRebuildPackage produces:
// every detection metadata column preserved from the row being replaced,
// the build records from the side file.
func recordFromPackage(p Package) RebuildPackage {
	return RebuildPackage{
		Pkgbase:         p.Pkgbase,
		Branch:          p.Branch,
		VCSKind:         p.VCSKind,
		Arch:            p.Arch,
		CurrentVersion:  p.CurrentVersion,
		Pkgdesc:         p.Pkgdesc,
		URL:             p.URL,
		Licenses:        p.Licenses,
		Conflicts:       p.Conflicts,
		Provides:        p.Provides,
		Pkgname:         p.Pkgname,
		Source:          p.Source,
		Pkgver:          p.Pkgver,
		Pkgrel:          p.Pkgrel,
		Epoch:           p.Epoch,
		PkgbuildRef:     p.PkgbuildRef,
		LastFailedAt:    p.LastFailedAt,
		Maintainers:     p.Maintainers,
		AURName:         p.AURName,
		AURSubmit:       p.AURSubmit,
		LastAURPushAt:   p.LastAURPushAt,
		LastAURCommit:   p.LastAURCommit,
		LastAURError:    p.LastAURError,
		LastSrcinfoHash: p.LastSrcinfoHash,
		LastUpstreamRef: p.LastUpstreamRef,
		Commit:          p.LastCommit,
		UpstreamRef:     p.LastUpstreamRef,
		SrcinfoHash:     p.LastSrcinfoHash,
		BuiltAt:         at(time.Hour),
		Artifacts: []Artifact{
			{File: p.Pkgbase + "-1.0-2-x86_64.pkg.tar.zst", Kind: "package", Pkgname: p.Pkgbase, Version: "1.0-2", Arch: "x86_64"},
		},
	}
}

// TestRebuildIndexPreservesMetadata asserts every detection metadata
// column survives RebuildIndex when the record carries it: the rebuilt
// row keeps url/licenses/conflicts/provides/pkgname/source/pkgver/pkgrel/
// epoch/pkgbuild_ref, the last_failed_at cooldown marker, the AUR record
// and the maintained pkgdesc/maintainers instead of falling back to
// defaults.
func TestRebuildIndexPreservesMetadata(t *testing.T) {
	s := newTestStore(t)
	pkg, build := seededMetaPackage(t, s, "meta-keep")

	removed, err := s.RebuildIndex(testCtx, []RebuildPackage{recordFromPackage(pkg)})
	if err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	// Both seeded builds (the succeeded one and the failed one) are
	// replaced by the single fresh build of the record.
	if len(removed) != 2 || (removed[0] != build.ID && removed[1] != build.ID) {
		t.Errorf("removed build ids = %v, want the two seeded builds including %s", removed, build.ID)
	}

	got, err := s.GetPackageByBase(testCtx, "meta-keep")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if got.URL != "https://example.org/meta-keep" ||
		!reflect.DeepEqual(got.Licenses, []string{"MIT"}) ||
		!reflect.DeepEqual(got.Conflicts, []string{"meta-keep-legacy"}) ||
		!reflect.DeepEqual(got.Provides, []string{"meta-keep-lib"}) {
		t.Errorf("url/licenses/conflicts/provides = %q/%v/%v/%v, want preserved",
			got.URL, got.Licenses, got.Conflicts, got.Provides)
	}
	if !reflect.DeepEqual(got.Pkgname, []string{"meta-keep"}) ||
		!reflect.DeepEqual(got.Source, []string{"https://example.org/meta-keep.tar.gz"}) ||
		got.Pkgver != "1.0" || got.Pkgrel != "2" || got.Epoch != 1 {
		t.Errorf("pkgname/source/pkgver/pkgrel/epoch = %v/%v/%q/%q/%d, want preserved",
			got.Pkgname, got.Source, got.Pkgver, got.Pkgrel, got.Epoch)
	}
	if got.LastFailedAt == nil || !got.LastFailedAt.Equal(at(2*time.Minute)) {
		t.Errorf("last_failed_at = %v, want preserved cooldown marker %v", got.LastFailedAt, at(2*time.Minute))
	}
	if got.LastAURCommit != "c0ffee" || got.LastAURError != "git push rejected" ||
		got.LastAURPushAt == nil || !got.LastAURPushAt.Equal(at(3*time.Minute)) {
		t.Errorf("aur record = %+v, want preserved c0ffee push", got)
	}
	if got.CurrentVersion != "1.0-2" || got.Pkgdesc != "meta desc" {
		t.Errorf("current_version/pkgdesc = %q/%q, want 1.0-2/meta desc", got.CurrentVersion, got.Pkgdesc)
	}
	if got.LastCommit != "new-c" || got.LastSrcinfoHash != "new-h" || got.LastUpstreamRef != "new-ref" {
		t.Errorf("commit records = %q/%q/%q, want new-c/new-h/new-ref", got.LastCommit, got.LastSrcinfoHash, got.LastUpstreamRef)
	}
	if len(got.Maintainers) != 1 || got.Maintainers[0].Name != "Alice" {
		t.Errorf("maintainers = %+v, want preserved [Alice]", got.Maintainers)
	}
	builds, total, err := s.ListBuilds(testCtx, 1, 10, false)
	if err != nil {
		t.Fatalf("ListBuilds: %v", err)
	}
	if total != 1 || builds[0].ID == build.ID {
		t.Errorf("builds after rebuild = %d rows (id %s), want one fresh build replacing %s", total, builds[0].ID, build.ID)
	}
}

// TestRebuildIndexClearsEmptyMetadata asserts the empty side of the
// matrix: a record that carries no AUR state clears the AUR columns and a
// nil cooldown marker clears last_failed_at (the record is authoritative).
func TestRebuildIndexClearsEmptyMetadata(t *testing.T) {
	s := newTestStore(t)
	pkg, _ := seededMetaPackage(t, s, "meta-clear")
	rec := recordFromPackage(pkg)
	rec.AURName = ""
	rec.AURSubmit = false
	rec.LastAURPushAt = nil
	rec.LastAURCommit = ""
	rec.LastAURError = ""
	rec.LastFailedAt = nil
	if _, err := s.RebuildIndex(testCtx, []RebuildPackage{rec}); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	got, err := s.GetPackageByBase(testCtx, "meta-clear")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if got.LastFailedAt != nil {
		t.Errorf("last_failed_at = %v, want nil (record carried nil)", got.LastFailedAt)
	}
	if got.LastAURPushAt != nil || got.LastAURCommit != "" || got.LastAURError != "" || got.AURSubmit {
		t.Errorf("aur record = %+v, want cleared (record carried empty)", got)
	}
	if got.Pkgver != "1.0" || !reflect.DeepEqual(got.Pkgname, []string{"meta-clear"}) {
		t.Errorf("pkgver/pkgname = %q/%v, want still preserved", got.Pkgver, got.Pkgname)
	}
}

// TestRebuildIndexRemovedIDs covers the returned id list on both ends of
// the spectrum: an empty record list clears every build and reports every
// removed id; a rebuild of an already-empty index removes nothing.
func TestRebuildIndexRemovedIDs(t *testing.T) {
	s := newTestStore(t)
	_, b1 := seededMetaPackage(t, s, "gone-a")
	_, b2 := seededMetaPackage(t, s, "gone-b")

	removed, err := s.RebuildIndex(testCtx, nil)
	if err != nil {
		t.Fatalf("RebuildIndex(nil): %v", err)
	}
	// Every seeded build (each package contributes a succeeded and a
	// failed build) is reported.
	if len(removed) != 4 {
		t.Fatalf("removed ids = %v, want the four seeded build ids", removed)
	}
	seen := map[string]bool{}
	for _, id := range removed {
		seen[id] = true
	}
	if !seen[b1.ID] || !seen[b2.ID] {
		t.Errorf("removed ids = %v, want %s and %s included", removed, b1.ID, b2.ID)
	}
	// The index is now empty; a second rebuild removes nothing.
	removed, err = s.RebuildIndex(testCtx, nil)
	if err != nil {
		t.Fatalf("second RebuildIndex: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("second rebuild removed = %v, want none", removed)
	}
}

// TestRebuildIndexDedupeAndValidation covers the record-level guards: an
// empty pkgbase and a duplicate pkgbase are rejected before any write.
func TestRebuildIndexDedupeAndValidation(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RebuildIndex(testCtx, []RebuildPackage{{}}); err == nil {
		t.Error("RebuildIndex with empty pkgbase accepted")
	}
	if _, err := s.RebuildIndex(testCtx, []RebuildPackage{
		{Pkgbase: "dup", Branch: "main", Arch: "x86_64"},
		{Pkgbase: "dup", Branch: "main", Arch: "x86_64"},
	}); err == nil {
		t.Error("RebuildIndex with duplicate pkgbase accepted")
	}
}
