// SPDX-License-Identifier: AGPL-3.0-or-later
//
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
	"testing"
	"time"
)

// This file covers the methods added on behalf of the M4 dispatch module:
// UpsertPackage, GetPackageByID, GetWorkerByName, GetWorkerByID,
// RequeueTask, RequestTaskCancel and ListTimedOutTasks.

// TestUpsertPackage covers insert-then-refresh semantics: a new pkgbase
// creates a row, a second call refreshes the detection metadata but never
// touches the build-derived records.
func TestUpsertPackage(t *testing.T) {
	s := newTestStore(t)
	p := Package{Pkgbase: "fresh", Branch: "foo", VCSKind: "git", Arch: "x86_64", Maintainers: []string{"a@example.com"}}
	if err := s.UpsertPackage(testCtx, &p); err != nil {
		t.Fatalf("UpsertPackage(new): %v", err)
	}
	if p.ID == 0 {
		t.Fatal("UpsertPackage did not fill p.ID")
	}
	got, err := s.GetPackageByID(testCtx, p.ID)
	if err != nil {
		t.Fatalf("GetPackageByID: %v", err)
	}
	if got.Pkgbase != "fresh" || got.Branch != "foo" || got.VCSKind != "git" {
		t.Errorf("fresh row = %+v, want branch foo / vcs git", got)
	}
	if got.Enabled != true {
		t.Errorf("fresh package enabled = %v, want true", got.Enabled)
	}

	// Second upsert refreshes metadata + maintainers snapshot, keeps id.
	again := Package{Pkgbase: "fresh", Branch: "bar", VCSKind: "", Arch: "aarch64", Maintainers: []string{"b@example.com"}}
	if err := s.UpsertPackage(testCtx, &again); err != nil {
		t.Fatalf("UpsertPackage(existing): %v", err)
	}
	if again.ID != p.ID {
		t.Errorf("upsert changed id %d -> %d, want stable", p.ID, again.ID)
	}
	got, err = s.GetPackageByBase(testCtx, "fresh")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if got.Branch != "bar" || got.VCSKind != "" || got.Arch != "aarch64" {
		t.Errorf("refreshed = %+v, want bar//aarch64", got)
	}
	if len(got.Maintainers) != 1 || got.Maintainers[0] != "b@example.com" {
		t.Errorf("maintainers = %v, want refreshed snapshot", got.Maintainers)
	}
}

// TestGetPackageByIDNotFound covers the sentinel error.
func TestGetPackageByIDNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.GetPackageByID(testCtx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetPackageByID(missing) = %v, want ErrNotFound", err)
	}
}

// TestWorkerLookupByNameID covers both accessors and their sentinels.
func TestWorkerLookupByNameID(t *testing.T) {
	s := newTestStore(t)
	w := Worker{Name: "node-1", Role: "host", Mode: "host", Arch: "x86_64", Capacity: 2}
	if err := s.RegisterWorker(testCtx, &w); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	byName, err := s.GetWorkerByName(testCtx, "node-1")
	if err != nil {
		t.Fatalf("GetWorkerByName: %v", err)
	}
	if byName.ID != w.ID || byName.Capacity != 2 {
		t.Errorf("byName = %+v", byName)
	}
	byID, err := s.GetWorkerByID(testCtx, w.ID)
	if err != nil {
		t.Fatalf("GetWorkerByID: %v", err)
	}
	if byID.Name != "node-1" {
		t.Errorf("byID name = %q", byID.Name)
	}
	if _, err := s.GetWorkerByName(testCtx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetWorkerByName(missing) = %v, want ErrNotFound", err)
	}
	if _, err := s.GetWorkerByID(testCtx, 4242); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetWorkerByID(missing) = %v, want ErrNotFound", err)
	}
}

// TestRequeueTask covers the stall recovery transition: the worker and
// claim token are released, attempts increments, created_at is preserved
// and the build row mirrors queued with started_at cleared.
func TestRequeueTask(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "req")
	task, build := createTask(t, s, "req1", "queued", pkg, at(0))
	createdAt := task.CreatedAt
	w := Worker{Name: "host-1", Role: "host", Mode: "host", Arch: "x86_64", Capacity: 4}
	if err := s.RegisterWorker(testCtx, &w); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}

	// Claim first, then requeue.
	claimed, err := s.ClaimTask(testCtx, w.ID, 1, "tok123")
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if err := s.MarkRunning(testCtx, claimed.ID, at(time.Minute)); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := s.RequeueTask(testCtx, claimed.ID); err != nil {
		t.Fatalf("RequeueTask: %v", err)
	}
	got, err := s.GetTask(testCtx, claimed.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != "queued" {
		t.Errorf("state = %q, want queued", got.State)
	}
	if got.WorkerID != 0 || got.ClaimToken != "" {
		t.Errorf("worker/token not released: %+v", got)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", got.Attempts)
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Errorf("created_at changed: %v -> %v", createdAt, got.CreatedAt)
	}
	if got.AssignedAt != nil {
		t.Errorf("assigned_at = %v, want nil", got.AssignedAt)
	}
	b, err := s.GetBuild(testCtx, build.ID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if b.Status != "queued" || b.StartedAt != nil {
		t.Errorf("mirrored build = status %q started %v, want queued/nil", b.Status, b.StartedAt)
	}

	// Requeue of a terminal or queued task is a conflict.
	if err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeTask(testCtx, claimed.ID, "succeeded", "", at(2*time.Minute), nil, nil)
	}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if err := s.RequeueTask(testCtx, claimed.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("requeue terminal = %v, want ErrConflict", err)
	}
	if err := s.RequeueTask(testCtx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("requeue missing = %v, want ErrNotFound", err)
	}
}

// TestRequestTaskCancel covers the durable cancellation flag: it is set
// for active tasks, a no-op for terminal tasks and ErrNotFound otherwise.
func TestRequestTaskCancel(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "cancel")
	createTask(t, s, "c1", "queued", pkg, at(0))
	if err := s.RequestTaskCancel(testCtx, "c1"); err != nil {
		t.Fatalf("RequestTaskCancel: %v", err)
	}
	got, err := s.GetTask(testCtx, "c1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !got.CancelRequested {
		t.Error("cancel_requested not persisted")
	}
	if err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeTask(testCtx, "c1", "cancelled", "", at(time.Minute), nil, nil)
	}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	// Terminal task: no-op, not an error.
	if err := s.RequestTaskCancel(testCtx, "c1"); err != nil {
		t.Errorf("RequestTaskCancel(terminal) = %v, want nil", err)
	}
	if err := s.RequestTaskCancel(testCtx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RequestTaskCancel(missing) = %v, want ErrNotFound", err)
	}
}

// TestListTimedOutTasks covers the deadline predicate: only assigned or
// running tasks with an assigned_at older than before are returned.
func TestListTimedOutTasks(t *testing.T) {
	s := newTestStore(t)
	pkgA := mustSeedPackage(t, s, "timeout-a")
	pkgB := mustSeedPackage(t, s, "timeout-b")
	createTask(t, s, "t1", "queued", pkgA, at(0)) // queued: never assigned
	createTask(t, s, "t2", "queued", pkgB, at(0))
	w := Worker{Name: "pool-1", Role: "agent", Mode: "pool", Arch: "x86_64", Capacity: 4}
	if err := s.RegisterWorker(testCtx, &w); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	claimed, err := s.ClaimTask(testCtx, w.ID, 4, "tok")
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	// Backdate the claimed task's assigned_at for the scan.
	backdated := at(-time.Hour)
	if _, err := s.write.Exec(`UPDATE tasks SET assigned_at = ? WHERE id = ?`, formatTime(backdated), claimed.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	got, err := s.ListTimedOutTasks(testCtx, at(-30*time.Minute))
	if err != nil {
		t.Fatalf("ListTimedOutTasks: %v", err)
	}
	if len(got) != 1 || got[0].ID != claimed.ID {
		t.Fatalf("timed out = %+v, want [%s]", ids(got), claimed.ID)
	}
	if err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeTask(testCtx, claimed.ID, "succeeded", "", at(0), nil, nil)
	}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	got, err = s.ListTimedOutTasks(testCtx, at(-30*time.Minute))
	if err != nil {
		t.Fatalf("ListTimedOutTasks: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("timed out after finalize = %v, want none", ids(got))
	}
}

// ids extracts the task ids for readable assertions.
func ids(ts []Task) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}
