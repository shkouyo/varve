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

package host

import (
	"context"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/api"
)

// TestNewRunnerRequiresHostRole verifies the role precondition (H1).
func TestNewRunnerRequiresHostRole(t *testing.T) {
	cfg := testCfg()
	cfg.Role = "agent"
	if _, err := NewRunner(cfg, newFakeClient()); err == nil {
		t.Error("NewRunner must reject a non-host role")
	}
}

// TestNewRunnerRequiresImage verifies the image precondition (H1).
func TestNewRunnerRequiresImage(t *testing.T) {
	cfg := testCfg()
	cfg.Image = ""
	if _, err := NewRunner(cfg, newFakeClient()); err == nil {
		t.Error("NewRunner must require VARVE_WORKER_IMAGE")
	}
}

// TestNewRunnerProbesRuntime verifies NewRunner probes docker before
// podman and wires the discovered binary (H1).
func TestNewRunnerProbesRuntime(t *testing.T) {
	old := execCommand
	t.Cleanup(func() { execCommand = old })
	var probes []string
	execCommand = probeExec(map[string]bool{"podman": true}, &probes)

	cfg := testCfg()
	cfg.WorkerName = "node-1"
	cfg.DataDir = t.TempDir()
	r, err := NewRunner(cfg, newFakeClient())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	cr, ok := r.rt.(*containerRuntime)
	if !ok {
		t.Fatalf("rt = %T, want *containerRuntime", r.rt)
	}
	if cr.bin != "podman" {
		t.Errorf("runtime binary = %q, want podman", cr.bin)
	}
	if len(probes) != 2 || probes[0] != "docker" || probes[1] != "podman" {
		t.Errorf("probes = %v, want [docker podman]", probes)
	}
	if r.name != "node-1" {
		t.Errorf("name = %q, want node-1", r.name)
	}
}

// TestNewRunnerNoRuntime verifies startup fails when docker and podman are
// both unavailable (H1, DETAIL §11.5).
func TestNewRunnerNoRuntime(t *testing.T) {
	old := execCommand
	t.Cleanup(func() { execCommand = old })
	var probes []string
	execCommand = probeExec(map[string]bool{}, &probes)

	cfg := testCfg()
	cfg.WorkerName = "node-1"
	if _, err := NewRunner(cfg, newFakeClient()); err == nil {
		t.Error("NewRunner must fail when no container runtime is available")
	}
}

// TestNewRunnerPersistsName verifies the auto-generated name is persisted
// in <VARVE_DATA_DIR>/worker-name and stable across restarts (H2).
func TestNewRunnerPersistsName(t *testing.T) {
	old := execCommand
	t.Cleanup(func() { execCommand = old })
	execCommand = probeExec(map[string]bool{"podman": true}, &[]string{})

	dataDir := t.TempDir()
	cfg := testCfg()
	cfg.DataDir = dataDir

	r1, err := NewRunner(cfg, newFakeClient())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	r2, err := NewRunner(cfg, newFakeClient())
	if err != nil {
		t.Fatalf("NewRunner (second): %v", err)
	}
	if r1.name == "" || !namePattern.MatchString(r1.name) {
		t.Errorf("name = %q, want the auto-generated format", r1.name)
	}
	if r2.name != r1.name {
		t.Errorf("name changed across restarts: %q then %q", r1.name, r2.name)
	}
}

// TestProcessOneLaunchesContainer verifies poll → handleTask wiring: the
// claimed task reaches Run with the one-shot env (H5, group 2).
func TestProcessOneLaunchesContainer(t *testing.T) {
	rt := newFakeRuntime()
	c := newFakeClient()
	c.pollRepeat = &api.PollResp{Task: testTask("t1"), ClaimToken: "tok-1"}
	r := testRunner(t, nil, c, rt)
	r.slots <- struct{}{} // the poll worker holds a slot before processOne

	if !r.processOne(context.Background()) {
		t.Fatal("processOne must report a claim")
	}

	waitFor(t, 2*time.Second, func() bool { return rt.runCount() >= 1 })
	run := rt.run(0)
	want := []string{
		"VARVE_ROLE=agent",
		"VARVE_ONE_SHOT=1",
		"VARVE_TASK_ID=t1",
		"VARVE_TASK_TOKEN=tok-1",
		"VARVE_CONTROLLER_URL=https://ctrl.example.org",
	}
	assertEnvExact(t, run.env, want)
	assertNoToken(t, run.env)
}

// TestProcessOneEmptyPoll verifies a task-less poll releases nothing and
// reports false.
func TestProcessOneEmptyPoll(t *testing.T) {
	c := newFakeClient() // pollQueue empty → task=null
	r := testRunner(t, nil, c, newFakeRuntime())
	if r.processOne(context.Background()) {
		t.Error("processOne must report false for an empty poll")
	}
	if c.pollCount() != 1 {
		t.Errorf("poll count = %d, want 1", c.pollCount())
	}
}

// TestConcurrentCapacity verifies the slot semaphore bounds concurrent
// containers to the node capacity (H5, DETAIL §11.7 group 5): with
// capacity=2 and a poller that always gets tasks, exactly two containers
// run at once and no third is started.
func TestConcurrentCapacity(t *testing.T) {
	rt := newFakeRuntime()
	rt.blocked = make(chan struct{}) // containers stay alive
	c := newFakeClient()
	for i := 1; i <= 5; i++ {
		c.pollQueue = append(c.pollQueue, &api.PollResp{
			Task:       testTask(taskID(i)),
			ClaimToken: "tok",
		})
	}
	cfg := testCfg()
	cfg.Concurrency = 2
	r := testRunner(t, cfg, c, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Both slots fill.
	waitFor(t, 3*time.Second, func() bool { return rt.runCount() >= 2 })
	time.Sleep(300 * time.Millisecond) // give any spurious third a chance
	if got := rt.runCount(); got != 2 {
		t.Errorf("concurrent containers = %d, want exactly 2", got)
	}
	if got := rt.activeCount(); got != 2 {
		t.Errorf("active containers = %d, want 2", got)
	}
	if got := c.registerCall(0).Capacity; got != 2 {
		t.Errorf("registered capacity = %d, want 2", got)
	}

	cancel()
	close(rt.blocked)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}
}

// taskID builds a distinct task id for a queue position.
func taskID(i int) string {
	return "t" + string(rune('0'+i))
}
