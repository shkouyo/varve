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

package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/api"
	"git.0x0f.dev/varve/internal/config"
)

// fakeRuntime is the runtime substitute (DETAIL §11.7): it records every
// call and lets tests program exit codes, OOM flags, errors and blocking
// behavior. When blocked is non-nil, Wait blocks until it is closed.
type fakeRuntime struct {
	mu sync.Mutex

	pulls    []string
	runs     []fakeRun
	kills    []string
	inspects []string
	waits    []string
	active   map[string]bool // container IDs currently inside Wait

	runErr     error
	pullErr    error
	killErr    error
	inspectErr error

	exitCodes map[string]int
	oomKilled map[string]bool

	blocked chan struct{}
}

// fakeRun records one Run call.
type fakeRun struct {
	image    string
	env      []string
	cpuLimit int
	memLimit string
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		exitCodes: map[string]int{},
		oomKilled: map[string]bool{},
		active:    map[string]bool{},
	}
}

func (f *fakeRuntime) Pull(_ context.Context, image string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls = append(f.pulls, image)
	return f.pullErr
}

func (f *fakeRuntime) Run(_ context.Context, image string, env []string, cpuLimit int, memLimit string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runErr != nil {
		return "", f.runErr
	}
	id := fmt.Sprintf("c%d", len(f.runs)+1)
	f.runs = append(f.runs, fakeRun{image: image, env: env, cpuLimit: cpuLimit, memLimit: memLimit})
	return id, nil
}

func (f *fakeRuntime) Kill(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kills = append(f.kills, id)
	return f.killErr
}

func (f *fakeRuntime) Inspect(_ context.Context, id string) (ContainerStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspects = append(f.inspects, id)
	if f.inspectErr != nil {
		return ContainerStatus{}, f.inspectErr
	}
	return ContainerStatus{
		ExitCode:  f.exitCodes[id],
		OOMKilled: f.oomKilled[id],
		Running:   f.active[id],
	}, nil
}

func (f *fakeRuntime) Wait(ctx context.Context, id string) (int, error) {
	f.mu.Lock()
	f.waits = append(f.waits, id)
	f.active[id] = true
	code := f.exitCodes[id]
	blocked := f.blocked
	f.mu.Unlock()

	if blocked != nil {
		select {
		case <-ctx.Done():
		case <-blocked:
		}
	}

	f.mu.Lock()
	delete(f.active, id)
	f.mu.Unlock()
	return code, nil
}

func (f *fakeRuntime) runCount() int    { f.mu.Lock(); defer f.mu.Unlock(); return len(f.runs) }
func (f *fakeRuntime) killCount() int   { f.mu.Lock(); defer f.mu.Unlock(); return len(f.kills) }
func (f *fakeRuntime) waitCount() int   { f.mu.Lock(); defer f.mu.Unlock(); return len(f.waits) }
func (f *fakeRuntime) activeCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.active) }
func (f *fakeRuntime) run(i int) fakeRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[i]
}
func (f *fakeRuntime) killed(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kills[i]
}

// fakeClient implements the narrowed client interface (DETAIL §11.7). It
// records every call; pollQueue is consumed in order, pollRepeat (when
// set) is returned for every poll.
type fakeClient struct {
	mu sync.Mutex

	registerCalls []api.RegisterReq
	registerErrs  int // fail the first N Register calls

	heartbeatReqs []api.HeartbeatReq
	heartbeatErr  error
	heartbeatResp *api.HeartbeatResp

	pollReqs   []api.PollReq
	pollQueue  []*api.PollResp
	pollRepeat *api.PollResp
	pollErr    error

	results   []resultCall
	resultErr error

	deregisters   []string
	deregisterErr error
}

// resultCall records one ReportResult invocation.
type resultCall struct {
	taskID string
	token  string
	res    api.ResultReq
}

func newFakeClient() *fakeClient { return &fakeClient{} }

func (c *fakeClient) Register(_ context.Context, req api.RegisterReq) (*api.RegisterResp, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.registerCalls = append(c.registerCalls, req)
	if len(c.registerCalls) <= c.registerErrs {
		return nil, errors.New("fake: register failed")
	}
	return &api.RegisterResp{ID: int64(len(c.registerCalls)), Name: req.Name}, nil
}

func (c *fakeClient) Heartbeat(_ context.Context, req api.HeartbeatReq) (*api.HeartbeatResp, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heartbeatReqs = append(c.heartbeatReqs, req)
	if c.heartbeatErr != nil {
		return nil, c.heartbeatErr
	}
	if c.heartbeatResp != nil {
		return c.heartbeatResp, nil
	}
	return &api.HeartbeatResp{}, nil
}

func (c *fakeClient) Poll(_ context.Context, req api.PollReq) (*api.PollResp, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pollReqs = append(c.pollReqs, req)
	if c.pollErr != nil {
		return nil, c.pollErr
	}
	if c.pollRepeat != nil {
		return c.pollRepeat, nil
	}
	if len(c.pollQueue) > 0 {
		resp := c.pollQueue[0]
		c.pollQueue = c.pollQueue[1:]
		return resp, nil
	}
	return &api.PollResp{}, nil
}

func (c *fakeClient) ReportResult(_ context.Context, taskID, token string, res api.ResultReq) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.results = append(c.results, resultCall{taskID: taskID, token: token, res: res})
	return c.resultErr
}

func (c *fakeClient) Deregister(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deregisters = append(c.deregisters, name)
	return c.deregisterErr
}

func (c *fakeClient) registerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.registerCalls)
}
func (c *fakeClient) registerCall(i int) api.RegisterReq {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.registerCalls[i]
}
func (c *fakeClient) pollCount() int   { c.mu.Lock(); defer c.mu.Unlock(); return len(c.pollReqs) }
func (c *fakeClient) resultCount() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.results) }
func (c *fakeClient) deregisterCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.deregisters)
}
func (c *fakeClient) heartbeatReq(i int) api.HeartbeatReq {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.heartbeatReqs[i]
}
func (c *fakeClient) result(i int) resultCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.results[i]
}

// testTask builds a minimal TaskDetail for tests. The Build block is left
// zero (no per-task timeout) unless a test assigns it directly; its type is
// dispatch-internal and not re-exported by api.
func testTask(id string) *api.TaskDetail {
	return &api.TaskDetail{ID: id}
}

// testCfg builds the default host WorkerConfig used by testRunner; it
// mirrors the production defaults of LoadWorker (DETAIL §1.2).
func testCfg() *config.WorkerConfig {
	return &config.WorkerConfig{
		Role:          "host",
		ControllerURL: "https://ctrl.example.org",
		Image:         "archlinux/archlinux:multilib-devel",
		Concurrency:   1,
		PullImage:     true,
	}
}

// withSlot acquires one capacity slot, as the poll worker does before a
// task reaches handleTask/monitor. Tests that drive handleTask or monitor
// directly must hold a slot so their release does not block.
func withSlot(r *Runner) {
	r.slots <- struct{}{}
}

// testRunner builds a fully-wired Runner over the fakes with short
// intervals so tests do not wait on real durations.
func testRunner(t *testing.T, cfg *config.WorkerConfig, c *fakeClient, rt *fakeRuntime) *Runner {
	t.Helper()
	if cfg == nil {
		cfg = testCfg()
	}
	r := newRunner(cfg, c, rt, "test-node", t.TempDir())
	r.pollInterval = 10 * time.Millisecond
	r.heartbeatInterval = time.Hour
	r.timeoutCheck = 20 * time.Millisecond
	r.drainInterval = 5 * time.Millisecond
	r.registerBackoff = time.Millisecond
	r.registerBackoffMax = 2 * time.Millisecond
	r.deregisterTimeout = time.Second
	return r
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

// writeFile writes a fixture file inside dir.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertEnvExact asserts env is exactly the want list (order-insensitive).
func assertEnvExact(t *testing.T, env, want []string) {
	t.Helper()
	if len(env) != len(want) {
		t.Fatalf("env = %v, want exactly %v", env, want)
	}
	got := append([]string(nil), env...)
	w := append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(w)
	for i := range got {
		if got[i] != w[i] {
			t.Fatalf("env = %v, want exactly %v", env, want)
		}
	}
}

// assertNoToken asserts env contains no VARVE_TOKEN entry (A10/A26).
func assertNoToken(t *testing.T, env []string) {
	t.Helper()
	for _, kv := range env {
		if strings.HasPrefix(kv, "VARVE_TOKEN=") {
			t.Errorf("env must never contain the shared VARVE_TOKEN; got %v", env)
		}
	}
}

// fakeClock is a manually advanced clock for the injected now func.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{t: start} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
