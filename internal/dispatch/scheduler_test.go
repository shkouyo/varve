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

package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/storage"
)

// claimAndStall claims the fifo task and advances the clock past the stall
// timeout without any progress.
func (e *testEnv) claimAndStall(t *testing.T, worker string) (string, string) {
	t.Helper()
	claimed, token := e.claim(t, worker)
	e.advance(e.cfg.Worker.StallTimeout + time.Minute)
	return claimed, token
}

// TestStallRecovery covers decision A17: the first stall re-queues the task
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
	// Re-claim the re-queued task and let it stall a second time.
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
	if env.not.calls[0].LogURL != "https://varve.example.org/builds/"+strconv.FormatInt(task.BuildID, 10) {
		t.Errorf("log URL = %q", env.not.calls[0].LogURL)
	}
}

// TestStallCancelledWins covers D4②: a stalled task with a durable cancel
// request is finalized cancelled, never re-queued.
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
	if err := env.logs.Append(strconv.FormatInt(oldTaskRow.BuildID, 10), []byte("old log")); err != nil {
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
	if err := env.logs.Append(strconv.FormatInt(recentRow.BuildID, 10), []byte("recent log")); err != nil {
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
	if err := env.logs.Append(strconv.FormatInt(failRow.BuildID, 10), []byte("failed log")); err != nil {
		t.Fatalf("append failed log: %v", err)
	}

	env.o.sweepLogs(context.Background())

	if _, err := env.logs.Read(strconv.FormatInt(oldTaskRow.BuildID, 10)); !errors.Is(err, ErrNotFound) {
		t.Errorf("old succeeded log not deleted: %v", err)
	}
	if _, err := env.logs.Read(strconv.FormatInt(recentRow.BuildID, 10)); err != nil {
		t.Errorf("recent succeeded log deleted: %v", err)
	}
	if _, err := env.logs.Read(strconv.FormatInt(failRow.BuildID, 10)); err != nil {
		t.Errorf("failed log must be permanent: %v", err)
	}
}

// TestSweepWorkers covers decision A18: agents offline for more than 24h
// are deleted, hosts never are, and disabled nodes are left alone.
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

// TestSweepStaging covers the stale-staging pass: task directories older
// than 24h are removed, fresh ones are kept.
func TestSweepStaging(t *testing.T) {
	env := newTestEnv(t)
	// The sweep enumerates the real filesystem, so this test drives the
	// orchestrator with a real local backend instead of the in-memory fake.
	backend, err := storage.OpenLocal(env.cfg.Storage.Local.Root)
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	env.o.storage = backend
	stagingRoot := filepath.Join(env.cfg.Storage.Local.Root, "staging")
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
