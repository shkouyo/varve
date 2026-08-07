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
	"strconv"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/api"
)

// TestHeartbeatPayload verifies the heartbeat request: name, fake-/proc
// metrics, tasks: [] and the running-container states.
func TestHeartbeatPayload(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "stat", "cpu  100 0 0 900 0 0 0 0\n")
	writeFile(t, dir, "meminfo", "MemTotal: 16384000 kB\nMemAvailable: 8388608 kB\n")
	writeFile(t, dir, "uptime", "12345.67 0.00\n")

	c := newFakeClient()
	r := testRunner(t, nil, c, newFakeRuntime())
	r.metrics = newMetricsReader(dir, t.TempDir())
	r.track("t1", "c1")

	r.heartbeat(context.Background())

	req := c.heartbeatReq(0)
	if req.Name != "test-node" {
		t.Errorf("heartbeat name = %q, want test-node", req.Name)
	}
	if req.Metrics.MemTotalBytes != 16384000*1024 {
		t.Errorf("MemTotalBytes = %d", req.Metrics.MemTotalBytes)
	}
	if req.Metrics.MemUsedBytes != (16384000-8388608)*1024 {
		t.Errorf("MemUsedBytes = %d", req.Metrics.MemUsedBytes)
	}
	if req.Metrics.UptimeSecs != 12345 {
		t.Errorf("UptimeSecs = %d", req.Metrics.UptimeSecs)
	}
	if len(req.Tasks) != 0 {
		t.Errorf("tasks = %v, want empty (host progress lives in containers)", req.Tasks)
	}
	if len(req.Containers) != 1 {
		t.Fatalf("containers = %v, want 1", req.Containers)
	}
	st := req.Containers[0]
	if st.TaskID != "t1" || st.Status != "running" || st.ExitCode != nil {
		t.Errorf("container state = %+v, want {t1 running nil}", st)
	}
}

// TestHeartbeatCancelKillsContainer verifies cancelled_task_ids in the
// heartbeat response kill the matching container and flag it cancelled
// (cancellation channel 1).
func TestHeartbeatCancelKillsContainer(t *testing.T) {
	rt := newFakeRuntime()
	c := newFakeClient()
	c.heartbeatResp = &api.HeartbeatResp{CancelledTaskIDs: []string{"t1"}}
	r := testRunner(t, nil, c, rt)
	r.track("t1", "c1")

	r.heartbeat(context.Background())

	if rt.killCount() != 1 || rt.killed(0) != "c1" {
		t.Errorf("kills = %v, want [c1]", rt.kills)
	}
	if !r.isCancelled("t1") {
		t.Error("task t1 must be flagged cancelled")
	}
}

// TestPollResponseCancellations verifies the poll response also carries
// the cancellation channel and triggers the kill (dual channel).
func TestPollResponseCancellations(t *testing.T) {
	rt := newFakeRuntime()
	c := newFakeClient()
	c.pollRepeat = &api.PollResp{CancelledTaskIDs: []string{"t1"}} // no task
	r := testRunner(t, nil, c, rt)
	r.track("t1", "c1")

	if r.processOne(context.Background()) {
		t.Error("processOne must report false for a task-less poll")
	}
	if rt.killCount() != 1 || rt.killed(0) != "c1" {
		t.Errorf("kills = %v, want [c1]", rt.kills)
	}
	if !r.isCancelled("t1") {
		t.Error("task t1 must be flagged cancelled")
	}
}

// TestHeartbeatGenericErrorNoReregister verifies transient errors do not
// trigger re-registration.
func TestHeartbeatGenericErrorNoReregister(t *testing.T) {
	c := newFakeClient()
	c.heartbeatErr = errors.New("boom")
	r := testRunner(t, nil, c, newFakeRuntime())

	r.heartbeat(context.Background())
	if got := c.registerCount(); got != 0 {
		t.Errorf("register count = %d, want 0 for a transient error", got)
	}
}

// TestCancelKillReportsCancelled verifies the full cancellation path: the
// killed container's monitor reports cancelled, not failed.
func TestCancelKillReportsCancelled(t *testing.T) {
	rt := newFakeRuntime()
	rt.exitCodes["c1"] = 137 // killed by SIGKILL
	rt.blocked = make(chan struct{})
	c := newFakeClient()
	r := testRunner(t, nil, c, rt)
	withSlot(r)

	done := make(chan struct{})
	go func() {
		r.monitor(context.Background(), testTask("t1"), "tok-1", "c1")
		close(done)
	}()
	waitFor(t, 2*time.Second, func() bool { return rt.waitCount() >= 1 })

	r.cancelTasks(context.Background(), []string{"t1"})
	if rt.killCount() != 1 || rt.killed(0) != "c1" {
		t.Fatalf("kills = %v, want [c1]", rt.kills)
	}

	close(rt.blocked)
	waitFor(t, 2*time.Second, func() bool { return c.resultCount() >= 1 })
	if got := c.result(0).res.Status; got != "cancelled" {
		t.Errorf("status = %q, want cancelled", got)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not return")
	}
}

// TestHeartbeatReregisterOn401Or404 verifies heartbeat 401/404 trigger a
// re-registration.
func TestHeartbeatReregisterOn401Or404(t *testing.T) {
	for _, status := range []int{401, 404} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			c := newFakeClient()
			c.heartbeatErr = &api.APIError{Status: status, Code: "x", Message: "node gone"}
			r := testRunner(t, nil, c, newFakeRuntime())

			before := c.registerCount()
			r.heartbeat(context.Background())
			if got := c.registerCount(); got != before+1 {
				t.Errorf("register count = %d, want %d (re-register on %d)", got, before+1, status)
			}
		})
	}
}
