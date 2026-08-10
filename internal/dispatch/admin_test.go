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

package dispatch

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/repo"
)

// TestRebuildPackage covers the admin rebuild: unknown packages are not
// found, a known package is force-enqueued, and an active task blocks the
// rebuild (partial index).
func TestRebuildPackage(t *testing.T) {
	env := newTestEnv(t)
	if err := env.o.RebuildPackage(ctx(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RebuildPackage(unknown) = %v, want ErrNotFound", err)
	}

	env.buildSucceeded(t, "foo", testArtifacts("foo", "1.0-1"))
	if err := env.o.RebuildPackage(ctx(), "foo"); err != nil {
		t.Fatalf("RebuildPackage: %v", err)
	}
	taskID := env.activeTaskFor(t, "foo")
	task, err := env.store.GetTask(ctx(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "queued" {
		t.Errorf("rebuild task state = %q, want queued", task.State)
	}
	// A rebuild while the task is active is idempotent: the active task
	// already covers it, so the duplicate submission succeeds.
	if err := env.o.RebuildPackage(ctx(), "foo"); err != nil {
		t.Errorf("RebuildPackage while active = %v, want nil (idempotent)", err)
	}
	tasks, err := env.store.ListActiveTasks(ctx())
	if err != nil {
		t.Fatalf("ListActiveTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != taskID {
		t.Errorf("active tasks = %v, want only the original task %s", tasks, taskID)
	}

	// A rebuild after the task finished enqueues a fresh task.
	if err := env.store.WithTx(ctx(), func(tx *db.Tx) error {
		return tx.FinalizeTask(ctx(), taskID, "succeeded", "", env.now.UTC(), nil, nil)
	}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if err := env.o.RebuildPackage(ctx(), "foo"); err != nil {
		t.Fatalf("RebuildPackage after success: %v", err)
	}
	second := env.activeTaskFor(t, "foo")
	if second == taskID {
		t.Errorf("rebuild after success reused the finished task %s", taskID)
	}
}

// TestRebuildPackagePreservesMetadata asserts the rebuild path carries the
// full package metadata onto the enqueued change: the version fields
// (pkgver/pkgrel/epoch), pkgname/source and the already-carried
// url/licenses/... survive the enqueue upsert, and a rebuild whose build
// later fails leaves the row intact instead of blanking the version
// fields.
func TestRebuildPackagePreservesMetadata(t *testing.T) {
	env := newTestEnv(t)
	seed := &db.Package{
		Pkgbase: "meta", Branch: "main", VCSKind: "git", Arch: "x86_64",
		URL: "https://example.org/meta", Licenses: []string{"MIT"}, Conflicts: []string{"meta-legacy"}, Provides: []string{"meta-lib"},
		Pkgname: []string{"meta"}, Source: []string{"https://example.org/meta.tar.gz"},
		Pkgver: "1.0", Pkgrel: "2", Epoch: 1,
	}
	if err := env.store.UpsertPackage(ctx(), seed); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	if err := env.o.RebuildPackage(ctx(), "meta"); err != nil {
		t.Fatalf("RebuildPackage: %v", err)
	}
	taskID := env.activeTaskFor(t, "meta")

	check := func(what string) {
		t.Helper()
		pkg, err := env.store.GetPackageByBase(ctx(), "meta")
		if err != nil {
			t.Fatalf("GetPackageByBase: %v", err)
		}
		if pkg.Pkgver != "1.0" || pkg.Pkgrel != "2" || pkg.Epoch != 1 {
			t.Errorf("%s: version fields = %q/%q/%d, want 1.0/2/1", what, pkg.Pkgver, pkg.Pkgrel, pkg.Epoch)
		}
		if !reflect.DeepEqual(pkg.Pkgname, []string{"meta"}) ||
			!reflect.DeepEqual(pkg.Source, []string{"https://example.org/meta.tar.gz"}) {
			t.Errorf("%s: pkgname/source = %v/%v, want preserved", what, pkg.Pkgname, pkg.Source)
		}
		if pkg.URL != "https://example.org/meta" || !reflect.DeepEqual(pkg.Licenses, []string{"MIT"}) {
			t.Errorf("%s: url/licenses = %q/%v, want preserved", what, pkg.URL, pkg.Licenses)
		}
	}
	check("after rebuild enqueue")

	// A failed rebuild must not blank the preserved metadata either.
	if err := env.store.WithTx(ctx(), func(tx *db.Tx) error {
		return tx.FinalizeFailed(ctx(), taskID, "test: boom", env.now.UTC(), nil, nil)
	}); err != nil {
		t.Fatalf("finalize failed: %v", err)
	}
	check("after failed rebuild")
}

// TestRemoveWorker covers the admin node removal: active tasks block it,
// idle workers are deleted.
func TestRemoveWorker(t *testing.T) {
	env := newTestEnv(t)
	env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	env.claim(t, "w1")
	if err := env.o.RemoveWorker(ctx(), "w1"); !errors.Is(err, ErrConflict) {
		t.Errorf("RemoveWorker with active task = %v, want ErrConflict", err)
	}
	// A missing worker is already removed: the operation is idempotent.
	if err := env.o.RemoveWorker(ctx(), "ghost"); err != nil {
		t.Errorf("RemoveWorker(unknown) = %v, want nil (idempotent)", err)
	}

	env.registerWorker(t, "w2", "host", "host", 1)
	if err := env.o.RemoveWorker(ctx(), "w2"); err != nil {
		t.Fatalf("RemoveWorker idle: %v", err)
	}
	if _, err := env.store.GetWorkerByName(ctx(), "w2"); err == nil {
		t.Error("idle worker not removed")
	}
	// Removing again is a no-op.
	if err := env.o.RemoveWorker(ctx(), "w2"); err != nil {
		t.Errorf("RemoveWorker again = %v, want nil (idempotent)", err)
	}
}

// TestEnableWorker covers the re-enable operation: a disabled worker
// becomes online again and claims work, a missing worker is a no-op.
func TestEnableWorker(t *testing.T) {
	env := newTestEnv(t)
	env.registerWorker(t, "w1", "host", "host", 1)
	if err := env.o.DisableWorker(ctx(), "w1"); err != nil {
		t.Fatalf("DisableWorker: %v", err)
	}
	if err := env.o.EnableWorker(ctx(), "w1"); err != nil {
		t.Fatalf("EnableWorker: %v", err)
	}
	w, err := env.store.GetWorkerByName(ctx(), "w1")
	if err != nil {
		t.Fatalf("GetWorkerByName: %v", err)
	}
	if w.Status != "online" {
		t.Errorf("status after enable = %q, want online", w.Status)
	}
	// An enabled worker claims new work again.
	env.enqueue(t, "foo", "foo")
	claimed, _ := env.claim(t, "w1")
	if claimed == "" {
		t.Fatal("enabled worker could not claim")
	}
	if err := env.o.EnableWorker(ctx(), "ghost"); err != nil {
		t.Errorf("EnableWorker(unknown) = %v, want nil (idempotent)", err)
	}
}

// TestAdminIdempotency covers the repeated-submission matrix: every admin
// operation applied twice yields the same success and never leaves a
// misleading error behind.
func TestAdminIdempotency(t *testing.T) {
	env := newTestEnv(t)

	// disable/enable twice: the second call is a no-op on the same row.
	env.registerWorker(t, "w1", "host", "host", 1)
	for i := 0; i < 2; i++ {
		if err := env.o.DisableWorker(ctx(), "w1"); err != nil {
			t.Fatalf("DisableWorker pass %d: %v", i, err)
		}
	}
	w, err := env.store.GetWorkerByName(ctx(), "w1")
	if err != nil {
		t.Fatalf("GetWorkerByName: %v", err)
	}
	if w.Status != "disabled" {
		t.Errorf("status = %q, want disabled", w.Status)
	}
	for i := 0; i < 2; i++ {
		if err := env.o.EnableWorker(ctx(), "w1"); err != nil {
			t.Fatalf("EnableWorker pass %d: %v", i, err)
		}
	}

	// remove twice: the second submission finds the worker gone.
	if err := env.o.RemoveWorker(ctx(), "w1"); err != nil {
		t.Fatalf("RemoveWorker: %v", err)
	}
	if err := env.o.RemoveWorker(ctx(), "w1"); err != nil {
		t.Errorf("RemoveWorker again = %v, want nil", err)
	}
	// disable/enable after removal are no-ops too.
	if err := env.o.DisableWorker(ctx(), "w1"); err != nil {
		t.Errorf("DisableWorker after removal = %v, want nil", err)
	}

	// cancel twice on a queued task: the first finalizes it, the second
	// sees a terminal task and is a no-op.
	taskID := env.enqueue(t, "task-two", "task-two")
	for i := 0; i < 2; i++ {
		if err := env.o.CancelTask(ctx(), taskID); err != nil {
			t.Fatalf("CancelTask pass %d: %v", i, err)
		}
	}
	task, err := env.store.GetTask(ctx(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "cancelled" {
		t.Errorf("state = %q, want cancelled", task.State)
	}

	// cancel twice on a running task: the flag is set twice, harmlessly.
	taskID2 := env.enqueue(t, "task-three", "task-three")
	env.registerWorker(t, "w2", "host", "host", 1)
	env.claim(t, "w2")
	for i := 0; i < 2; i++ {
		if err := env.o.CancelTask(ctx(), taskID2); err != nil {
			t.Fatalf("CancelTask running pass %d: %v", i, err)
		}
	}
	task2, err := env.store.GetTask(ctx(), taskID2)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !task2.CancelRequested {
		t.Error("cancel_requested not set on running task")
	}
}

// TestStats covers the dashboard aggregation.
func TestStats(t *testing.T) {
	env := newTestEnv(t)
	env.buildSucceeded(t, "done", testArtifacts("done", "1.0-1"))
	env.enqueue(t, "queued-a", "queued-a")
	env.enqueue(t, "queued-b", "queued-b")
	env.registerWorker(t, "w1", "host", "host", 1)
	env.claim(t, "w1") // claims the FIFO head: queued-a

	stats, err := env.o.Stats(ctx())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.QueueLen != 1 {
		t.Errorf("QueueLen = %d, want 1", stats.QueueLen)
	}
	if stats.ByStatus["queued"] != 1 || stats.ByStatus["assigned"] != 1 || stats.ByStatus["succeeded"] != 1 {
		t.Errorf("ByStatus = %v", stats.ByStatus)
	}
	if len(stats.RecentBuilds) < 3 {
		t.Fatalf("RecentBuilds = %+v, want at least 3 builds", stats.RecentBuilds)
	}
	foundSucceeded := false
	for _, b := range stats.RecentBuilds {
		if b.Status == "succeeded" && b.Branch == "done" {
			foundSucceeded = true
		}
	}
	if !foundSucceeded {
		t.Errorf("RecentBuilds misses the succeeded done build: %+v", stats.RecentBuilds)
	}
	foundW1 := false
	for _, w := range stats.Workers {
		if w.Name == "w1" {
			foundW1 = true
		}
	}
	if !foundW1 || len(stats.Workers) == 0 {
		t.Errorf("Workers = %+v, want w1 present", stats.Workers)
	}
}

// TestValidateConflicts covers the startup integrity check: collisions
// between a pkgbase and another package's produced pkgname are listed, and
// a clean repository passes.
func TestValidateConflicts(t *testing.T) {
	env := newTestEnv(t)
	if err := env.o.ValidateConflicts(ctx()); err != nil {
		t.Fatalf("clean repo reported conflicts: %v", err)
	}
	// foo ships a subpackage named libfoo.
	env.buildSucceeded(t, "foo", []repo.Artifact{
		{File: "foo-1.0-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "foo", Version: "1.0-1", Arch: "x86_64",
			Size: int64(len(stagedContent("foo-1.0-1-x86_64.pkg.tar.zst"))), SHA256: sha256Hex(stagedContent("foo-1.0-1-x86_64.pkg.tar.zst"))},
		{File: "libfoo-1.0-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "libfoo", Version: "1.0-1", Arch: "x86_64",
			Size: int64(len(stagedContent("libfoo-1.0-1-x86_64.pkg.tar.zst"))), SHA256: sha256Hex(stagedContent("libfoo-1.0-1-x86_64.pkg.tar.zst"))},
		{File: ".SRCINFO", Kind: "srcinfo",
			Size: int64(len(stagedContent(".SRCINFO"))), SHA256: sha256Hex(stagedContent(".SRCINFO"))},
	})
	// A second package whose pkgbase collides with the subpackage.
	if err := env.o.Enqueue(ctx(), detectChange("libfoo", "libfoo"), true); err != nil {
		t.Fatalf("enqueue colliding package: %v", err)
	}
	err := env.o.ValidateConflicts(ctx())
	if err == nil {
		t.Fatal("conflicted repository validated clean")
	}
	if !errors.Is(err, ErrConflict) {
		t.Errorf("ValidateConflicts error = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "libfoo") {
		t.Errorf("conflict error should name libfoo: %v", err)
	}
}

// TestReadTailLog covers the log reader surface consumed by web.
func TestReadTailLog(t *testing.T) {
	env := newTestEnv(t)
	if err := env.logs.Append("42", []byte("line1\nline2\n")); err != nil {
		t.Fatalf("append: %v", err)
	}
	data, err := env.o.ReadLog(ctx(), "42")
	if err != nil {
		t.Fatalf("ReadLog: %v", err)
	}
	if string(data) != "line1\nline2\n" {
		t.Errorf("ReadLog = %q", data)
	}
	if _, err := env.o.ReadLog(ctx(), "999"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadLog(missing) = %v, want ErrNotFound", err)
	}
	var buf strings.Builder
	off, err := env.o.TailLog(ctx(), "42", 6, &buf)
	if err != nil {
		t.Fatalf("TailLog: %v", err)
	}
	if buf.String() != "line2\n" || off != 12 {
		t.Errorf("TailLog = %q/%d, want line2/12", buf.String(), off)
	}
}
