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

package agent

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// TestPoolRegistersAndIdlesOut asserts the pool lifecycle: register as a
// capacity-1 agent node (auto-generated name), poll with no tasks, exit
// via the idle timeout with a deregister.
func TestPoolRegistersAndIdlesOut(t *testing.T) {
	f := &fakeClient{}
	cfg := configForTest(t, false)
	cfg.TaskID, cfg.TaskToken = "", ""
	r := NewRunner(cfg, f)
	clock := newFakeClock(time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC))
	r.now = clock.now
	r.pollInterval = time.Millisecond
	r.registerBackoff = func(int) time.Duration { return time.Millisecond }
	r.procDir = t.TempDir()

	done := make(chan error, 1)
	go func() { done <- r.runPool(context.Background()) }()

	// Wait for registration, then push the clock past the idle timeout.
	if !waitFor(t, 5*time.Second, func() bool { return f.regCount() > 0 }) {
		t.Fatal("node never registered")
	}
	clock.advance(cfg.PoolIdleTimeout + time.Second)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runPool: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pool never idled out")
	}

	if f.regCount() != 1 {
		t.Errorf("register calls = %d, want 1", f.regCount())
	}
	reg := f.regReqs[0]
	if reg.Role != "agent" || reg.Mode != "pool" || reg.Capacity != 1 || reg.Arch != "x86_64" || reg.Version != version {
		t.Errorf("register req = %+v", reg)
	}
	if !regexp.MustCompile(`^[a-z]+-[a-z]+-[0-9]+$`).MatchString(reg.Name) {
		t.Errorf("auto-generated name = %q", reg.Name)
	}
	if len(f.deregNames) != 1 || f.deregNames[0] != reg.Name {
		t.Errorf("deregister = %v, want [%s]", f.deregNames, reg.Name)
	}
}

// TestPoolManualName asserts VARVE_WORKER_NAME is used verbatim.
func TestPoolManualName(t *testing.T) {
	f := &fakeClient{}
	cfg := configForTest(t, false)
	cfg.WorkerName = "manual-node"
	cfg.TaskID, cfg.TaskToken = "", ""
	r := NewRunner(cfg, f)
	clock := newFakeClock(time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC))
	r.now = clock.now
	r.pollInterval = time.Millisecond
	r.registerBackoff = func(int) time.Duration { return time.Millisecond }
	r.procDir = t.TempDir()

	done := make(chan error, 1)
	go func() { done <- r.runPool(context.Background()) }()
	if !waitFor(t, 5*time.Second, func() bool { return f.regCount() > 0 }) {
		t.Fatal("node never registered")
	}
	clock.advance(cfg.PoolIdleTimeout + time.Second)
	if err := <-done; err != nil {
		t.Fatalf("runPool: %v", err)
	}
	if f.regReqs[0].Name != "manual-node" {
		t.Errorf("register name = %q, want manual-node", f.regReqs[0].Name)
	}
}

// TestPoolRegisterBackoff asserts exponential retry until registration
// succeeds (same rule as the host).
func TestPoolRegisterBackoff(t *testing.T) {
	f := &fakeClient{regFailTimes: 2, regErr: errors.New("boom")}
	cfg := configForTest(t, false)
	cfg.TaskID, cfg.TaskToken = "", ""
	r := NewRunner(cfg, f)
	var waits []time.Duration
	r.registerBackoff = func(failures int) time.Duration {
		waits = append(waits, backoffDelay(failures))
		return time.Millisecond
	}

	if err := r.registerWithBackoff(context.Background(), "n-1"); err != nil {
		t.Fatalf("registerWithBackoff: %v", err)
	}
	if f.regCount() != 3 {
		t.Errorf("register attempts = %d, want 3 (2 failures + success)", f.regCount())
	}
	// Backoff values follow the 5s→60s doubling schedule.
	want := []string{"5s", "10s"}
	if len(waits) != 2 || waits[0].String() != want[0] || waits[1].String() != want[1] {
		t.Errorf("backoff waits = %v, want %v", waits, want)
	}
}

// TestPoolHeartbeatPayload asserts the heartbeat carries system metrics,
// the running task's progress and empty containers.
func TestPoolHeartbeatPayload(t *testing.T) {
	f := &fakeClient{}
	cfg := configForTest(t, false)
	cfg.TaskID, cfg.TaskToken = "", ""
	r := NewRunner(cfg, f)
	proc := t.TempDir()
	writeFile(t, filepath.Join(proc, "stat"), "cpu  100 0 0 100 0 0 0 0 0 0\n")
	writeFile(t, filepath.Join(proc, "meminfo"), "MemTotal: 8000000 kB\nMemAvailable: 3000000 kB\n")
	writeFile(t, filepath.Join(proc, "uptime"), "42.0 10.0\n")
	r.procDir = proc

	r.state.begin("t-1")
	r.state.setStage("makepkg")
	r.sendHeartbeat(context.Background(), "node-1")
	r.state.end()

	if len(f.heartbeats) != 1 {
		t.Fatalf("heartbeats = %d, want 1", len(f.heartbeats))
	}
	hb := f.heartbeats[0]
	if hb.Name != "node-1" {
		t.Errorf("heartbeat name = %q", hb.Name)
	}
	if hb.Metrics.MemTotalBytes != 8000000*1024 || hb.Metrics.UptimeSecs != 42 {
		t.Errorf("metrics = %+v", hb.Metrics)
	}
	if len(hb.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1 while running", len(hb.Tasks))
	}
	if hb.Tasks[0].TaskID != "t-1" || hb.Tasks[0].Stage != "makepkg" {
		t.Errorf("task progress = %+v", hb.Tasks[0])
	}
	if hb.Containers == nil || len(hb.Containers) != 0 {
		t.Errorf("containers must be an empty list, got %v", hb.Containers)
	}

	// Idle heartbeats carry an empty task list.
	r.sendHeartbeat(context.Background(), "node-1")
	if len(f.heartbeats[1].Tasks) != 0 {
		t.Errorf("idle heartbeat tasks = %v, want empty", f.heartbeats[1].Tasks)
	}
}

// TestPoolPollCancellationDelivered asserts poll responses carry channel-1
// cancellation signals for the running task.
func TestPoolPollCancellationDelivered(t *testing.T) {
	f := &fakeClient{}
	r := NewRunner(configForTest(t, false), f)
	stateCh := r.state.begin("t-1")
	defer r.state.end()

	if !r.state.cancelTask("t-1") {
		t.Fatal("cancelTask(t-1) not delivered")
	}
	select {
	case <-stateCh:
	default:
		t.Fatal("task cancellation channel never closed")
	}
	// A stale id from a finished task is ignored.
	if r.state.cancelTask("other") {
		t.Error("cancelTask for a non-running task should be ignored")
	}
}
