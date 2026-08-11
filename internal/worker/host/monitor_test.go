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
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/api"
)

// TestHandleTaskEnvInjection verifies the one-shot env set: all five
// required variables and no VARVE_TOKEN.
func TestHandleTaskEnvInjection(t *testing.T) {
	rt := newFakeRuntime()
	c := newFakeClient()
	r := testRunner(t, nil, c, rt)
	r.slots <- struct{}{} // the poll worker holds the slot before handleTask

	r.handleTask(context.Background(), testTask("t1"), "tok-1")

	waitFor(t, 2*time.Second, func() bool { return rt.runCount() >= 1 })
	run := rt.run(0)
	if run.image != "archlinux/archlinux:multilib-devel" {
		t.Errorf("image = %q", run.image)
	}
	// The zero limits of the test task pass through as "unrestricted".
	if run.cpuLimit != 0 || run.memLimit != "" {
		t.Errorf(`run limits = %d/%q, want 0/"" for a task without limits`, run.cpuLimit, run.memLimit)
	}
	want := []string{
		"VARVE_ROLE=agent",
		"VARVE_ONE_SHOT=1",
		"VARVE_TASK_ID=t1",
		"VARVE_TASK_TOKEN=tok-1",
		"VARVE_CONTROLLER_URL=https://ctrl.example.org",
	}
	assertEnvExact(t, run.env, want)
	assertNoToken(t, run.env)

	// Pull happened before the run (VARVE_PULL_IMAGE default true).
	if len(rt.pulls) != 1 || rt.pulls[0] != run.image {
		t.Errorf("pulls = %v, want one pull of the image", rt.pulls)
	}
	// The monitor released the slot when the (instant) container exited.
	waitFor(t, 2*time.Second, func() bool { return len(r.slots) == 0 })
}

// TestHandleTaskNoPullWhenDisabled verifies VARVE_PULL_IMAGE=false skips
// the pull.
func TestHandleTaskNoPullWhenDisabled(t *testing.T) {
	rt := newFakeRuntime()
	c := newFakeClient()
	cfg := testCfg()
	cfg.PullImage = false
	r := testRunner(t, cfg, c, rt)
	r.slots <- struct{}{}

	r.handleTask(context.Background(), testTask("t1"), "tok-1")

	waitFor(t, 2*time.Second, func() bool { return rt.runCount() >= 1 })
	if len(rt.pulls) != 0 {
		t.Errorf("pulls = %v, want none with PullImage=false", rt.pulls)
	}
}

// TestHandleTaskPullFailure verifies a failed pull is reported
// failed(stage=container) and the slot is released.
func TestHandleTaskPullFailure(t *testing.T) {
	rt := newFakeRuntime()
	rt.pullErr = errors.New("pull blew up")
	c := newFakeClient()
	r := testRunner(t, nil, c, rt)
	r.slots <- struct{}{}

	r.handleTask(context.Background(), testTask("t1"), "tok-1")

	if got := rt.runCount(); got != 0 {
		t.Errorf("runs = %d, want 0 after pull failure", got)
	}
	if len(r.slots) != 0 {
		t.Errorf("slot must be released after pull failure")
	}
	if c.resultCount() != 1 {
		t.Fatalf("results = %d, want 1", c.resultCount())
	}
	res := c.result(0).res
	if res.Status != "failed" || res.Error == nil || res.Error.Stage != "container" ||
		!strings.Contains(res.Error.Summary, "pull") {
		t.Errorf("result = %+v, want failed(stage=container) mentioning the pull error", res)
	}
}

// TestHandleTaskRunFailure verifies a failed run is reported the same way.
func TestHandleTaskRunFailure(t *testing.T) {
	rt := newFakeRuntime()
	rt.runErr = errors.New("run blew up")
	c := newFakeClient()
	r := testRunner(t, nil, c, rt)
	r.slots <- struct{}{}

	r.handleTask(context.Background(), testTask("t1"), "tok-1")

	if len(r.slots) != 0 {
		t.Errorf("slot must be released after run failure")
	}
	res := c.result(0).res
	if res.Status != "failed" || res.Error == nil || res.Error.Stage != "container" ||
		!strings.Contains(res.Error.Summary, "run") {
		t.Errorf("result = %+v, want failed(stage=container) mentioning the run error", res)
	}
}

// TestMonitorExitCodeClassification covers the exit classification table:
// exit 0 → no report; non-zero → failed with the exit code; signal-killed
// → the signal in the summary; OOMKilled → "OOM" in the summary.
func TestMonitorExitCodeClassification(t *testing.T) {
	cases := []struct {
		name       string
		exitCode   int
		oom        bool
		wantReport bool
		wantStatus string
		wantSubstr []string
	}{
		{"exit zero is left to the agent", 0, false, false, "", nil},
		{"non-zero exit", 3, false, true, "failed", []string{"3"}},
		{"killed by signal", 137, false, true, "failed", []string{"137", "signal 9"}},
		{"oom killed", 137, true, true, "failed", []string{"137", "signal 9", "OOM"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newFakeRuntime()
			rt.exitCodes["c1"] = tc.exitCode
			rt.oomKilled["c1"] = tc.oom
			c := newFakeClient()
			r := testRunner(t, nil, c, rt)
			withSlot(r)

			r.monitor(context.Background(), testTask("t1"), "tok-1", "c1")

			if !tc.wantReport {
				if c.resultCount() != 0 {
					t.Errorf("results = %d, want none for exit 0", c.resultCount())
				}
				return
			}
			if c.resultCount() != 1 {
				t.Fatalf("results = %d, want 1", c.resultCount())
			}
			res := c.result(0)
			if res.taskID != "t1" || res.token != "tok-1" {
				t.Errorf("reported for %q with token %q, want t1/tok-1", res.taskID, res.token)
			}
			if res.res.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", res.res.Status, tc.wantStatus)
			}
			if res.res.Error == nil {
				t.Fatalf("failed result must carry an error block")
			}
			if res.res.Error.Stage != "container" {
				t.Errorf("stage = %q, want container", res.res.Error.Stage)
			}
			for _, sub := range tc.wantSubstr {
				if !strings.Contains(res.res.Error.Summary, sub) {
					t.Errorf("summary %q must contain %q", res.res.Error.Summary, sub)
				}
			}
		})
	}
}

// TestMonitorInspectFailureFallback verifies the --rm degradation: when
// Inspect fails the exit code observed by Wait is used.
func TestMonitorInspectFailureFallback(t *testing.T) {
	rt := newFakeRuntime()
	rt.exitCodes["c1"] = 7
	rt.inspectErr = errors.New("no such container (removed by --rm)")
	c := newFakeClient()
	r := testRunner(t, nil, c, rt)
	withSlot(r)

	r.monitor(context.Background(), testTask("t1"), "tok-1", "c1")

	if c.resultCount() != 1 {
		t.Fatalf("results = %d, want 1", c.resultCount())
	}
	res := c.result(0).res
	if !strings.Contains(res.Error.Summary, "7") {
		t.Errorf("summary %q must contain the Wait exit code 7", res.Error.Summary)
	}
}

// TestMonitorBuildTimeout verifies the per-task timeout path: the
// container is killed and failed(stage=timeout) is reported.
func TestMonitorBuildTimeout(t *testing.T) {
	rt := newFakeRuntime()
	rt.blocked = make(chan struct{}) // Wait never returns on its own
	c := newFakeClient()
	r := testRunner(t, nil, c, rt)
	withSlot(r)
	clock := newFakeClock(time.Unix(1000, 0))
	r.now = clock.now
	task := testTask("t1")
	task.Build.TimeoutSeconds = 60

	done := make(chan struct{})
	go func() {
		r.monitor(context.Background(), task, "tok-1", "c1")
		close(done)
	}()

	waitFor(t, 2*time.Second, func() bool { return rt.waitCount() >= 1 })
	clock.add(61 * time.Second) // pass the deadline
	waitFor(t, 2*time.Second, func() bool { return c.resultCount() >= 1 })

	if rt.killCount() != 1 || rt.killed(0) != "c1" {
		t.Errorf("kills = %v, want [c1]", rt.kills)
	}
	res := c.result(0).res
	if res.Status != "failed" || res.Error == nil || res.Error.Stage != "timeout" {
		t.Errorf("result = %+v, want failed(stage=timeout)", res)
	}

	close(rt.blocked) // let the stray Wait goroutine finish
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not return after the timeout")
	}
}

// TestMonitorReportConflictIgnored verifies a 409 from ReportResult is
// ignored (the agent already reported).
func TestMonitorReportConflictIgnored(t *testing.T) {
	rt := newFakeRuntime()
	rt.exitCodes["c1"] = 3
	c := newFakeClient()
	c.resultErr = &api.APIError{Status: 409, Code: "conflict", Message: "already reported"}
	r := testRunner(t, nil, c, rt)
	withSlot(r)

	r.monitor(context.Background(), testTask("t1"), "tok-1", "c1")

	if c.resultCount() != 1 {
		t.Errorf("result count = %d, want 1 (the 409 must not panic or loop)", c.resultCount())
	}
}

// TestMonitorDrainCapBoundsNoTimeoutTask asserts the shutdown drain cap:
// a container with no per-task build timeout that refuses to exit is
// force-killed and reported failed(stage=timeout) once the drain cap
// passes after shutdown, instead of hanging awaitExit forever.
func TestMonitorDrainCapBoundsNoTimeoutTask(t *testing.T) {
	rt := newFakeRuntime()
	rt.blocked = make(chan struct{}) // Wait never returns on its own
	c := newFakeClient()
	r := testRunner(t, nil, c, rt)
	withSlot(r)
	r.drainCap = 30 * time.Second
	clock := newFakeClock(time.Unix(1000, 0))
	// Gate the fake clock on the monitor's first read: that read is
	// exactly the drain-deadline computation (the task has no build
	// timeout, so the main loop's deadline check short-circuits before
	// calling now). The monitor blocks inside now until the test has
	// advanced the clock, so the advance can never land before the
	// deadline is computed — a deadline computed from an already
	// advanced clock would move the drain cap with it and the result
	// wait below would misreport.
	drainEntered := make(chan struct{})
	release := make(chan struct{})
	var gateOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	r.now = func() time.Time {
		now := clock.now()
		gateOnce.Do(func() { close(drainEntered) })
		<-release
		return now
	}

	task := testTask("t1") // Build zero: no per-task timeout

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.monitor(ctx, task, "tok-1", "c1")
		close(done)
	}()
	waitFor(t, 2*time.Second, func() bool { return rt.waitCount() >= 1 })

	cancel() // shutdown: the monitor enters the drain wait
	select {
	case <-drainEntered: // the monitor is computing the drain deadline now
	case <-time.After(5 * time.Second):
		t.Fatal("monitor never entered the drain wait")
	}
	clock.add(31 * time.Second)
	unblock()
	waitFor(t, 2*time.Second, func() bool { return c.resultCount() >= 1 })

	if rt.killCount() != 1 || rt.killed(0) != "c1" {
		t.Errorf("kills = %v, want the drain cap to force-kill [c1]", rt.kills)
	}
	res := c.result(0).res
	if res.Status != "failed" || res.Error == nil || res.Error.Stage != "timeout" ||
		!strings.Contains(res.Error.Summary, "drain cap") {
		t.Errorf("result = %+v, want failed(stage=timeout) with the drain-cap summary", res)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not return after the drain cap")
	}
	close(rt.blocked) // let the stray Wait goroutine finish
}

// TestHandleTaskPassesResourceLimits asserts the per-task cpu/memory
// limits are handed to the container runtime (the config values were dead
// before: the run call hardcoded 0, "").
func TestHandleTaskPassesResourceLimits(t *testing.T) {
	rt := newFakeRuntime()
	c := newFakeClient()
	r := testRunner(t, nil, c, rt)
	r.slots <- struct{}{}

	task := testTask("t1")
	task.Build.CPULimit = 4
	task.Build.MemoryLimit = "8GiB"
	r.handleTask(context.Background(), task, "tok-1")

	waitFor(t, 2*time.Second, func() bool { return rt.runCount() >= 1 })
	run := rt.run(0)
	if run.cpuLimit != 4 || run.memLimit != "8GiB" {
		t.Errorf("run limits = %d/%q, want 4/8GiB", run.cpuLimit, run.memLimit)
	}
}
