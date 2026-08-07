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

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/storage"
)

// TestRebuildIndex exercises the rebuild semantics: a side file set
// including a corrupt file is turned into the authoritative
// packages/builds state, the task queue is cleared, workers survive, and
// the report counts every outcome. The real local storage and the real
// database both run under t.TempDir().
func TestRebuildIndex(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store, err := db.Open(filepath.Join(dir, "varve.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	backend, err := storage.OpenLocal(filepath.Join(dir, "repo"))
	if err != nil {
		t.Fatal(err)
	}

	// Seed state that the rebuild must preserve or clear:
	//   - worker "w1" must survive untouched;
	//   - package "keep" carries pkgdesc/maintainers the side file does
	//     not contain (preserved) and its build record is replaced;
	//   - package "stale" has a queued task and no side file (deleted).
	if err := store.RegisterWorker(ctx, &db.Worker{Name: "w1", Role: "host", Mode: "host", Arch: "x86_64", Capacity: 1}); err != nil {
		t.Fatal(err)
	}
	keep := &db.Package{Pkgbase: "keep", Branch: "master", VCSKind: "git", Arch: "x86_64", Maintainers: []string{"m@example.org"}}
	if err := store.UpsertPackage(ctx, keep); err != nil {
		t.Fatal(err)
	}
	seedTask := &db.Task{ID: "seed-keep", PackageID: keep.ID, State: "running"}
	if err := store.CreateTask(ctx, seedTask, &db.Build{Branch: "master", Commit: "old-c", SrcinfoHash: "old-h"}); err != nil {
		t.Fatal(err)
	}
	err = store.WithTx(ctx, func(tx *db.Tx) error {
		if err := tx.FinalizeTask(ctx, seedTask.ID, "succeeded", "", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), nil, nil); err != nil {
			return err
		}
		return tx.UpdatePackageAfterBuild(ctx, "keep", "1.0-1", "old desc", "old-h", "old-ref", seedTask.BuildID)
	})
	if err != nil {
		t.Fatal(err)
	}
	stale := &db.Package{Pkgbase: "stale", Branch: "master"}
	if err := store.UpsertPackage(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(ctx, &db.Task{ID: "seed-stale", PackageID: stale.ID, State: "queued"},
		&db.Build{Branch: "master", Commit: "c", SrcinfoHash: "h"}); err != nil {
		t.Fatal(err)
	}

	// Side files: "keep" refreshes an existing package, "newpkg" is new,
	// "broken" fails to parse and must be skipped with a warning, and
	// "stale" has no side file at all.
	repoRoot := filepath.Join(dir, "repo")
	built := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	writeSidecar(t, filepath.Join(repoRoot, "keep.meta.toml"), sidecarTOML{
		pkgbase: "keep",
		branch:  "master",
		vcs:     "git",
		artifacts: []sidecarArtifact{
			{file: "keep-2.0-1-x86_64.pkg.tar.zst", kind: "package", pkgname: "keep", version: "2.0-1", arch: "x86_64", size: 100, sha256: "aa"},
			{file: "keep-2.0-1-x86_64.pkg.tar.zst.sig", kind: "signature"},
		},
		build: sidecarBuild{commit: "c2", upstreamRef: "ref2", srcinfoHash: "h2", time: built, worker: "w1"},
	})
	writeSidecar(t, filepath.Join(repoRoot, "newpkg.meta.toml"), sidecarTOML{
		pkgbase: "newpkg",
		branch:  "next",
		artifacts: []sidecarArtifact{
			{file: "newpkg-1.0-1-x86_64.pkg.tar.zst", kind: "package", pkgname: "newpkg", version: "1.0-1", arch: "x86_64", size: 5, sha256: "bb"},
		},
		build: sidecarBuild{commit: "c3", srcinfoHash: "h3", time: built.Add(time.Hour), worker: "ghost"},
	})
	if err := os.WriteFile(filepath.Join(repoRoot, "broken.meta.toml"), []byte("pkgbase = ["), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := rebuildIndex(ctx, store, backend)
	if err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}
	if report.Added != 1 || report.Updated != 1 || report.Deleted != 1 || report.Skipped != 1 {
		t.Fatalf("report = %+v, want {Added:1 Updated:1 Deleted:1 Skipped:1}", report)
	}

	// packages: "keep" refreshed with the side file state plus the
	// preserved pkgdesc/maintainers; "newpkg" created; "stale" removed.
	keep2, err := store.GetPackageByBase(ctx, "keep")
	if err != nil {
		t.Fatal(err)
	}
	if keep2.Branch != "master" || keep2.VCSKind != "git" || keep2.Arch != "x86_64" {
		t.Errorf("keep = %+v", keep2)
	}
	if keep2.CurrentVersion != "2.0-1" {
		t.Errorf("keep.current_version = %q, want %q", keep2.CurrentVersion, "2.0-1")
	}
	if keep2.Pkgdesc != "old desc" {
		t.Errorf("keep.pkgdesc = %q, want preserved %q", keep2.Pkgdesc, "old desc")
	}
	if len(keep2.Maintainers) != 1 || keep2.Maintainers[0] != "m@example.org" {
		t.Errorf("keep.maintainers = %v, want preserved [m@example.org]", keep2.Maintainers)
	}
	if keep2.LastSrcinfoHash != "h2" || keep2.LastUpstreamRef != "ref2" {
		t.Errorf("keep hashes = (%q, %q), want (h2, ref2)", keep2.LastSrcinfoHash, keep2.LastUpstreamRef)
	}

	newpkg, err := store.GetPackageByBase(ctx, "newpkg")
	if err != nil {
		t.Fatal(err)
	}
	if newpkg.VCSKind != "" || newpkg.Arch != "x86_64" || newpkg.CurrentVersion != "1.0-1" ||
		newpkg.LastSrcinfoHash != "h3" || newpkg.Pkgdesc != "" || len(newpkg.Maintainers) != 0 {
		t.Errorf("newpkg = %+v", newpkg)
	}

	if _, err := store.GetPackageByBase(ctx, "stale"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("stale package still present (err = %v)", err)
	}

	// builds: exactly one per package, status succeeded, worker resolved
	// by name from the side file, artifacts carried over.
	builds, total, err := store.ListBuilds(ctx, 1, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(builds) != 2 {
		t.Fatalf("builds = %d/%d, want 2", len(builds), total)
	}
	w1, err := store.GetWorkerByName(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	byPackage := map[int64]db.Build{}
	for _, b := range builds {
		byPackage[b.PackageID] = b
	}
	keepBuild, ok := byPackage[keep2.ID]
	if !ok {
		t.Fatalf("no build for keep; builds = %+v", builds)
	}
	if keepBuild.Status != "succeeded" || keepBuild.WorkerID != w1.ID {
		t.Errorf("keep build = %+v, want status succeeded, worker %d", keepBuild, w1.ID)
	}
	if keepBuild.Commit != "c2" || keepBuild.UpstreamRef != "ref2" || keepBuild.SrcinfoHash != "h2" {
		t.Errorf("keep build refs = (%q,%q,%q)", keepBuild.Commit, keepBuild.UpstreamRef, keepBuild.SrcinfoHash)
	}
	if keepBuild.FinishedAt == nil || !keepBuild.FinishedAt.Equal(built) {
		t.Errorf("keep build finished_at = %v, want %v", keepBuild.FinishedAt, built)
	}
	if len(keepBuild.Artifacts) != 2 || keepBuild.Artifacts[0].Kind != "package" || keepBuild.Artifacts[0].Pkgname != "keep" {
		t.Errorf("keep build artifacts = %+v", keepBuild.Artifacts)
	}
	if keepBuild.LogPath != "logs/"+keepBuild.ID+".log" {
		t.Errorf("keep build log_path = %q", keepBuild.LogPath)
	}
	if keep2.LastBuildID != keepBuild.ID {
		t.Errorf("keep.last_build_id = %q, want %q", keep2.LastBuildID, keepBuild.ID)
	}
	newBuild, ok := byPackage[newpkg.ID]
	if !ok {
		t.Fatalf("no build for newpkg; builds = %+v", builds)
	}
	if newBuild.WorkerID != 0 {
		t.Errorf("newpkg build worker_id = %d, want 0 (unknown worker \"ghost\")", newBuild.WorkerID)
	}
	if len(newBuild.Artifacts) != 1 {
		t.Errorf("newpkg build artifacts = %+v", newBuild.Artifacts)
	}

	// tasks: the queue was cleared (both seeded tasks are gone).
	if _, err := store.GetTask(ctx, "seed-keep"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("seed-keep task still present (err = %v)", err)
	}
	if _, err := store.GetTask(ctx, "seed-stale"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("seed-stale task still present (err = %v)", err)
	}
	active, err := store.ListActiveTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Errorf("active tasks after rebuild = %+v, want none", active)
	}

	// workers: untouched.
	workers, err := store.ListWorkers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 || workers[0].Name != "w1" {
		t.Errorf("workers after rebuild = %+v, want [w1]", workers)
	}
}

// TestRebuildIndexEmpty covers the empty interface: no side files must
// rebuild without error. An empty database stays empty; a populated one is
// cleared — the index must mirror the side files, so packages without a
// side file (and their tasks) are removed.
func TestRebuildIndexEmpty(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store, err := db.Open(filepath.Join(dir, "varve.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	backend, err := storage.OpenLocal(filepath.Join(dir, "repo"))
	if err != nil {
		t.Fatal(err)
	}

	// Seed a package with a queued task, then rebuild with no side files.
	pkg := &db.Package{Pkgbase: "orphan", Branch: "master"}
	if err := store.UpsertPackage(ctx, pkg); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(ctx, &db.Task{ID: "orphan-1", PackageID: pkg.ID, State: "queued"},
		&db.Build{Branch: "master", Commit: "c", SrcinfoHash: "h"}); err != nil {
		t.Fatal(err)
	}

	report, err := rebuildIndex(ctx, store, backend)
	if err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}
	if report.Added != 0 || report.Updated != 0 || report.Deleted != 1 || report.Skipped != 0 {
		t.Fatalf("report = %+v, want {Added:0 Updated:0 Deleted:1 Skipped:0}", report)
	}
	if _, err := store.GetPackageByBase(ctx, "orphan"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("orphan package still present (err = %v)", err)
	}
	if _, err := store.GetTask(ctx, "orphan-1"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("orphan task still present (err = %v)", err)
	}
	_, total, err := store.ListBuilds(ctx, 1, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Errorf("builds after empty rebuild = %d, want 0", total)
	}

	// A second run against the now-empty database stays a no-op.
	report, err = rebuildIndex(ctx, store, backend)
	if err != nil {
		t.Fatalf("rebuildIndex (second run): %v", err)
	}
	if *report != (RebuildReport{}) {
		t.Fatalf("second report = %+v, want all-zero", report)
	}
}

// ---------------------------------------------------------------------------
// Sidecar helpers
// ---------------------------------------------------------------------------

// sidecarArtifact is one [[artifacts]] entry of a side file.
type sidecarArtifact struct {
	file    string
	kind    string
	pkgname string
	version string
	arch    string
	size    int64
	sha256  string
}

// sidecarBuild is the [build] section of a side file.
type sidecarBuild struct {
	commit      string
	upstreamRef string
	srcinfoHash string
	time        time.Time
	worker      string
}

// sidecarTOML is a side file payload.
type sidecarTOML struct {
	pkgbase   string
	branch    string
	vcs       string
	artifacts []sidecarArtifact
	build     sidecarBuild
}

// writeSidecar renders a side file and writes it to path.
func writeSidecar(t *testing.T, path string, sc sidecarTOML) {
	t.Helper()
	var b []byte
	appendf := func(format string, args ...any) {
		b = append(b, []byte(fmt.Sprintf(format, args...))...)
	}
	appendf("pkgbase = %q\n", sc.pkgbase)
	appendf("branch = %q\n", sc.branch)
	appendf("vcs = %q\n", sc.vcs)
	for _, a := range sc.artifacts {
		appendf("\n[[artifacts]]\n")
		appendf("file = %q\n", a.file)
		appendf("kind = %q\n", a.kind)
		if a.pkgname != "" {
			appendf("pkgname = %q\n", a.pkgname)
		}
		if a.version != "" {
			appendf("version = %q\n", a.version)
		}
		if a.arch != "" {
			appendf("arch = %q\n", a.arch)
		}
		if a.size != 0 {
			appendf("size = %d\n", a.size)
		}
		if a.sha256 != "" {
			appendf("sha256 = %q\n", a.sha256)
		}
	}
	appendf("\n[build]\n")
	appendf("commit = %q\n", sc.build.commit)
	if sc.build.upstreamRef != "" {
		appendf("upstream_ref = %q\n", sc.build.upstreamRef)
	}
	appendf("srcinfo_hash = %q\n", sc.build.srcinfoHash)
	appendf("time = %s\n", sc.build.time.UTC().Format(time.RFC3339))
	if sc.build.worker != "" {
		appendf("worker = %q\n", sc.build.worker)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
