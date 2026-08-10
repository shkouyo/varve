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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/db"
)

// restartEnv is a test harness that can simulate a controller restart:
// the store and the orchestrator are torn down and rebuilt on the same
// database file, while the injected clock and the dispatch recorder
// survive. The fake storage, updater, signer, notifier and log store are
// per-process dependencies and are recreated like a fresh controller
// would (the persisted database is the only state that survives).
type restartEnv struct {
	path    string
	cfg     *config.ControllerConfig
	actions *fakeActionsDispatcher
	now     time.Time

	store *db.Store
	o     *OrchestratorImpl
	fs    *fakeStorage
	up    *fakeUpdater
	sig   *fakeSigner
	not   *fakeNotifier
	logs  *Logs
}

func newRestartEnv(t *testing.T) *restartEnv {
	t.Helper()
	cfg := &config.ControllerConfig{
		Source: config.SourceConfig{PollInterval: time.Hour},
		Worker: config.WorkerLimits{
			HeartbeatTimeout: 90 * time.Second,
			StallTimeout:     10 * time.Minute,
			BuildTimeout:     30 * time.Minute,
		},
		Repo:    config.RepoConfig{Sign: "off"},
		Logs:    config.LogsConfig{Dir: t.TempDir(), Retention: 90 * 24 * time.Hour, MaxBuilds: 1000},
		Storage: config.StorageConfig{Backend: "local", Local: config.LocalConfig{Root: t.TempDir()}},
		Server:  config.ServerConfig{WebURL: "https://varve.example.org"},
	}
	env := &restartEnv{
		path:    filepath.Join(t.TempDir(), "restart.db"),
		cfg:     cfg,
		actions: &fakeActionsDispatcher{},
		now:     time.Now().UTC(),
	}
	env.open(t)
	t.Cleanup(func() { env.close() })
	return env
}

// open builds a fresh orchestrator on the existing database file, the
// way a controller restart would.
func (e *restartEnv) open(t *testing.T) {
	t.Helper()
	store, err := db.Open(e.path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	log := newOpLog(t)
	e.store = store
	e.fs = newFakeStorage(t)
	e.up = &fakeUpdater{log: log}
	e.sig = newFakeSigner(t)
	e.not = newFakeNotifier(t)
	e.logs = NewLogs(t.TempDir())
	e.o = NewOrchestrator(e.cfg, store, e.fs, e.sig, e.up, e.not, e.logs)
	e.o.now = func() time.Time { return e.now }
	state := &gitState{Commit: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"}
	e.o.execCommand = fakeGitFor(t, log, state)
	e.o.mirrorDir = "/data/source/fake.git"
	e.o.actions = e.actions
}

// close tears the controller down without touching the database file.
func (e *restartEnv) close() {
	if e.o != nil {
		e.o.Stop()
		e.o = nil
	}
	if e.store != nil {
		e.store.Close()
		e.store = nil
	}
}

// restart simulates a controller restart: stop, close, reopen.
func (e *restartEnv) restart(t *testing.T) {
	t.Helper()
	e.close()
	e.open(t)
}

// advance moves the injected clock forward (shared across restarts).
func (e *restartEnv) advance(d time.Duration) { e.now = e.now.Add(d) }

// enqueue submits a change and returns the active task id.
func (e *restartEnv) enqueue(t *testing.T, pkgbase string) string {
	t.Helper()
	if err := e.o.Enqueue(context.Background(), detectChange(pkgbase, pkgbase), false); err != nil {
		t.Fatalf("Enqueue %s: %v", pkgbase, err)
	}
	return e.activeTaskFor(t, pkgbase)
}

// activeTaskFor resolves the active task id of a pkgbase.
func (e *restartEnv) activeTaskFor(t *testing.T, pkgbase string) string {
	t.Helper()
	pkg, err := e.store.GetPackageByBase(context.Background(), pkgbase)
	if err != nil {
		t.Fatalf("GetPackageByBase %s: %v", pkgbase, err)
	}
	tasks, err := e.store.ListActiveTasks(context.Background())
	if err != nil {
		t.Fatalf("ListActiveTasks: %v", err)
	}
	for _, tk := range tasks {
		if tk.PackageID == pkg.ID {
			return tk.ID
		}
	}
	t.Fatalf("no active task for %s", pkgbase)
	return ""
}

// registerWorker registers a node and returns its id.
func (e *restartEnv) registerWorker(t *testing.T, name string) int64 {
	t.Helper()
	resp, err := e.o.Register(context.Background(), RegisterReq{
		Name: name, Role: "host", Mode: "host", Arch: "x86_64", Capacity: 1, Version: "0.1.0",
	})
	if err != nil {
		t.Fatalf("Register %s: %v", name, err)
	}
	return resp.ID
}

// claim polls for one task and returns the claimed task id plus its token.
func (e *restartEnv) claim(t *testing.T, worker string) (string, string) {
	t.Helper()
	resp, err := e.o.Poll(context.Background(), PollReq{Name: worker, Arch: "x86_64"})
	if err != nil {
		t.Fatalf("Poll %s: %v", worker, err)
	}
	if resp.Task == nil {
		t.Fatalf("Poll %s: no task claimable", worker)
	}
	return resp.Task.ID, resp.ClaimToken
}

// stage uploads deterministic content for a task artifact.
func (e *restartEnv) stage(t *testing.T, taskID, file string) {
	t.Helper()
	if err := e.fs.Put(context.Background(), e.fs.StagingPath(taskID, file),
		strings.NewReader(stagedContent(file)), -1); err != nil {
		t.Fatalf("stage %s: %v", file, err)
	}
}

// TestRestartKeepsDispatchedTaskToken covers the dispatched-but-unclaimed
// case: the one-shot token handed to a runner before a controller restart
// still claims its queued task afterwards, and the recovered binding
// suppresses a re-dispatch while the run is within its claim window.
func TestRestartKeepsDispatchedTaskToken(t *testing.T) {
	env := newRestartEnv(t)
	env.cfg.Worker.Actions = enabledActions()
	taskID := env.enqueue(t, "foo")

	env.o.autoscaleWorkers(context.Background())
	if got := env.actions.count(); got != 1 {
		t.Fatalf("dispatch calls = %d, want 1", got)
	}
	call := env.actions.last()

	env.restart(t)

	// The pre-restart token still claims the queued task.
	detail, err := env.o.GetTask(context.Background(), taskID, call.token)
	if err != nil {
		t.Fatalf("GetTask with pre-restart dispatch token: %v", err)
	}
	if detail.Package.Pkgbase != "foo" {
		t.Errorf("task detail pkgbase = %q, want foo", detail.Package.Pkgbase)
	}
	// The recovered binding keeps the run bound: no double dispatch.
	env.o.autoscaleWorkers(context.Background())
	if got := env.actions.count(); got != 1 {
		t.Errorf("dispatch calls after restart = %d, want 1 (binding recovered)", got)
	}
}

// TestRestartKeepsClaimedTaskProtocol covers the claimed case: a running
// task's token survives the restart and the worker keeps driving it
// through the task protocol (log append, task fetch, result report)
// without a re-claim. A wrong token stays forbidden.
func TestRestartKeepsClaimedTaskProtocol(t *testing.T) {
	env := newRestartEnv(t)
	env.registerWorker(t, "w1")
	taskID := env.enqueue(t, "foo")
	claimedID, token := env.claim(t, "w1")
	if claimedID != taskID {
		t.Fatalf("claimed %s, want %s", claimedID, taskID)
	}

	env.restart(t)

	if _, err := env.o.AppendLog(context.Background(), taskID, token, LogSegment{Offset: 0, Data: "x"}); err != nil {
		t.Errorf("AppendLog after restart: %v", err)
	}
	if _, err := env.o.GetTask(context.Background(), taskID, token); err != nil {
		t.Errorf("GetTask after restart: %v", err)
	}
	if _, err := env.o.GetTask(context.Background(), taskID, "wrong"); !errors.Is(err, ErrForbidden) {
		t.Errorf("GetTask with wrong token = %v, want ErrForbidden", err)
	}
	// A failed report with the surviving token finalizes the task.
	res := ResultReq{Status: "failed", Error: &ResultError{Stage: "build", Summary: "boom"}}
	if err := env.o.ReportResult(context.Background(), taskID, token, res); err != nil {
		t.Fatalf("ReportResult after restart: %v", err)
	}
	task, err := env.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !isTerminal(task.State) {
		t.Errorf("task state = %q, want terminal after post-restart report", task.State)
	}
}

// TestRestartRunningTaskFinalizesSucceeded covers the full success path
// of a running task after a restart: the re-staged artifacts verify,
// the ingest runs and the task lands succeeded with the original token.
func TestRestartRunningTaskFinalizesSucceeded(t *testing.T) {
	env := newRestartEnv(t)
	env.registerWorker(t, "w1")
	taskID := env.enqueue(t, "foo")
	claimedID, token := env.claim(t, "w1")
	if claimedID != taskID {
		t.Fatalf("claimed %s, want %s", claimedID, taskID)
	}
	artifacts := testArtifacts("foo", "1.2.3-1")

	env.restart(t)

	// The staging area lives outside the restarted controller; re-stage
	// the deterministic artifacts into the fresh fake backend.
	for _, a := range artifacts {
		env.stage(t, taskID, a.File)
	}
	if err := env.o.ReportResult(context.Background(), taskID, token, ResultReq{
		Status: "succeeded", Commit: "deadbeef", Artifacts: artifacts,
	}); err != nil {
		t.Fatalf("ReportResult after restart: %v", err)
	}
	task, err := env.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "succeeded" {
		t.Errorf("task state = %q, want succeeded", task.State)
	}
}

// TestRestartRotatesExpiredDispatchToken covers the claim-timeout
// rotation across a restart: a binding that exceeded the claim window is
// released and the task is re-dispatched with a fresh token, while the
// stale token is forbidden.
func TestRestartRotatesExpiredDispatchToken(t *testing.T) {
	env := newRestartEnv(t)
	ac := enabledActions()
	ac.ClaimTimeout = 5 * time.Minute
	env.cfg.Worker.Actions = ac
	taskID := env.enqueue(t, "foo")

	env.o.autoscaleWorkers(context.Background())
	first := env.actions.last()

	// The claim window elapses, then the controller restarts.
	env.advance(ac.ClaimTimeout + time.Second)
	env.restart(t)

	env.o.autoscaleWorkers(context.Background())
	if got := env.actions.count(); got != 2 {
		t.Fatalf("dispatch calls after expiry + restart = %d, want 2", got)
	}
	second := env.actions.last()
	if second.task != taskID {
		t.Fatalf("re-dispatched task = %q, want %q", second.task, taskID)
	}
	if second.token == first.token {
		t.Errorf("re-dispatch reused the stale token")
	}
	if _, err := env.o.GetTask(context.Background(), taskID, first.token); !errors.Is(err, ErrForbidden) {
		t.Errorf("GetTask with expired token = %v, want ErrForbidden", err)
	}
	if _, err := env.o.GetTask(context.Background(), taskID, second.token); err != nil {
		t.Errorf("GetTask with fresh token = %v, want nil", err)
	}
}

// TestRestartTerminalTokenConflict asserts the terminal-token semantic
// survives a restart: a late report with the original token after the
// task was finalized is a state conflict (409), not a forbidden token
// (403).
func TestRestartTerminalTokenConflict(t *testing.T) {
	env := newRestartEnv(t)
	env.registerWorker(t, "w1")
	taskID := env.enqueue(t, "foo")
	_, token := env.claim(t, "w1")

	res := ResultReq{Status: "failed", Error: &ResultError{Stage: "build", Summary: "boom"}}
	if err := env.o.ReportResult(context.Background(), taskID, token, res); err != nil {
		t.Fatalf("ReportResult: %v", err)
	}
	env.restart(t)

	if err := env.o.ReportResult(context.Background(), taskID, token, res); !errors.Is(err, ErrConflict) {
		t.Errorf("late ReportResult = %v, want ErrConflict", err)
	}
	if _, err := env.o.GetTask(context.Background(), taskID, token); !errors.Is(err, ErrConflict) {
		t.Errorf("GetTask on terminal task = %v, want ErrConflict", err)
	}
}

// TestRestartRequeueRotatesToken asserts a task requeued after a restart
// rotates its token: the stale container's token is rejected, while the
// next claim issues a fresh one that drives the task.
func TestRestartRequeueRotatesToken(t *testing.T) {
	env := newRestartEnv(t)
	env.cfg.Worker.StallTimeout = time.Minute
	// The heartbeat window is generous so the injected advance trips the
	// stall scan without marking the worker offline (claim timestamps come
	// from the real clock and drift under race-detector scheduling).
	env.cfg.Worker.HeartbeatTimeout = 10 * time.Minute
	env.registerWorker(t, "w1")
	taskID := env.enqueue(t, "foo")
	_, token := env.claim(t, "w1")

	env.restart(t)
	env.advance(2 * time.Minute)
	if err := env.o.scanStalled(context.Background()); err != nil {
		t.Fatalf("scanStalled: %v", err)
	}
	task, err := env.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "queued" {
		t.Fatalf("state = %q, want queued after stall requeue", task.State)
	}
	// The stale token died with the requeue.
	if _, err := env.o.GetTask(context.Background(), taskID, token); !errors.Is(err, ErrForbidden) {
		t.Errorf("GetTask with stale token = %v, want ErrForbidden", err)
	}
	// A fresh claim issues a new token that drives the task.
	_, fresh := env.claim(t, "w1")
	if fresh == token {
		t.Errorf("re-claim reused the stale token")
	}
	if _, err := env.o.GetTask(context.Background(), taskID, fresh); err != nil {
		t.Errorf("GetTask with fresh token = %v, want nil", err)
	}
}
