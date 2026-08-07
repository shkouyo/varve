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

// TestRegisterWorkerUpsert covers the upsert semantics: a second
// registration with the same name keeps the id and updates the fields.
func TestRegisterWorkerUpsert(t *testing.T) {
	s := newTestStore(t)

	w1 := &Worker{
		Name:     "w1",
		Role:     "agent",
		Mode:     "pool",
		Arch:     "x86_64",
		Capacity: 2,
		Status:   "online",
		Version:  "v1",
	}
	if err := s.RegisterWorker(testCtx, w1); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if w1.ID == 0 {
		t.Fatal("RegisterWorker did not fill ID")
	}

	// Second registration: same name, updated fields, same id.
	w2 := &Worker{
		Name:     "w1",
		Role:     "agent",
		Mode:     "pool",
		Arch:     "aarch64",
		Capacity: 4,
		Status:   "offline",
		Version:  "v2",
	}
	if err := s.RegisterWorker(testCtx, w2); err != nil {
		t.Fatalf("second register: %v", err)
	}
	if w2.ID != w1.ID {
		t.Errorf("second register ID = %d, want %d (same key)", w2.ID, w1.ID)
	}

	got, err := s.GetWorkerByName(testCtx, "w1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.ID != w1.ID || got.Arch != "aarch64" || got.Capacity != 4 ||
		got.Status != "offline" || got.Version != "v2" {
		t.Errorf("updated fields mismatch: %+v", got)
	}
}

// TestHeartbeat covers timestamp update, online status and ErrNotFound.
func TestHeartbeat(t *testing.T) {
	s := newTestStore(t)
	w := registerWorker(t, s, "hb", 1)

	if err := s.SetWorkerStatus(testCtx, "hb", "offline"); err != nil {
		t.Fatalf("SetWorkerStatus: %v", err)
	}
	at := at(time.Minute)
	if err := s.Heartbeat(testCtx, "hb", at); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	got, err := s.GetWorkerByName(testCtx, "hb")
	if err != nil {
		t.Fatalf("GetWorkerByName: %v", err)
	}
	if got.Status != "online" {
		t.Errorf("Status = %q after heartbeat, want online", got.Status)
	}
	if got.LastHeartbeat == nil || !got.LastHeartbeat.Equal(at) {
		t.Errorf("LastHeartbeat = %v, want %v", got.LastHeartbeat, at)
	}
	if got.ID != w.ID {
		t.Errorf("ID = %d, want %d", got.ID, w.ID)
	}

	if err := s.Heartbeat(testCtx, "ghost", at); !errors.Is(err, ErrNotFound) {
		t.Errorf("Heartbeat(ghost) = %v, want ErrNotFound", err)
	}
}

// TestSetWorkerStatus covers status updates and ErrNotFound.
func TestSetWorkerStatus(t *testing.T) {
	s := newTestStore(t)
	registerWorker(t, s, "sw", 1)

	if err := s.SetWorkerStatus(testCtx, "sw", "disabled"); err != nil {
		t.Fatalf("SetWorkerStatus: %v", err)
	}
	got, err := s.GetWorkerByName(testCtx, "sw")
	if err != nil {
		t.Fatalf("GetWorkerByName: %v", err)
	}
	if got.Status != "disabled" {
		t.Errorf("Status = %q, want disabled", got.Status)
	}

	if err := s.SetWorkerStatus(testCtx, "ghost", "online"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetWorkerStatus(ghost) = %v, want ErrNotFound", err)
	}
}

// TestListWorkers asserts ordering and completeness.
func TestListWorkers(t *testing.T) {
	s := newTestStore(t)
	registerWorker(t, s, "zebra", 1)
	registerWorker(t, s, "alpha", 2)
	registerWorker(t, s, "mike", 1)

	rows, err := s.ListWorkers(testCtx)
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	want := []string{"alpha", "mike", "zebra"}
	for i, w := range rows {
		if w.Name != want[i] {
			t.Errorf("rows[%d].Name = %q, want %q (order by name)", i, w.Name, want[i])
		}
	}
}

// TestDeleteWorker covers deletion and ErrNotFound on a missing worker.
func TestDeleteWorker(t *testing.T) {
	s := newTestStore(t)
	registerWorker(t, s, "gone", 1)

	if err := s.DeleteWorker(testCtx, "gone"); err != nil {
		t.Fatalf("DeleteWorker: %v", err)
	}
	if _, err := s.GetWorkerByName(testCtx, "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("worker still present: %v", err)
	}
	if err := s.DeleteWorker(testCtx, "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteWorker(missing) = %v, want ErrNotFound", err)
	}
}

// TestMarkStaleWorkersOffline covers the offline transition: only online
// workers whose heartbeat predates the cutoff are marked offline; fresh
// and disabled workers keep their status.
func TestMarkStaleWorkersOffline(t *testing.T) {
	s := newTestStore(t)
	registerWorker(t, s, "stale", 1)
	if err := s.Heartbeat(testCtx, "stale", at(-time.Hour)); err != nil {
		t.Fatalf("Heartbeat stale: %v", err)
	}
	registerWorker(t, s, "fresh", 1)
	if err := s.Heartbeat(testCtx, "fresh", at(time.Minute)); err != nil {
		t.Fatalf("Heartbeat fresh: %v", err)
	}
	registerWorker(t, s, "disabled", 1)
	if err := s.SetWorkerStatus(testCtx, "disabled", "disabled"); err != nil {
		t.Fatalf("SetWorkerStatus: %v", err)
	}

	n, err := s.MarkStaleWorkersOffline(testCtx, at(0))
	if err != nil {
		t.Fatalf("MarkStaleWorkersOffline: %v", err)
	}
	if n != 1 {
		t.Errorf("marked = %d, want exactly the stale worker", n)
	}
	for _, tc := range []struct {
		name, want string
	}{
		{"stale", "offline"},
		{"fresh", "online"},
		{"disabled", "disabled"},
	} {
		w, err := s.GetWorkerByName(testCtx, tc.name)
		if err != nil {
			t.Fatalf("GetWorkerByName %s: %v", tc.name, err)
		}
		if w.Status != tc.want {
			t.Errorf("%s status = %q, want %q", tc.name, w.Status, tc.want)
		}
	}
}

// TestDeleteWorkerKeepsHistory covers the no-FK contract: a worker that
// claimed a task can be deleted and the build keeps the executing node's
// name as plain text.
func TestDeleteWorkerKeepsHistory(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "hist")
	_, b := createTask(t, s, "hist-1", "queued", pkg, at(0))
	w := registerWorker(t, s, "machine", 1)
	if _, err := s.ClaimTask(testCtx, w.ID, 1, "tok"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	if err := s.DeleteWorker(testCtx, "machine"); err != nil {
		t.Fatalf("DeleteWorker with history: %v", err)
	}
	build, err := s.GetBuild(testCtx, b.ID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if build.WorkerName != "machine" {
		t.Errorf("build worker_name = %q, want %q (plain-text ownership survives)", build.WorkerName, "machine")
	}
}
