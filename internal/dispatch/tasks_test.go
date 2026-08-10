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
	"errors"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/db"
)

// TestGetTaskTransitions covers the one-shot detail fetch: the first call
// moves assigned → running with started_at, later calls are idempotent, and
// wrong tokens are forbidden.
func TestGetTaskTransitions(t *testing.T) {
	env := newTestEnv(t)
	env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	taskID, token := env.claim(t, "w1")

	detail, err := env.o.GetTask(ctx(), taskID, token)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if detail.Package.Pkgbase != "foo" || detail.Source.Mode != "clone" || detail.Source.Commit == "" {
		t.Errorf("detail = %+v, want foo/clone with commit", detail)
	}
	if detail.Build.TimeoutSeconds != 1800 {
		t.Errorf("timeout = %d, want 1800", detail.Build.TimeoutSeconds)
	}
	if detail.Build.Deadline.IsZero() {
		t.Error("deadline missing")
	}
	if detail.Packager != "" {
		t.Errorf("packager = %q, want empty by default", detail.Packager)
	}
	task, err := env.store.GetTask(ctx(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "running" {
		t.Errorf("state = %q, want running", task.State)
	}

	// Idempotent re-fetch.
	again, err := env.o.GetTask(ctx(), taskID, token)
	if err != nil {
		t.Fatalf("GetTask again: %v", err)
	}
	if again.ID != detail.ID {
		t.Errorf("detail id changed")
	}

	// Wrong token.
	if _, err := env.o.GetTask(ctx(), taskID, "deadbeef"); !errors.Is(err, ErrForbidden) {
		t.Errorf("GetTask bad token = %v, want ErrForbidden", err)
	}
	// Unknown task: the token check fires first.
	if _, err := env.o.GetTask(ctx(), "ghost", token); !errors.Is(err, ErrForbidden) {
		t.Errorf("GetTask unknown = %v, want ErrForbidden", err)
	}
}

// TestAppendLogOffsetSemantics covers the strict offset contract: matching
// offsets append, mismatches return ErrConflict wrapped in OffsetError with
// the current offset, and the ack offset advances accordingly.
func TestAppendLogOffsetSemantics(t *testing.T) {
	env := newTestEnv(t)
	env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	taskID, token := env.claim(t, "w1")

	ack, err := env.o.AppendLog(ctx(), taskID, token, LogSegment{Offset: 0, Data: "hello "})
	if err != nil {
		t.Fatalf("AppendLog: %v", err)
	}
	if ack.Offset != 6 || ack.Cancelled {
		t.Errorf("ack = %+v, want offset 6 cancelled false", ack)
	}
	ack, err = env.o.AppendLog(ctx(), taskID, token, LogSegment{Offset: 6, Data: "world"})
	if err != nil {
		t.Fatalf("AppendLog: %v", err)
	}
	if ack.Offset != 11 {
		t.Errorf("ack offset = %d, want 11", ack.Offset)
	}

	// Mismatched offset → ErrConflict with the current offset.
	_, err = env.o.AppendLog(ctx(), taskID, token, LogSegment{Offset: 3, Data: "x"})
	var offErr *OffsetError
	if !errors.As(err, &offErr) || !errors.Is(err, ErrConflict) {
		t.Fatalf("AppendLog mismatch = %v, want OffsetError(ErrConflict)", err)
	}
	if offErr.Current != 11 {
		t.Errorf("reported offset = %d, want 11", offErr.Current)
	}
	// Bad token.
	if _, err := env.o.AppendLog(ctx(), taskID, "nope", LogSegment{Offset: 11}); !errors.Is(err, ErrForbidden) {
		t.Errorf("AppendLog bad token = %v, want ErrForbidden", err)
	}

	// The log file landed on disk under the build id.
	task, err := env.store.GetTask(ctx(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	data, err := env.logs.Read(task.BuildID)
	if err != nil {
		t.Fatalf("logs.Read: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("log = %q, want %q", data, "hello world")
	}
}

// TestAppendLogCancelFlag covers the log cancellation signal: after a
// cancel request the log ack reports cancelled=true.
func TestAppendLogCancelFlag(t *testing.T) {
	env := newTestEnv(t)
	env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	taskID, token := env.claim(t, "w1")

	if err := env.o.CancelTask(ctx(), taskID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	ack, err := env.o.AppendLog(ctx(), taskID, token, LogSegment{Offset: 0, Data: "bye"})
	if err != nil {
		t.Fatalf("AppendLog: %v", err)
	}
	if !ack.Cancelled {
		t.Error("cancelled flag not set on log ack")
	}
}

// TestAppendLogProgress covers the one-shot progress channel: progress in a
// log segment refreshes last_progress_at and appends a resource sample.
func TestAppendLogProgress(t *testing.T) {
	env := newTestEnv(t)
	env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	taskID, token := env.claim(t, "w1")

	env.advance(time.Minute)
	_, err := env.o.AppendLog(ctx(), taskID, token, LogSegment{
		Offset: 0,
		Data:   "==> building",
		Progress: &TaskProgress{
			TaskID:      taskID,
			Stage:       "makepkg",
			CPUTimeNS:   999,
			MemoryBytes: 888,
			At:          env.now,
		},
	})
	if err != nil {
		t.Fatalf("AppendLog: %v", err)
	}
	task, err := env.store.GetTask(ctx(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !task.LastProgressAt.After(env.now.Add(-time.Minute)) {
		t.Errorf("last_progress_at not refreshed: %v", task.LastProgressAt)
	}
	build, err := env.store.GetBuild(ctx(), task.BuildID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if len(build.ResourceUsage) != 1 || build.ResourceUsage[0].CPUTimeNS != 999 {
		t.Errorf("samples = %+v, want one sample with cpu 999", build.ResourceUsage)
	}
}

// TestTaskDetailCarriesLimits asserts the configured worker cpu/memory
// limits are dispatched onto the task detail so the host can hand them
// to the container runtime.
func TestTaskDetailCarriesLimits(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Worker.CPULimit = 4
	env.cfg.Worker.MemoryLimit = "8GiB"
	env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	taskID, token := env.claim(t, "w1")

	detail, err := env.o.GetTask(ctx(), taskID, token)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if detail.Build.CPULimit != 4 {
		t.Errorf("cpu limit = %d, want 4", detail.Build.CPULimit)
	}
	if detail.Build.MemoryLimit != "8GiB" {
		t.Errorf("memory limit = %q, want 8GiB", detail.Build.MemoryLimit)
	}
}

// TestTaskDetailPackager asserts the configured worker.packager identity
// is carried onto the dispatched task detail (and thus into the agent's
// build environment).
func TestTaskDetailPackager(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Worker.Packager = "Jane Packager <jane@example.org>"
	env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	taskID, token := env.claim(t, "w1")

	detail, err := env.o.GetTask(ctx(), taskID, token)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if detail.Packager != "Jane Packager <jane@example.org>" {
		t.Errorf("packager = %q, want the configured identity", detail.Packager)
	}
}

// TestAppendLogTerminalBudget covers the post-terminal log policy: the
// task grants exactly one segment after becoming terminal (the final
// drain of the normal flow), then rejects every further append with
// ErrConflict so a token holder cannot grow a terminal task's log
// without bound.
func TestAppendLogTerminalBudget(t *testing.T) {
	env := newTestEnv(t)
	taskID := env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, token := env.claim(t, "w1")
	if claimed != taskID {
		t.Fatalf("claimed %s", claimed)
	}
	if err := env.store.WithTx(ctx(), func(tx *db.Tx) error {
		return tx.FinalizeTask(ctx(), claimed, "succeeded", "", env.now.UTC(), nil, nil)
	}); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	ack, err := env.o.AppendLog(ctx(), claimed, token, LogSegment{Offset: 0, Data: "tail"})
	if err != nil {
		t.Fatalf("first post-terminal segment: %v", err)
	}
	if ack.Offset != 4 {
		t.Errorf("ack offset = %d, want 4", ack.Offset)
	}
	if _, err := env.o.AppendLog(ctx(), claimed, token, LogSegment{Offset: 4, Data: "more"}); !errors.Is(err, ErrConflict) {
		t.Errorf("second post-terminal segment = %v, want ErrConflict", err)
	}
}
