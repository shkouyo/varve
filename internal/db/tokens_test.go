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
	"testing"
	"time"
)

// TestDispatchedAtColumn asserts migration 010 added the dispatched_at
// column to tasks.
func TestDispatchedAtColumn(t *testing.T) {
	s := newTestStore(t)
	var n int
	if err := s.read.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name = 'dispatched_at'`).Scan(&n); err != nil {
		t.Fatalf("check dispatched_at column: %v", err)
	}
	if n != 1 {
		t.Error("tasks.dispatched_at column missing after migration")
	}
}

// TestDispatchBindingRoundTrip covers the persist, list and clear
// operations of a one-shot dispatch binding.
func TestDispatchBindingRoundTrip(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "binding")
	createTask(t, s, "binding-1", "queued", pkg, at(0))

	when := at(time.Minute)
	if err := s.SetDispatchBinding(testCtx, "binding-1", "tok-1", when); err != nil {
		t.Fatalf("SetDispatchBinding: %v", err)
	}
	got, err := s.TaskClaimToken(testCtx, "binding-1")
	if err != nil {
		t.Fatalf("TaskClaimToken: %v", err)
	}
	if got != "tok-1" {
		t.Errorf("claim token = %q, want tok-1", got)
	}
	bindings, err := s.ListDispatchBindings(testCtx)
	if err != nil {
		t.Fatalf("ListDispatchBindings: %v", err)
	}
	if len(bindings) != 1 || bindings[0].TaskID != "binding-1" ||
		bindings[0].Token != "tok-1" || !bindings[0].DispatchedAt.Equal(when) {
		t.Errorf("bindings = %+v, want binding-1/tok-1 dispatched at %v", bindings, when)
	}

	if err := s.ClearDispatchBinding(testCtx, "binding-1"); err != nil {
		t.Fatalf("ClearDispatchBinding: %v", err)
	}
	if got, _ := s.TaskClaimToken(testCtx, "binding-1"); got != "" {
		t.Errorf("claim token after clear = %q, want empty", got)
	}
	if bindings, _ := s.ListDispatchBindings(testCtx); len(bindings) != 0 {
		t.Errorf("bindings after clear = %+v, want none", bindings)
	}
	// Clearing a task without a binding is a no-op.
	if err := s.ClearDispatchBinding(testCtx, "binding-1"); err != nil {
		t.Errorf("second ClearDispatchBinding: %v", err)
	}
}

// TestSetDispatchBindingGuardsClaimed asserts the state guard: a task
// that was claimed by a worker (state assigned) is never overwritten,
// and a missing task reports ErrNotFound.
func TestSetDispatchBindingGuardsClaimed(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "guard")
	createTask(t, s, "guard-1", "queued", pkg, at(0))
	w := registerWorker(t, s, "guard-node", 1)
	if _, err := s.ClaimTask(testCtx, w.ID, 1, "worker-token"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if err := s.SetDispatchBinding(testCtx, "guard-1", "dispatch-token", at(time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatalf("SetDispatchBinding on claimed task = %v, want ErrConflict", err)
	}
	if got, _ := s.TaskClaimToken(testCtx, "guard-1"); got != "worker-token" {
		t.Errorf("claim token = %q, want worker-token (claim never clobbered)", got)
	}
	if err := s.SetDispatchBinding(testCtx, "missing", "tok", at(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetDispatchBinding on missing task = %v, want ErrNotFound", err)
	}
}

// TestRequeueClearsDispatchBinding asserts the requeue primitives drop
// the persisted binding, so a re-dispatched task starts with a fresh
// token.
func TestRequeueClearsDispatchBinding(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "requeue")
	createTask(t, s, "requeue-1", "queued", pkg, at(0))
	if err := s.SetDispatchBinding(testCtx, "requeue-1", "dispatch-token", at(time.Minute)); err != nil {
		t.Fatalf("SetDispatchBinding: %v", err)
	}
	w := registerWorker(t, s, "requeue-node", 1)
	if _, err := s.ClaimTask(testCtx, w.ID, 1, "worker-token"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if err := s.RequeueTask(testCtx, "requeue-1"); err != nil {
		t.Fatalf("RequeueTask: %v", err)
	}
	task, err := s.GetTask(testCtx, "requeue-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.ClaimToken != "" {
		t.Errorf("claim token after requeue = %q, want empty", task.ClaimToken)
	}
	if bindings, _ := s.ListDispatchBindings(testCtx); len(bindings) != 0 {
		t.Errorf("bindings after requeue = %+v, want none", bindings)
	}

	// The failed-retry requeue drops a binding the same way.
	createTask(t, s, "requeue-2", "queued", mustSeedPackage(t, s, "requeue-b"), at(time.Minute))
	if err := s.SetDispatchBinding(testCtx, "requeue-2", "dispatch-token", at(2*time.Minute)); err != nil {
		t.Fatalf("SetDispatchBinding: %v", err)
	}
	if err := s.ClaimTaskToken(testCtx, "requeue-2", "worker-token", at(3*time.Minute)); err != nil {
		t.Fatalf("ClaimTaskToken: %v", err)
	}
	if err := s.RequeueFailedTask(testCtx, "requeue-2"); err != nil {
		t.Fatalf("RequeueFailedTask: %v", err)
	}
	if bindings, _ := s.ListDispatchBindings(testCtx); len(bindings) != 0 {
		t.Errorf("bindings after failed requeue = %+v, want none", bindings)
	}
}

// TestTaskClaimTokenSurvivesReopen simulates a controller restart at the
// store level: a claimed task's token and a queued task's dispatch
// binding survive close and reopen of the database file.
func TestTaskClaimTokenSurvivesReopen(t *testing.T) {
	path := t.TempDir() + "/restart.db"
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	pkgA := seedPackage(t, s1, Package{Pkgbase: "reopen-a", Branch: "main", Arch: "x86_64"})
	pkgB := seedPackage(t, s1, Package{Pkgbase: "reopen-b", Branch: "main", Arch: "x86_64"})
	createTask(t, s1, "reopen-queued", "queued", pkgA, at(0))
	createTask(t, s1, "reopen-running", "queued", pkgB, at(time.Second))

	if err := s1.SetDispatchBinding(testCtx, "reopen-queued", "dispatch-tok", at(time.Minute)); err != nil {
		t.Fatalf("SetDispatchBinding: %v", err)
	}
	if err := s1.ClaimTaskToken(testCtx, "reopen-running", "worker-tok", at(2*time.Minute)); err != nil {
		t.Fatalf("ClaimTaskToken: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()
	if got, err := s2.TaskClaimToken(testCtx, "reopen-queued"); err != nil || got != "dispatch-tok" {
		t.Errorf("queued task token = %q, %v, want dispatch-tok", got, err)
	}
	if got, err := s2.TaskClaimToken(testCtx, "reopen-running"); err != nil || got != "worker-tok" {
		t.Errorf("running task token = %q, %v, want worker-tok", got, err)
	}
	bindings, err := s2.ListDispatchBindings(testCtx)
	if err != nil {
		t.Fatalf("ListDispatchBindings: %v", err)
	}
	if len(bindings) != 1 || bindings[0].TaskID != "reopen-queued" || bindings[0].Token != "dispatch-tok" {
		t.Errorf("bindings across reopen = %+v, want reopen-queued/dispatch-tok", bindings)
	}
}

// TestSetDispatchBindingConcurrentClaim asserts the claim race is safe:
// a dispatch binding never clobbers a concurrent worker claim. Whichever
// op wins, the final claim token is the worker's token once the claim
// happened.
func TestSetDispatchBindingConcurrentClaim(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "race")
	createTask(t, s, "race-1", "queued", pkg, at(0))
	w := registerWorker(t, s, "race-node", 1)

	done := make(chan error, 2)
	go func() {
		_, err := s.ClaimTask(testCtx, w.ID, 1, "worker-tok")
		done <- err
	}()
	go func() {
		done <- s.SetDispatchBinding(testCtx, "race-1", "dispatch-tok", at(time.Minute))
	}()
	for i := 0; i < 2; i++ {
		err := <-done
		// SetDispatchBinding legitimately conflicts when the claim won
		// first; the claim itself must always succeed.
		if err != nil && !errors.Is(err, ErrConflict) {
			t.Fatalf("concurrent op: %v", err)
		}
	}
	task, err := s.GetTask(testCtx, "race-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "assigned" {
		t.Errorf("task state = %q, want assigned (a fresh worker always claims)", task.State)
	}
	if task.ClaimToken != "worker-tok" {
		t.Errorf("claimed task token = %q, want worker-tok", task.ClaimToken)
	}
}
