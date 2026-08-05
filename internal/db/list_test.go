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
	"testing"
	"time"
)

// seedListScenario builds one queued, one assigned, one running, one
// succeeded and one failed task across distinct packages.
func seedListScenario(t *testing.T, s *Store) map[string]string {
	t.Helper()
	out := map[string]string{}
	mk := func(id, state, pkgbase string) {
		pkg := mustSeedPackage(t, s, pkgbase)
		task, _ := createTask(t, s, id, state, pkg, at(0))
		out[id] = state
		_ = task
	}
	mk("lst-q", "queued", "lst-q")
	mk("lst-a", "assigned", "lst-a")
	mk("lst-r", "running", "lst-r")
	mk("lst-s", "succeeded", "lst-s")
	mk("lst-f", "failed", "lst-f")
	return out
}

// TestListStalledTasks asserts the state filter and the progress cutoff.
func TestListStalledTasks(t *testing.T) {
	s := newTestStore(t)
	seedListScenario(t, s)

	// Fresh tasks have last_progress_at = created_at = at(0); a cutoff in
	// the past excludes everything.
	tasks, err := s.ListStalledTasks(testCtx, at(-time.Hour), "assigned", "running")
	if err != nil {
		t.Fatalf("ListStalledTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks before cutoff = %d, want 0", len(tasks))
	}

	// Age the assigned and running tasks past the cutoff, and the queued
	// task for the state-filter subtest.
	for _, id := range []string{"lst-a", "lst-r", "lst-q"} {
		if err := s.TouchTaskProgress(testCtx, id, at(-2*time.Hour)); err != nil {
			t.Fatalf("age %s: %v", id, err)
		}
	}
	tasks, err = s.ListStalledTasks(testCtx, at(-time.Hour), "assigned", "running")
	if err != nil {
		t.Fatalf("ListStalledTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("stalled = %d, want 2", len(tasks))
	}
	got := map[string]bool{}
	for _, tk := range tasks {
		got[tk.ID] = true
		if tk.State != "assigned" && tk.State != "running" {
			t.Errorf("unexpected state %q for %s", tk.State, tk.ID)
		}
	}
	if !got["lst-a"] || !got["lst-r"] {
		t.Errorf("stalled set = %v, want {lst-a lst-r}", got)
	}

	// Queued tasks are excluded when not in the requested states.
	queued, err := s.ListStalledTasks(testCtx, at(-time.Hour), "queued")
	if err != nil {
		t.Fatalf("ListStalledTasks(queued): %v", err)
	}
	if len(queued) != 1 || queued[0].ID != "lst-q" {
		t.Errorf("queued stalled = %v, want [lst-q]", queued)
	}

	// No states -> empty result.
	none, err := s.ListStalledTasks(testCtx, at(-time.Hour))
	if err != nil {
		t.Fatalf("ListStalledTasks(): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("no-state call returned %d tasks", len(none))
	}
}

// TestListActiveTasks asserts the queued/assigned/running filter.
func TestListActiveTasks(t *testing.T) {
	s := newTestStore(t)
	seedListScenario(t, s)

	tasks, err := s.ListActiveTasks(testCtx)
	if err != nil {
		t.Fatalf("ListActiveTasks: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("active = %d, want 3", len(tasks))
	}
	states := map[string]bool{}
	for _, tk := range tasks {
		states[tk.State] = true
	}
	for _, st := range []string{"queued", "assigned", "running"} {
		if !states[st] {
			t.Errorf("missing active state %q in %v", st, states)
		}
	}
}

// TestListTasksByWorker asserts the worker filter.
func TestListTasksByWorker(t *testing.T) {
	s := newTestStore(t)
	pkg1 := mustSeedPackage(t, s, "byw-1")
	pkg2 := mustSeedPackage(t, s, "byw-2")
	createTask(t, s, "byw-a", "queued", pkg1, at(0))
	createTask(t, s, "byw-b", "queued", pkg2, at(time.Second))

	w := registerWorker(t, s, "byw-w", 2)
	claimed, err := s.ClaimTask(testCtx, w.ID, 2, "tok")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Only the claimed task is bound to the worker.
	tasks, err := s.ListTasksByWorker(testCtx, w.ID)
	if err != nil {
		t.Fatalf("ListTasksByWorker: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != claimed.ID {
		t.Errorf("worker tasks = %v, want [%s]", tasks, claimed.ID)
	}

	// Unknown worker -> empty.
	empty, err := s.ListTasksByWorker(testCtx, 424242)
	if err != nil {
		t.Fatalf("ListTasksByWorker(unknown): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("unknown worker tasks = %d, want 0", len(empty))
	}
}
