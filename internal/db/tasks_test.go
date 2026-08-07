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
	"strings"
	"testing"
	"time"
)

// TestCreateTaskDedupe covers the partial unique index semantics: a second
// active task for the same package is rejected, and after the task
// reaches a terminal state a new task may be enqueued.
func TestCreateTaskDedupe(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "dedupe")

	createTask(t, s, "d1", "queued", pkg, at(0))
	dupe, dupeB := newTaskBuild("d2", "queued", pkg, at(time.Second))
	if err := s.CreateTask(testCtx, dupe, dupeB); !errors.Is(err, ErrConflict) {
		t.Fatalf("second queued task: got %v, want ErrConflict", err)
	}
	// Duplicate task id is also a conflict.
	dupeID, dupeIDB := newTaskBuild("d1", "queued", pkg, at(2*time.Second))
	if err := s.CreateTask(testCtx, dupeID, dupeIDB); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate id: got %v, want ErrConflict", err)
	}

	// Terminal state frees the partial index slot.
	if err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeTask(testCtx, "d1", "succeeded", "", at(3*time.Second), nil, nil)
	}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	createTask(t, s, "d3", "queued", pkg, at(4*time.Second))
	got, err := s.GetTask(testCtx, "d3")
	if err != nil {
		t.Fatalf("GetTask(d3): %v", err)
	}
	if got.State != "queued" {
		t.Errorf("d3 state = %q, want queued", got.State)
	}
}

// TestCreateTaskFill asserts b.ID and b.LogPath backfill plus the build
// mirror.
func TestCreateTaskFill(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "fill")
	task, build := newTaskBuild("fill-1", "queued", pkg, at(0))
	if err := s.CreateTask(testCtx, task, build); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if build.ID == "" {
		t.Error("build.ID not filled")
	}
	if !strings.HasSuffix(build.LogPath, ".log") || !strings.HasPrefix(build.LogPath, "logs/") {
		t.Errorf("build.LogPath = %q, want logs/<id>.log", build.LogPath)
	}
	if task.BuildID != build.ID {
		t.Errorf("task.BuildID = %s, want %s", task.BuildID, build.ID)
	}

	b, err := s.GetBuild(testCtx, build.ID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if b.Status != "queued" || b.LogPath != "logs/"+build.ID+".log" {
		t.Errorf("mirrored build = status %q log %q", b.Status, b.LogPath)
	}
	// Artifacts/resource_usage default to empty arrays, not null.
	if len(b.Artifacts) != 0 || len(b.ResourceUsage) != 0 {
		t.Errorf("defaults not empty: %+v", b)
	}
}

// TestMarkRunning covers the assigned precondition and the build mirror.
func TestMarkRunning(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "run")
	_, b := createTask(t, s, "run-1", "assigned", pkg, at(0))

	when := at(time.Minute)
	if err := s.MarkRunning(testCtx, "run-1", when); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	task, err := s.GetTask(testCtx, "run-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "running" {
		t.Errorf("state = %q, want running", task.State)
	}
	build, err := s.GetBuild(testCtx, b.ID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if build.Status != "running" {
		t.Errorf("build status = %q, want running (mirror)", build.Status)
	}
	if build.StartedAt == nil || !build.StartedAt.Equal(when) {
		t.Errorf("build.StartedAt = %v, want %v", build.StartedAt, when)
	}

	// Running is not assignable: MarkRunning must conflict.
	if err := s.MarkRunning(testCtx, "run-1", at(2*time.Minute)); !errors.Is(err, ErrConflict) {
		t.Errorf("MarkRunning on running task = %v, want ErrConflict", err)
	}
	// Missing task.
	if err := s.MarkRunning(testCtx, "ghost", at(2*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkRunning(ghost) = %v, want ErrNotFound", err)
	}
}

// TestTouchTaskProgress covers last_progress_at updates and ErrNotFound.
func TestTouchTaskProgress(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "touch")
	createTask(t, s, "touch-1", "running", pkg, at(0))

	when := at(5 * time.Minute)
	if err := s.TouchTaskProgress(testCtx, "touch-1", when); err != nil {
		t.Fatalf("TouchTaskProgress: %v", err)
	}
	task, err := s.GetTask(testCtx, "touch-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !task.LastProgressAt.Equal(when) {
		t.Errorf("LastProgressAt = %v, want %v", task.LastProgressAt, when)
	}
	if err := s.TouchTaskProgress(testCtx, "ghost", when); !errors.Is(err, ErrNotFound) {
		t.Errorf("TouchTaskProgress(ghost) = %v, want ErrNotFound", err)
	}
}

// TestAppendResourceSamples covers dedupe-by-timestamp merging: repeated
// samples with the same timestamp never stack.
func TestAppendResourceSamples(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "samp")
	_, b := createTask(t, s, "samp-1", "running", pkg, at(0))

	first := []Sample{
		{At: at(10 * time.Second), CPUTimeNS: 1, MemoryBytes: 100},
		{At: at(20 * time.Second), CPUTimeNS: 2, MemoryBytes: 200},
	}
	if err := s.AppendResourceSamples(testCtx, b.ID, first); err != nil {
		t.Fatalf("first append: %v", err)
	}

	// Out-of-order incoming: one duplicate, one existing timestamp with a
	// different value, one brand new timestamp.
	second := []Sample{
		{At: at(20 * time.Second), CPUTimeNS: 999, MemoryBytes: 999}, // duplicate at -> ignored
		{At: at(15 * time.Second), CPUTimeNS: 3, MemoryBytes: 300},   // new
	}
	if err := s.AppendResourceSamples(testCtx, b.ID, second); err != nil {
		t.Fatalf("second append: %v", err)
	}

	build, err := s.GetBuild(testCtx, b.ID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if len(build.ResourceUsage) != 3 {
		t.Fatalf("ResourceUsage = %v, want 3 samples", build.ResourceUsage)
	}
	// Sorted ascending by at; duplicate-at entries not stacked.
	if !build.ResourceUsage[0].At.Equal(at(10*time.Second)) ||
		!build.ResourceUsage[1].At.Equal(at(15*time.Second)) ||
		!build.ResourceUsage[2].At.Equal(at(20*time.Second)) {
		t.Errorf("order mismatch: %+v", build.ResourceUsage)
	}
	if build.ResourceUsage[2].CPUTimeNS != 2 {
		t.Errorf("duplicate-at sample overwrote original: %+v", build.ResourceUsage[2])
	}
	// Appending the same samples again is a no-op.
	if err := s.AppendResourceSamples(testCtx, b.ID, second); err != nil {
		t.Fatalf("third append: %v", err)
	}
	build, _ = s.GetBuild(testCtx, b.ID)
	if len(build.ResourceUsage) != 3 {
		t.Errorf("repeated append grew the list: %d samples", len(build.ResourceUsage))
	}

	if err := s.AppendResourceSamples(testCtx, "0000000000000000", first); !errors.Is(err, ErrNotFound) {
		t.Errorf("AppendResourceSamples(missing build) = %v, want ErrNotFound", err)
	}
}

// TestGetTask asserts full task decode including nullable fields.
func TestGetTask(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "gt")
	task, _ := createTask(t, s, "gt-1", "queued", pkg, at(0))

	if task.WorkerID != 0 || task.AssignedAt != nil || task.Attempts != 0 ||
		task.ClaimToken != "" || task.CancelRequested {
		t.Errorf("fresh task defaults mismatch: %+v", task)
	}

	w := registerWorker(t, s, "gt-worker", 1)
	claimed, err := s.ClaimTask(testCtx, w.ID, 1, "tok")
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if claimed.State != "assigned" || claimed.WorkerID != w.ID ||
		claimed.ClaimToken != "tok" || claimed.AssignedAt == nil {
		t.Errorf("claimed task mismatch: %+v", claimed)
	}
	if _, err := s.GetTask(testCtx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTask(ghost) = %v, want ErrNotFound", err)
	}
}

// TestRequeueFailedTask covers the retry primitive: fail_count+1, the
// worker and claim token are released, the build mirror returns to queued
// with worker fields cleared; a task that is no longer active conflicts.
func TestRequeueFailedTask(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "retry")
	task, b := createTask(t, s, "retry-1", "queued", pkg, at(0))
	w := registerWorker(t, s, "rw", 1)
	if _, err := s.ClaimTask(testCtx, w.ID, 1, "tok"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if err := s.RequeueFailedTask(testCtx, task.ID); err != nil {
		t.Fatalf("RequeueFailedTask: %v", err)
	}
	got, err := s.GetTask(testCtx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != "queued" || got.FailCount != 1 {
		t.Errorf("task = %+v, want queued with fail_count 1", got)
	}
	if got.WorkerID != 0 || got.ClaimToken != "" {
		t.Errorf("worker/claim not released: %+v", got)
	}
	build, err := s.GetBuild(testCtx, b.ID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if build.Status != "queued" || build.WorkerName != "" || build.StartedAt != nil {
		t.Errorf("build mirror = %+v, want queued with worker fields cleared", build)
	}
	// The task is queued now: a second requeue conflicts.
	if err := s.RequeueFailedTask(testCtx, task.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("second requeue = %v, want ErrConflict", err)
	}
	if err := s.RequeueFailedTask(testCtx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("requeue unknown = %v, want ErrNotFound", err)
	}
}

// TestFinalizeFailedStampsCooldown covers Tx.FinalizeFailed: the task and
// build reach the failed terminal state and the package's last_failed_at
// rebuild-cooldown marker is written in the same transaction.
func TestFinalizeFailedStampsCooldown(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "failpkg")
	createTask(t, s, "fail-1", "assigned", pkg, at(0))
	when := at(5 * time.Minute)
	if err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeFailed(testCtx, "fail-1", "makepkg: boom", when, nil, nil)
	}); err != nil {
		t.Fatalf("FinalizeFailed: %v", err)
	}
	got, err := s.GetTask(testCtx, "fail-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != "failed" {
		t.Errorf("task state = %q, want failed", got.State)
	}
	p, err := s.GetPackageByBase(testCtx, "failpkg")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if p.LastFailedAt == nil || !p.LastFailedAt.Equal(when) {
		t.Errorf("last_failed_at = %v, want %v (cooldown marker stamped)", p.LastFailedAt, when)
	}
}

// TestLatestBuildForPackage covers the newest-build lookup used by the
// rebuild cooldown comparison.
func TestLatestBuildForPackage(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "lbp")
	_, b1 := createTask(t, s, "lbp-1", "queued", pkg, at(0))
	if err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeTask(testCtx, "lbp-1", "succeeded", "", at(time.Minute), nil, nil)
	}); err != nil {
		t.Fatalf("FinalizeTask: %v", err)
	}
	_, b2 := createTask(t, s, "lbp-2", "queued", pkg, at(time.Minute))
	latest, err := s.LatestBuildForPackage(testCtx, pkg.ID)
	if err != nil {
		t.Fatalf("LatestBuildForPackage: %v", err)
	}
	if latest.ID != b2.ID {
		t.Errorf("latest = %s, want %s (newest by seq)", latest.ID, b2.ID)
	}
	_ = b1
	if _, err := s.LatestBuildForPackage(testCtx, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("latest for unknown package = %v, want ErrNotFound", err)
	}
}
