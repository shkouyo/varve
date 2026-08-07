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
	"sync"
	"testing"
	"time"
)

// TestClaimFIFO asserts that tasks are claimed in creation order even when
// inserted out of order.
func TestClaimFIFO(t *testing.T) {
	s := newTestStore(t)
	// Distinct packages so all four are independently claimable.
	pkgs := []Package{
		mustSeedPackage(t, s, "f-a"),
		mustSeedPackage(t, s, "f-b"),
		mustSeedPackage(t, s, "f-c"),
		mustSeedPackage(t, s, "f-d"),
	}
	// Insert with created_at shuffled.
	createTask(t, s, "f-3", "queued", pkgs[2], at(3*time.Second))
	createTask(t, s, "f-1", "queued", pkgs[0], at(time.Second))
	createTask(t, s, "f-4", "queued", pkgs[3], at(4*time.Second))
	createTask(t, s, "f-2", "queued", pkgs[1], at(2*time.Second))

	w := registerWorker(t, s, "fifo", 10)
	want := []string{"f-1", "f-2", "f-3", "f-4"}
	for _, id := range want {
		task, err := s.ClaimTask(testCtx, w.ID, 10, "tok-"+id)
		if err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
		if task.ID != id {
			t.Fatalf("claimed %q, want %q (FIFO by created_at)", task.ID, id)
		}
	}
	if _, err := s.ClaimTask(testCtx, w.ID, 10, "tok-last"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("claim after exhaustion = %v, want ErrNoTask", err)
	}
}

// TestClaimSkipActivePackage asserts that a queued task whose package
// already has an active task is skipped (partial-index dedupe plus claim
// guard).
func TestClaimSkipActivePackage(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "dup")
	createTask(t, s, "active-1", "assigned", pkg, at(time.Second))
	// Cannot even enqueue a second queued task for the same package, but a
	// direct row with a different package stays claimable.
	other := mustSeedPackage(t, s, "other")
	createTask(t, s, "other-1", "queued", other, at(2*time.Second))

	w := registerWorker(t, s, "skip", 2)
	task, err := s.ClaimTask(testCtx, w.ID, 2, "tok")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if task.ID != "other-1" {
		t.Fatalf("claimed %q, want other-1 (active package skipped)", task.ID)
	}
	// The active package has no queued task left; nothing to claim now.
	if _, err := s.ClaimTask(testCtx, w.ID, 2, "tok2"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("second claim = %v, want ErrNoTask", err)
	}
}

// TestClaimCapacity asserts that a worker cannot exceed its capacity.
func TestClaimCapacity(t *testing.T) {
	s := newTestStore(t)
	pkgs := []Package{
		mustSeedPackage(t, s, "c-1"),
		mustSeedPackage(t, s, "c-2"),
		mustSeedPackage(t, s, "c-3"),
	}
	for i, p := range pkgs {
		createTask(t, s, "c-task-"+string(rune('a'+i)), "queued", p, at(time.Duration(i+1)*time.Second))
	}
	w := registerWorker(t, s, "cap", 2)

	if _, err := s.ClaimTask(testCtx, w.ID, 2, "tok1"); err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if _, err := s.ClaimTask(testCtx, w.ID, 2, "tok2"); err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if _, err := s.ClaimTask(testCtx, w.ID, 2, "tok3"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("claim 3 = %v, want ErrNoTask (capacity full)", err)
	}
}

// TestClaimArchFilter asserts arch matching against the package arch.
func TestClaimArchFilter(t *testing.T) {
	s := newTestStore(t)
	x86 := seedPackage(t, s, Package{Pkgbase: "x86", Branch: "main", Arch: "x86_64", Enabled: true})
	arm := seedPackage(t, s, Package{Pkgbase: "arm", Branch: "main", Arch: "aarch64", Enabled: true})
	createTask(t, s, "x86-1", "queued", x86, at(time.Second))
	createTask(t, s, "arm-1", "queued", arm, at(2*time.Second))

	xw := registerWorker(t, s, "x86-worker", 1)
	task, err := s.ClaimTask(testCtx, xw.ID, 1, "tok")
	if err != nil {
		t.Fatalf("x86 worker claim: %v", err)
	}
	if task.ID != "x86-1" {
		t.Fatalf("x86 worker got %q, want x86-1", task.ID)
	}
	if _, err := s.ClaimTask(testCtx, xw.ID, 1, "tok2"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("x86 worker second claim = %v, want ErrNoTask (arm task filtered)", err)
	}
}

// TestClaimAnyArchMatrix asserts the arch-independent contract: a package
// declaring arch=any is claimable by every worker architecture, and an
// "any" worker claims packages of every architecture. The old exact-match
// claim (p.arch = ?) never matched an "any" package against a concrete
// worker, leaving such tasks in the queue forever.
func TestClaimAnyArchMatrix(t *testing.T) {
	tests := []struct {
		name    string
		pkgArch string
		wkArch  string
		want    bool // whether the queued task must be claimable
	}{
		{name: "any package by x86_64 worker", pkgArch: "any", wkArch: "x86_64", want: true},
		{name: "any package by aarch64 worker", pkgArch: "any", wkArch: "aarch64", want: true},
		{name: "any package by riscv64 worker", pkgArch: "any", wkArch: "riscv64", want: true},
		{name: "any package by any worker", pkgArch: "any", wkArch: "any", want: true},
		{name: "x86_64 package by any worker", pkgArch: "x86_64", wkArch: "any", want: true},
		{name: "multi package by any worker", pkgArch: "aarch64|x86_64", wkArch: "any", want: true},
		{name: "x86_64 package by x86_64 worker", pkgArch: "x86_64", wkArch: "x86_64", want: true},
		{name: "x86_64 package by aarch64 worker", pkgArch: "x86_64", wkArch: "aarch64", want: false},
		// Legacy single-value rows (pre-multi-arch storage) stay claimable.
		{name: "legacy aarch64 row by aarch64 worker", pkgArch: "aarch64", wkArch: "aarch64", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			pkg := seedPackage(t, s, Package{Pkgbase: "p", Branch: "main", Arch: tt.pkgArch, Enabled: true})
			createTask(t, s, "p-1", "queued", pkg, at(0))
			w := registerWorkerArch(t, s, "w", tt.wkArch, 1)

			task, err := s.ClaimTask(testCtx, w.ID, 1, "tok")
			if tt.want {
				if err != nil {
					t.Fatalf("claim = %v, want a claimed task", err)
				}
				if task == nil || task.ID != "p-1" {
					t.Fatalf("claimed %+v, want p-1", task)
				}
			} else if !errors.Is(err, ErrNoTask) {
				t.Fatalf("claim = %v, want ErrNoTask", err)
			}
		})
	}
}

// TestClaimMultiArch asserts every element of a package's declared arch
// set is matched at claim time: a worker matches when its architecture is
// any element, and a set sharing no element with the worker is filtered.
// The old code stored and matched only the first element.
func TestClaimMultiArch(t *testing.T) {
	s := newTestStore(t)
	multi := seedPackage(t, s, Package{Pkgbase: "multi", Branch: "main", Arch: "aarch64|x86_64", Enabled: true})
	createTask(t, s, "multi-1", "queued", multi, at(time.Second))
	armOnly := seedPackage(t, s, Package{Pkgbase: "arm", Branch: "main", Arch: "aarch64|riscv64", Enabled: true})
	createTask(t, s, "arm-1", "queued", armOnly, at(2*time.Second))

	// x86_64 worker: matches multi (second element), filters arm-only.
	xw := registerWorkerArch(t, s, "xw", "x86_64", 1)
	task, err := s.ClaimTask(testCtx, xw.ID, 1, "tok")
	if err != nil {
		t.Fatalf("x86_64 worker claim: %v", err)
	}
	if task.ID != "multi-1" {
		t.Fatalf("x86_64 worker got %q, want multi-1", task.ID)
	}
	if _, err := s.ClaimTask(testCtx, xw.ID, 1, "tok2"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("x86_64 worker second claim = %v, want ErrNoTask (arm-1 filtered)", err)
	}

	// aarch64 worker: matches arm-1 (first element).
	aw := registerWorkerArch(t, s, "aw", "aarch64", 1)
	task, err = s.ClaimTask(testCtx, aw.ID, 1, "tok3")
	if err != nil {
		t.Fatalf("aarch64 worker claim: %v", err)
	}
	if task.ID != "arm-1" {
		t.Fatalf("aarch64 worker got %q, want arm-1", task.ID)
	}
}

// TestClaimUnknownWorker asserts ErrNotFound for an unregistered worker.
func TestClaimUnknownWorker(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "uw")
	createTask(t, s, "uw-1", "queued", pkg, at(0))
	if _, err := s.ClaimTask(testCtx, 12345, 1, "tok"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim by unknown worker = %v, want ErrNotFound", err)
	}
}

// TestClaimMirror asserts the build row mirrors the assigned state: the
// worker's plain-text name and started_at are backfilled at claim time.
func TestClaimMirror(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "mirror")
	_, b := createTask(t, s, "mirror-1", "queued", pkg, at(0))
	w := registerWorker(t, s, "mirror-w", 1)

	claimed, err := s.ClaimTask(testCtx, w.ID, 1, "tok")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	build, err := s.GetBuild(testCtx, b.ID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if build.Status != "assigned" {
		t.Errorf("build status = %q, want assigned (mirror)", build.Status)
	}
	if build.WorkerName != "mirror-w" {
		t.Errorf("build worker_name = %q, want mirror-w (plain-text backfill)", build.WorkerName)
	}
	if build.StartedAt == nil {
		t.Error("build started_at = nil, want set at claim time")
	}
	if claimed.BuildID != b.ID {
		t.Errorf("claimed.BuildID = %s, want %s", claimed.BuildID, b.ID)
	}
}

// TestClaimConcurrent is the atomicity test: N goroutines racing for a
// single task — exactly one succeeds.
func TestClaimConcurrent(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "race")
	createTask(t, s, "race-1", "queued", pkg, at(0))
	w := registerWorker(t, s, "race-w", 1)

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			task, err := s.ClaimTask(testCtx, w.ID, 1, "tok")
			if err != nil {
				results <- err
				return
			}
			if task.ID != "race-1" {
				results <- errors.New("claimed wrong task")
				return
			}
			results <- nil
		}(i)
	}
	wg.Wait()
	close(results)

	success := 0
	failures := 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrNoTask) {
			failures++
		} else {
			t.Errorf("unexpected claim error: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("successes = %d, want exactly 1 (lost update or double claim)", success)
	}
	if failures != n-1 {
		t.Fatalf("ErrNoTask count = %d, want %d", failures, n-1)
	}

	// Sanity: the task is assigned to the worker exactly once.
	task, err := s.GetTask(testCtx, "race-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "assigned" || task.WorkerID != w.ID {
		t.Errorf("task state after race = %+v", task)
	}
}

// TestClaimConcurrentFile repeats the race on a real WAL file database.
func TestClaimConcurrentFile(t *testing.T) {
	s := newFileTestStore(t)
	pkg := mustSeedPackage(t, s, "race-file")
	createTask(t, s, "race-file-1", "queued", pkg, at(0))
	w := registerWorker(t, s, "race-w", 1)

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := s.ClaimTask(testCtx, w.ID, 1, "tok")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrNoTask):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("successes = %d, want exactly 1", success)
	}
}
