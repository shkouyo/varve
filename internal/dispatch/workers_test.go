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
	"context"
	"errors"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/db"
)

func ctx() context.Context { return context.Background() }

// TestRegisterUpsert covers the upsert semantics: the name is the stable
// key, a second registration keeps the id and refreshes the fields.
func TestRegisterUpsert(t *testing.T) {
	env := newTestEnv(t)
	r1, err := env.o.Register(ctx(), RegisterReq{Name: "node-1", Role: "host", Mode: "host", Arch: "x86_64", Capacity: 2, Version: "0.1.0"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	r2, err := env.o.Register(ctx(), RegisterReq{Name: "node-1", Role: "host", Mode: "host", Arch: "aarch64", Capacity: 4, Version: "0.1.1"})
	if err != nil {
		t.Fatalf("Register again: %v", err)
	}
	if r1.ID != r2.ID {
		t.Errorf("id changed across re-register: %d -> %d", r1.ID, r2.ID)
	}
	w, err := env.store.GetWorkerByName(ctx(), "node-1")
	if err != nil {
		t.Fatalf("GetWorkerByName: %v", err)
	}
	if w.Arch != "aarch64" || w.Capacity != 4 || w.Version != "0.1.1" || w.Status != "online" {
		t.Errorf("worker refreshed = %+v", w)
	}
	if _, err := env.o.Register(ctx(), RegisterReq{Name: ""}); err == nil {
		t.Error("Register with empty name succeeded")
	}
}

// TestPollClaimsFIFOAndHeartbeat covers the claim path: FIFO order, the
// poll-doubles-as-heartbeat refresh and the token cache.
func TestPollClaimsFIFOAndHeartbeat(t *testing.T) {
	env := newTestEnv(t)
	env.enqueue(t, "a", "a")
	env.advance(time.Second)
	env.enqueue(t, "b", "b")
	env.advance(time.Second)
	env.enqueue(t, "c", "c")
	env.registerWorker(t, "w1", "host", "host", 4)

	for _, want := range []string{"a", "b", "c"} {
		got, token := env.claim(t, "w1")
		if got != env.activeTaskFor(t, want) {
			t.Errorf("claimed %s, want the %s task", got, want)
		}
		if len(token) != 64 {
			t.Errorf("claim token length = %d, want 64", len(token))
		}
		if err := env.o.checkToken(ctx(), got, token); err != nil {
			t.Errorf("cached token rejected: %v", err)
		}
	}
	// Heartbeat was refreshed by the polls.
	w, err := env.store.GetWorkerByName(ctx(), "w1")
	if err != nil {
		t.Fatalf("GetWorkerByName: %v", err)
	}
	if w.LastHeartbeat == nil {
		t.Fatal("poll did not refresh last_heartbeat")
	}
	// No tasks left: task=null, not an error.
	resp, err := env.o.Poll(ctx(), PollReq{Name: "w1", Arch: "x86_64"})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if resp.Task != nil {
		t.Errorf("Poll returned a task when the queue is empty: %+v", resp.Task)
	}
}

// TestPollArchFilterAndCapacity covers the db claim constraints surfaced
// through Poll: an arch mismatch blocks the claim and capacity limits the
// concurrent tasks per node.
func TestPollArchFilterAndCapacity(t *testing.T) {
	env := newTestEnv(t)
	env.enqueue(t, "foo", "foo") // arch x86_64
	env.registerWorkerArch(t, "arm", "aarch64", 1)
	// ClaimTask matches against the registered arch: no x86_64 task for it.
	resp, err := env.o.Poll(ctx(), PollReq{Name: "arm", Arch: "aarch64"})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if resp.Task != nil {
		t.Fatalf("aarch64 worker claimed an x86_64 task: %+v", resp.Task)
	}

	env.registerWorker(t, "cap1", "host", "host", 1)
	env.registerWorker(t, "cap2", "host", "host", 2)
	id1, _ := env.claim(t, "cap1")
	if id1 != env.activeTaskFor(t, "foo") {
		t.Fatalf("cap1 claimed %s", id1)
	}
	// cap1 is at capacity; cap2 has free slots and claims the next FIFO task.
	env.enqueue(t, "bar", "bar")
	if _, err := env.o.Poll(ctx(), PollReq{Name: "cap1", Arch: "x86_64"}); err != nil {
		t.Fatalf("Poll cap1: %v", err)
	}
	resp, err = env.o.Poll(ctx(), PollReq{Name: "cap2", Arch: "x86_64"})
	if err != nil || resp.Task == nil {
		t.Fatalf("cap2 should claim bar: %v %+v", err, resp)
	}
}

// TestPollNotForOfflineOrDisabled covers the status gate: neither an
// offline nor a disabled worker is allocated new work. Deregistering
// deletes the row entirely, so the node is unknown to Poll afterwards.
func TestPollNotForOfflineOrDisabled(t *testing.T) {
	env := newTestEnv(t)
	env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 4)
	env.registerWorker(t, "w2", "host", "host", 4)

	if err := env.o.Deregister(ctx(), "w1"); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	// Deregister deletes the node: Poll no longer knows it.
	if _, err := env.o.Poll(ctx(), PollReq{Name: "w1", Arch: "x86_64"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Poll deregistered worker = %v, want ErrNotFound", err)
	}

	if err := env.o.DisableWorker(ctx(), "w2"); err != nil {
		t.Fatalf("DisableWorker: %v", err)
	}
	resp, err := env.o.Poll(ctx(), PollReq{Name: "w2", Arch: "x86_64"})
	if err != nil {
		t.Fatalf("Poll disabled: %v", err)
	}
	if resp.Task != nil {
		t.Errorf("disabled worker got a task")
	}
	if _, err := env.o.Poll(ctx(), PollReq{Name: "ghost", Arch: "x86_64"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Poll unknown worker = %v, want ErrNotFound", err)
	}
}

// TestClaimBackfillsWorkerAndStarted covers Task B: claiming a task
// stamps the build row with the worker's plain-text name and started_at,
// in both host and pool (agent) modes via the shared Poll/ClaimTask path.
func TestClaimBackfillsWorkerAndStarted(t *testing.T) {
	env := newTestEnv(t)
	env.enqueue(t, "host-pkg", "host-pkg")
	env.enqueue(t, "pool-pkg", "pool-pkg")
	env.registerWorker(t, "host-node", "host", "host", 1)
	env.registerWorker(t, "pool-node", "agent", "pool", 1)

	for _, tc := range []struct {
		worker, pkgbase string
	}{
		{"host-node", "host-pkg"},
		{"pool-node", "pool-pkg"},
	} {
		claimed, _ := env.claim(t, tc.worker)
		task, err := env.store.GetTask(ctx(), claimed)
		if err != nil {
			t.Fatalf("GetTask %s: %v", claimed, err)
		}
		build, err := env.store.GetBuild(ctx(), task.BuildID)
		if err != nil {
			t.Fatalf("GetBuild: %v", err)
		}
		if build.WorkerName != tc.worker {
			t.Errorf("%s build worker_name = %q, want %q", tc.worker, build.WorkerName, tc.worker)
		}
		if build.StartedAt == nil {
			t.Errorf("%s build started_at = nil, want set at claim", tc.worker)
		}
	}
}

// TestHeartbeatProgressAndCancel covers progress application and the
// cancellation signal delivery.
func TestHeartbeatProgressAndCancel(t *testing.T) {
	env := newTestEnv(t)
	taskID := env.enqueue(t, "foo", "foo", "maint@example.org")
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, token := env.claim(t, "w1")
	if claimed != taskID {
		t.Fatalf("claimed %s", claimed)
	}

	before := env.now
	env.advance(time.Minute)
	hb := HeartbeatReq{
		Name: "w1",
		Tasks: []TaskProgress{{
			TaskID:             claimed,
			Stage:              "makepkg",
			CPUTimeNS:          123,
			MemoryBytes:        456,
			DiskTotalBytes:     789,
			DiskAvailableBytes: 1011,
			DiskUsedBytes:      1213,
			At:                 env.now,
		}},
	}
	resp, err := env.o.Heartbeat(ctx(), hb)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if len(resp.CancelledTaskIDs) != 0 {
		t.Errorf("unexpected cancels: %v", resp.CancelledTaskIDs)
	}
	task, err := env.store.GetTask(ctx(), claimed)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !task.LastProgressAt.After(before) {
		t.Errorf("last_progress_at not refreshed: %v", task.LastProgressAt)
	}
	build, err := env.store.GetBuild(ctx(), task.BuildID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if len(build.ResourceUsage) != 1 || build.ResourceUsage[0].CPUTimeNS != 123 {
		t.Errorf("resource samples = %+v, want one sample", build.ResourceUsage)
	}
	// The progress payload carries the disk fields so a build ending
	// shortly after the last flush still records disk usage (the final
	// sample collides with the last progress sample by timestamp and the
	// merge keeps the existing entry).
	if build.ResourceUsage[0].DiskTotalBytes != 789 ||
		build.ResourceUsage[0].DiskAvailableBytes != 1011 ||
		build.ResourceUsage[0].DiskUsedBytes != 1213 {
		t.Errorf("progress sample disk fields = %+v, want 789/1011/1213", build.ResourceUsage[0])
	}

	// Cancel the running task: both heartbeat and poll carry the signal.
	if err := env.o.CancelTask(ctx(), claimed); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	resp, err = env.o.Heartbeat(ctx(), HeartbeatReq{Name: "w1"})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if len(resp.CancelledTaskIDs) != 1 || resp.CancelledTaskIDs[0] != claimed {
		t.Errorf("heartbeat cancels = %v, want [%s]", resp.CancelledTaskIDs, claimed)
	}
	pollResp, err := env.o.Poll(ctx(), PollReq{Name: "w1", Arch: "x86_64"})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(pollResp.CancelledTaskIDs) != 1 || pollResp.CancelledTaskIDs[0] != claimed {
		t.Errorf("poll cancels = %v, want [%s]", pollResp.CancelledTaskIDs, claimed)
	}

	// Unknown worker heartbeat.
	if _, err := env.o.Heartbeat(ctx(), HeartbeatReq{Name: "ghost"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Heartbeat unknown = %v, want ErrNotFound", err)
	}
	_ = token
}

// TestDeregisterCovers covers the delete-self semantics and the conflict
// when a node still has active tasks.
func TestDeregisterCovers(t *testing.T) {
	env := newTestEnv(t)
	env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	env.claim(t, "w1")
	if err := env.o.Deregister(ctx(), "w1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Deregister with active task = %v, want ErrConflict", err)
	}

	env.registerWorker(t, "w2", "host", "host", 1)
	if err := env.o.Deregister(ctx(), "w2"); err != nil {
		t.Fatalf("Deregister idle: %v", err)
	}
	// Deregister deletes the row: the node is gone entirely.
	if _, err := env.store.GetWorkerByName(ctx(), "w2"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("worker still present after deregister: %v", err)
	}
	if err := env.o.Deregister(ctx(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Deregister unknown = %v, want ErrNotFound", err)
	}
}

// TestHeartbeatRejectsForeignProgress asserts progress reports are
// attributed: a heartbeat from a worker that does not own the task cannot
// refresh its last_progress_at or inject samples into its build.
func TestHeartbeatRejectsForeignProgress(t *testing.T) {
	env := newTestEnv(t)
	taskID := env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, _ := env.claim(t, "w1")
	if claimed != taskID {
		t.Fatalf("claimed %s", claimed)
	}
	taskBefore, err := env.store.GetTask(ctx(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}

	// A different worker reports progress on w1's task: rejected.
	env.registerWorker(t, "w2", "host", "host", 1)
	env.advance(time.Minute)
	resp, err := env.o.Heartbeat(ctx(), HeartbeatReq{
		Name: "w2",
		Tasks: []TaskProgress{{
			TaskID:    taskID,
			Stage:     "makepkg",
			CPUTimeNS: 999,
			At:        env.now,
		}},
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if len(resp.CancelledTaskIDs) != 0 {
		t.Errorf("unexpected cancels: %v", resp.CancelledTaskIDs)
	}
	task, err := env.store.GetTask(ctx(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !task.LastProgressAt.Equal(taskBefore.LastProgressAt) {
		t.Errorf("last_progress_at was refreshed by a foreign worker: %v -> %v",
			taskBefore.LastProgressAt, task.LastProgressAt)
	}
	build, err := env.store.GetBuild(ctx(), task.BuildID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if len(build.ResourceUsage) != 0 {
		t.Errorf("resource samples = %+v, want none (foreign progress injected)", build.ResourceUsage)
	}

	// Progress on a terminal task is ignored as well.
	if err := env.o.CancelTask(ctx(), taskID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if err := env.store.WithTx(ctx(), func(tx *db.Tx) error {
		return tx.FinalizeTask(ctx(), taskID, "cancelled", "", env.now.UTC(), nil, nil)
	}); err != nil {
		t.Fatalf("finalize cancelled: %v", err)
	}
	resp, err = env.o.Heartbeat(ctx(), HeartbeatReq{
		Name: "w1",
		Tasks: []TaskProgress{{
			TaskID:    taskID,
			Stage:     "makepkg",
			CPUTimeNS: 1,
			At:        env.now,
		}},
	})
	if err != nil {
		t.Fatalf("Heartbeat after cancel: %v", err)
	}
	task, err = env.store.GetTask(ctx(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !task.LastProgressAt.Equal(taskBefore.LastProgressAt) {
		t.Errorf("terminal task last_progress_at = %v, want untouched %v", task.LastProgressAt, taskBefore.LastProgressAt)
	}
	_ = resp
}
