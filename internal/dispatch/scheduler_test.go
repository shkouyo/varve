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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/storage"
)

// panicDispatcher panics on every dispatch attempt; it proves the
// scheduler pass wrapper contains a panicking pass instead of killing the
// scheduler goroutine.
type panicDispatcher struct{}

func (panicDispatcher) Dispatch(ctx context.Context, ref, taskID, token string) error {
	panic("test: actions dispatcher exploded")
}

// TestRunScanPassContainsPanic covers the pass containment directly: a
// panic inside the scan pass (here the actions dispatcher) is recovered,
// the pass returns and the scheduler core still works afterwards.
func TestRunScanPassContainsPanic(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Worker.Actions = enabledActions()
	env.o.actions = panicDispatcher{}
	env.enqueue(t, "foo", "foo")

	env.o.runScanPass(context.Background()) // must not panic
	env.o.runHourlyPass(context.Background())
	if err := env.o.scanStalled(context.Background()); err != nil {
		t.Fatalf("scanStalled after a contained panic: %v", err)
	}
}

// TestSchedulerSurvivesPanickingPass drives the real loop: a panicking
// tick must not kill the scheduler goroutine, and cancellation must
// still halt it cleanly (otherwise Stop would hang on schedDone).
func TestSchedulerSurvivesPanickingPass(t *testing.T) {
	env := newTestEnv(t)
	env.o.Stop() // halt the constructor-started scheduler (30s tick)
	env.cfg.Worker.Actions = enabledActions()
	env.o.actions = panicDispatcher{}
	env.o.stallInterval = 10 * time.Millisecond
	env.enqueue(t, "foo", "foo")

	ctx, cancel := context.WithCancel(context.Background())
	env.o.schedCancel = cancel
	env.o.schedDone = make(chan struct{})
	done := make(chan struct{})
	go func() {
		env.o.runScheduler(ctx)
		close(done)
	}()
	time.Sleep(60 * time.Millisecond) // several ticks; the first panics
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler goroutine died after a panicking pass")
	}
}

// claimAndStall claims the fifo task and advances the clock past the stall
// timeout without any progress.
func (e *testEnv) claimAndStall(t *testing.T, worker string) (string, string) {
	t.Helper()
	claimed, token := e.claim(t, worker)
	e.advance(e.cfg.Worker.StallTimeout + time.Minute)
	return claimed, token
}

// heartbeat refreshes a worker's heartbeat through the public path (the
// 30s scan marks stale-heartbeat workers offline, so re-claiming a node
// after a long advance needs a fresh heartbeat first).
func (e *testEnv) heartbeat(t *testing.T, name string) {
	t.Helper()
	if _, err := e.o.Heartbeat(context.Background(), HeartbeatReq{Name: name}); err != nil {
		t.Fatalf("Heartbeat %s: %v", name, err)
	}
}

// TestStallRecovery covers the first-stall policy: the task is re-queued
// (attempts 0→1, worker and claim token released, created_at preserved).
func TestStallRecovery(t *testing.T) {
	env := newTestEnv(t)
	taskID := env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	taskBefore, err := env.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	createdAt := taskBefore.CreatedAt

	claimed, _ := env.claimAndStall(t, "w1")
	if claimed != taskID {
		t.Fatalf("claimed %s", claimed)
	}
	if err := env.o.scanStalled(context.Background()); err != nil {
		t.Fatalf("scanStalled: %v", err)
	}
	task, err := env.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "queued" {
		t.Errorf("state = %q, want queued", task.State)
	}
	if task.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", task.Attempts)
	}
	if task.WorkerID != 0 || task.ClaimToken != "" {
		t.Errorf("worker/token not released: %+v", task)
	}
	if !task.CreatedAt.Equal(createdAt) {
		t.Errorf("created_at not preserved: %v -> %v", createdAt, task.CreatedAt)
	}
	// The stale claim token is dead: a late GetTask with it is forbidden.
	if _, err := env.o.GetTask(context.Background(), taskID, "stale-token"); !errors.Is(err, ErrForbidden) {
		t.Errorf("stale token GetTask = %v, want ErrForbidden", err)
	}
}

// TestStallSecondFails covers the second stall: attempts >= 1 means the task
// is finalized failed(stalled) with a notification.
func TestStallSecondFails(t *testing.T) {
	env := newTestEnv(t)
	taskID := env.enqueue(t, "foo", "foo", "maint@example.org")
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, _ := env.claimAndStall(t, "w1")
	if claimed != taskID {
		t.Fatalf("claimed %s", claimed)
	}
	if err := env.o.scanStalled(context.Background()); err != nil {
		t.Fatalf("scanStalled: %v", err)
	}
	// Re-claim the re-queued task and let it stall a second time. The
	// first scan marked the worker offline (stale heartbeat), so it
	// heartbeats again as a recovered node first.
	env.heartbeat(t, "w1")
	claimed, _ = env.claimAndStall(t, "w1")
	if claimed != taskID {
		t.Fatalf("re-claimed %s", claimed)
	}
	if err := env.o.scanStalled(context.Background()); err != nil {
		t.Fatalf("scanStalled: %v", err)
	}
	task, err := env.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "failed" {
		t.Errorf("state = %q, want failed", task.State)
	}
	build, err := env.store.GetBuild(context.Background(), task.BuildID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if !strings.Contains(build.Error, "stalled") {
		t.Errorf("build error = %q, want stalled stage", build.Error)
	}
	if len(env.not.calls) != 1 || env.not.calls[0].Stage != "stalled" {
		t.Errorf("notifications = %+v, want one stalled notification", env.not.calls)
	}
	if env.not.calls[0].LogURL != "https://varve.example.org/builds/"+task.BuildID {
		t.Errorf("log URL = %q", env.not.calls[0].LogURL)
	}
}

// TestStallCancelledWins covers cancellation priority: a stalled task
// with a durable cancel request is finalized cancelled, never re-queued.
func TestStallCancelledWins(t *testing.T) {
	env := newTestEnv(t)
	taskID := env.enqueue(t, "foo", "foo", "maint@example.org")
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, _ := env.claimAndStall(t, "w1")
	if claimed != taskID {
		t.Fatalf("claimed %s", claimed)
	}
	if err := env.o.CancelTask(context.Background(), claimed); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if err := env.o.scanStalled(context.Background()); err != nil {
		t.Fatalf("scanStalled: %v", err)
	}
	task, err := env.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "cancelled" {
		t.Errorf("state = %q, want cancelled (cancellation wins)", task.State)
	}
	if len(env.not.calls) != 0 {
		t.Errorf("cancelled tasks must not notify: %+v", env.not.calls)
	}
}

// TestTimeoutFinalizes covers the deadline scan: an actively progressing
// task past assigned_at + build_timeout fails with stage timeout and a
// notification.
func TestTimeoutFinalizes(t *testing.T) {
	env := newTestEnv(t)
	taskID := env.enqueue(t, "foo", "foo", "maint@example.org")
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, token := env.claim(t, "w1")
	if claimed != taskID {
		t.Fatalf("claimed %s", claimed)
	}
	// Advance past the deadline, then report progress so the task is NOT
	// stalled: only the deadline predicate matches.
	env.advance(env.cfg.Worker.BuildTimeout + time.Minute)
	if _, err := env.o.AppendLog(context.Background(), claimed, token,
		LogSegment{Offset: 0, Data: "x", Progress: &TaskProgress{TaskID: claimed, At: env.now}}); err != nil {
		t.Fatalf("AppendLog: %v", err)
	}
	if err := env.o.scanStalled(context.Background()); err != nil {
		t.Fatalf("scanStalled: %v", err)
	}
	task, err := env.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "failed" {
		t.Errorf("state = %q, want failed", task.State)
	}
	build, err := env.store.GetBuild(context.Background(), task.BuildID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if !strings.Contains(build.Error, "timeout") {
		t.Errorf("build error = %q, want timeout stage", build.Error)
	}
	if len(env.not.calls) != 1 || env.not.calls[0].Stage != "timeout" {
		t.Errorf("notifications = %+v, want one timeout notification", env.not.calls)
	}
}

// TestSweepLogs covers the retention policy: succeeded logs older than the
// retention window or beyond max_builds are deleted; failed logs are
// permanent.
func TestSweepLogs(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Logs.MaxBuilds = 1

	// Old succeeded build (outside the 90d retention).
	env.advance(-100 * 24 * time.Hour)
	oldTask := env.enqueue(t, "old", "old")
	for _, a := range testArtifacts("old", "1.0-1") {
		env.stage(t, oldTask, a.File)
	}
	env.registerWorker(t, "w1", "host", "host", 1)
	oldClaimed, oldToken := env.claim(t, "w1")
	if err := env.reportSucceeded(t, oldClaimed, oldToken, testArtifacts("old", "1.0-1"), ""); err != nil {
		t.Fatalf("report old: %v", err)
	}
	oldTaskRow, _ := env.store.GetTask(context.Background(), oldClaimed)
	if err := env.logs.Append(oldTaskRow.BuildID, []byte("old log")); err != nil {
		t.Fatalf("append old log: %v", err)
	}

	// Back to ~now: a recent succeeded build and a failed build.
	env.advance(100 * 24 * time.Hour)
	recentTask := env.enqueue(t, "recent", "recent")
	for _, a := range testArtifacts("recent", "1.0-1") {
		env.stage(t, recentTask, a.File)
	}
	recentClaimed, recentToken := env.claim(t, "w1")
	if err := env.reportSucceeded(t, recentClaimed, recentToken, testArtifacts("recent", "1.0-1"), ""); err != nil {
		t.Fatalf("report recent: %v", err)
	}
	recentRow, _ := env.store.GetTask(context.Background(), recentClaimed)
	if err := env.logs.Append(recentRow.BuildID, []byte("recent log")); err != nil {
		t.Fatalf("append recent log: %v", err)
	}

	env.enqueue(t, "fail", "fail")
	env.registerWorker(t, "w2", "host", "host", 1)
	failClaimed, failToken := env.claim(t, "w2")
	if err := env.o.ReportResult(context.Background(), failClaimed, failToken,
		ResultReq{Status: "failed", Error: &ResultError{Stage: "makepkg", Summary: "boom"}}); err != nil {
		t.Fatalf("report failed: %v", err)
	}
	failRow, _ := env.store.GetTask(context.Background(), failClaimed)
	if err := env.logs.Append(failRow.BuildID, []byte("failed log")); err != nil {
		t.Fatalf("append failed log: %v", err)
	}

	env.o.sweepLogs(context.Background())

	if _, err := env.logs.Read(oldTaskRow.BuildID); !errors.Is(err, ErrNotFound) {
		t.Errorf("old succeeded log not deleted: %v", err)
	}
	if _, err := env.logs.Read(recentRow.BuildID); err != nil {
		t.Errorf("recent succeeded log deleted: %v", err)
	}
	if _, err := env.logs.Read(failRow.BuildID); err != nil {
		t.Errorf("failed log must be permanent: %v", err)
	}
}

// TestSweepLogsKeepSuccessful covers the per-package trim: with
// keep_successful = 1 only the newest succeeded log of a package
// survives the sweep, keep_successful = 0 disables the trim entirely,
// and failed logs are never touched by it.
func TestSweepLogsKeepSuccessful(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Logs.KeepSuccessful = 1
	artifacts := testArtifacts("foo", "1.0-1")
	env.registerWorker(t, "w1", "host", "host", 1)

	// buildSucceeded runs one full successful cycle for the package and
	// returns the build id with its log appended. The clock advances
	// past the poll interval first so the round-set dedupe of the name
	// conflict check lets the same pkgbase enqueue again.
	buildSucceeded := func() string {
		env.advance(env.cfg.Source.PollInterval + time.Minute)
		taskID := env.enqueue(t, "foo", "foo")
		for _, a := range artifacts {
			env.stage(t, taskID, a.File)
		}
		claimed, token := env.claim(t, "w1")
		if err := env.reportSucceeded(t, claimed, token, artifacts, ""); err != nil {
			t.Fatalf("report succeeded: %v", err)
		}
		row, err := env.store.GetTask(context.Background(), claimed)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if err := env.logs.Append(row.BuildID, []byte("log "+row.BuildID)); err != nil {
			t.Fatalf("append log: %v", err)
		}
		return row.BuildID
	}

	first := buildSucceeded()
	second := buildSucceeded()
	third := buildSucceeded()

	// A failed build of the same package: its log must survive the trim.
	env.advance(env.cfg.Source.PollInterval + time.Minute)
	env.enqueue(t, "foo", "foo")
	failClaimed, failToken := env.claim(t, "w1")
	if err := env.o.ReportResult(context.Background(), failClaimed, failToken,
		ResultReq{Status: "failed", Error: &ResultError{Stage: "makepkg", Summary: "boom"}}); err != nil {
		t.Fatalf("report failed: %v", err)
	}
	failRow, _ := env.store.GetTask(context.Background(), failClaimed)
	if err := env.logs.Append(failRow.BuildID, []byte("failed log")); err != nil {
		t.Fatalf("append failed log: %v", err)
	}

	env.o.sweepLogs(context.Background())

	// Newest succeeded log kept, the two older ones trimmed.
	if _, err := env.logs.Read(third); err != nil {
		t.Errorf("newest succeeded log deleted: %v", err)
	}
	for _, id := range []string{first, second} {
		if _, err := env.logs.Read(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("older succeeded log %s not trimmed: %v", id, err)
		}
	}
	if _, err := env.logs.Read(failRow.BuildID); err != nil {
		t.Errorf("failed log trimmed: %v", err)
	}

	// keep_successful = 0 disables the per-package trim entirely.
	env.cfg.Logs.KeepSuccessful = 0
	keepOne := buildSucceeded()
	keepTwo := buildSucceeded()
	env.o.sweepLogs(context.Background())
	for _, id := range []string{keepOne, keepTwo} {
		if _, err := env.logs.Read(id); err != nil {
			t.Errorf("log trimmed with keep_successful = 0: %v", err)
		}
	}
}

// TestSweepWorkers covers the worker sweep: agents offline for more than
// 24h are deleted, hosts never are, and disabled nodes are left alone.
// Offline marking itself happens in the 30s scan (scanStalled); the sweep
// only reaps the long-dead agents.
func TestSweepWorkers(t *testing.T) {
	env := newTestEnv(t)
	env.advance(-25 * time.Hour)
	env.registerWorker(t, "agent-old", "agent", "pool", 1)
	env.registerWorker(t, "host-old", "host", "host", 1)
	env.registerWorker(t, "agent-disabled", "agent", "pool", 1)
	env.advance(25 * time.Hour)
	env.registerWorker(t, "agent-fresh", "agent", "pool", 1)
	if err := env.o.DisableWorker(context.Background(), "agent-disabled"); err != nil {
		t.Fatalf("DisableWorker: %v", err)
	}

	// The 30s scan marks the stale nodes offline (disabled nodes keep
	// their status).
	if err := env.o.scanStalled(context.Background()); err != nil {
		t.Fatalf("scanStalled: %v", err)
	}
	env.o.sweepWorkers(context.Background())

	if _, err := env.store.GetWorkerByName(context.Background(), "agent-old"); err == nil {
		t.Error("stale agent not deleted")
	}
	host, err := env.store.GetWorkerByName(context.Background(), "host-old")
	if err != nil {
		t.Fatalf("host deleted: %v", err)
	}
	if host.Status != "offline" {
		t.Errorf("stale host status = %q, want offline", host.Status)
	}
	fresh, err := env.store.GetWorkerByName(context.Background(), "agent-fresh")
	if err != nil {
		t.Fatalf("fresh agent deleted: %v", err)
	}
	if fresh.Status != "online" {
		t.Errorf("fresh agent status = %q, want online", fresh.Status)
	}
	disabled, err := env.store.GetWorkerByName(context.Background(), "agent-disabled")
	if err != nil {
		t.Fatalf("disabled agent deleted: %v", err)
	}
	if disabled.Status != "disabled" {
		t.Errorf("disabled status = %q", disabled.Status)
	}
}

// TestStaleHeartbeatOffline covers the offline transition driven by the
// 30s scan: a worker whose heartbeat is older than heartbeat_timeout is
// marked offline within one scan cycle, a fresh worker stays online and a
// disabled worker keeps its status.
func TestStaleHeartbeatOffline(t *testing.T) {
	env := newTestEnv(t)
	env.registerWorker(t, "stale", "host", "host", 1)
	env.registerWorker(t, "fresh", "host", "host", 1)
	env.registerWorker(t, "disabled", "agent", "pool", 1)

	// Past the timeout: the stale worker's heartbeat ages out.
	env.advance(env.cfg.Worker.HeartbeatTimeout + time.Second)
	// The fresh worker still heartbeats.
	if _, err := env.o.Heartbeat(ctx(), HeartbeatReq{Name: "fresh"}); err != nil {
		t.Fatalf("Heartbeat fresh: %v", err)
	}
	if err := env.o.DisableWorker(ctx(), "disabled"); err != nil {
		t.Fatalf("DisableWorker: %v", err)
	}

	if err := env.o.scanStalled(context.Background()); err != nil {
		t.Fatalf("scanStalled: %v", err)
	}

	stale, err := env.store.GetWorkerByName(ctx(), "stale")
	if err != nil {
		t.Fatalf("GetWorkerByName stale: %v", err)
	}
	if stale.Status != "offline" {
		t.Errorf("stale status = %q, want offline", stale.Status)
	}
	fresh, err := env.store.GetWorkerByName(ctx(), "fresh")
	if err != nil {
		t.Fatalf("GetWorkerByName fresh: %v", err)
	}
	if fresh.Status != "online" {
		t.Errorf("fresh status = %q, want online", fresh.Status)
	}
	disabled, err := env.store.GetWorkerByName(ctx(), "disabled")
	if err != nil {
		t.Fatalf("GetWorkerByName disabled: %v", err)
	}
	if disabled.Status != "disabled" {
		t.Errorf("disabled status = %q, want disabled", disabled.Status)
	}

	// A subsequent poll refuses work for the offline node.
	env.enqueue(t, "foo", "foo")
	resp, err := env.o.Poll(ctx(), PollReq{Name: "stale", Arch: "x86_64"})
	if err != nil {
		t.Fatalf("Poll offline: %v", err)
	}
	if resp.Task != nil {
		t.Errorf("offline worker got a task")
	}
}

// TestSweepStaging covers the stale-staging pass: task directories older
// than 24h are removed, fresh ones are kept.
func TestSweepStaging(t *testing.T) {
	env := newTestEnv(t)
	// The sweep enumerates the real filesystem, so this test drives the
	// orchestrator with a real local backend instead of the in-memory fake.
	backend, err := storage.OpenLocal(env.cfg.Storage.Local.Root, "")
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	env.o.storage = backend
	stagingRoot := backend.StagingDir()
	oldDir := filepath.Join(stagingRoot, "old-task")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "a.pkg.tar.zst"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	backdate := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(oldDir, backdate, backdate); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	freshDir := filepath.Join(stagingRoot, "fresh-task")
	if err := os.MkdirAll(freshDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(freshDir, "b.pkg.tar.zst"), []byte("y"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	env.o.sweepStaging(context.Background())

	if _, err := os.Stat(oldDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale staging dir not swept: %v", err)
	}
	if _, err := os.Stat(freshDir); err != nil {
		t.Errorf("fresh staging dir removed: %v", err)
	}
}

// TestSweepStagingCustomDir runs the stale-staging pass against a real
// local backend whose staging tree lives in a configured directory outside
// the repository root: only directories under that tree are considered.
func TestSweepStagingCustomDir(t *testing.T) {
	env := newTestEnv(t)
	root := t.TempDir()
	staging := t.TempDir() // absolute staging dir outside the root
	backend, err := storage.OpenLocal(root, staging)
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	env.o.storage = backend

	oldDir := filepath.Join(staging, "old-task")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "a.pkg.tar.zst"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	backdate := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(oldDir, backdate, backdate); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	freshDir := filepath.Join(staging, "fresh-task")
	if err := os.MkdirAll(freshDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// A non-staging directory under the root must never be touched.
	foreignDir := filepath.Join(root, "foreign")
	if err := os.MkdirAll(foreignDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	backdate = time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(foreignDir, backdate, backdate); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	env.o.sweepStaging(context.Background())

	if _, err := os.Stat(oldDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale staging dir not swept: %v", err)
	}
	if _, err := os.Stat(freshDir); err != nil {
		t.Errorf("fresh staging dir removed: %v", err)
	}
	if _, err := os.Stat(foreignDir); err != nil {
		t.Errorf("non-staging dir removed: %v", err)
	}
}

// TestSweepStagingKeepsActiveTasks asserts the staging sweep never removes
// the directory of an active task: a task that has been queued or running
// for more than 24h (queue backlog, long build) must keep its staged
// source archive and early artifacts.
func TestSweepStagingKeepsActiveTasks(t *testing.T) {
	env := newTestEnv(t)
	backend, err := storage.OpenLocal(env.cfg.Storage.Local.Root, "")
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	env.o.storage = backend
	stagingRoot := backend.StagingDir()

	// A queued task whose staging dir is well past the 24h mark.
	taskID := env.enqueue(t, "foo", "foo")
	liveDir := filepath.Join(stagingRoot, taskID)
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(liveDir, "source.tar.zst"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	backdate := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(liveDir, backdate, backdate); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	// An unrelated stale directory (no task) is still swept.
	staleDir := filepath.Join(stagingRoot, "no-such-task")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Chtimes(staleDir, backdate, backdate); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	env.o.sweepStaging(context.Background())

	if _, err := os.Stat(liveDir); err != nil {
		t.Errorf("active task staging dir removed: %v", err)
	}
	if _, err := os.Stat(staleDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale dir not swept: %v", err)
	}
}
