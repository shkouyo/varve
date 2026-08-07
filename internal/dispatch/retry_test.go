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
	"testing"
)

// reportFailed reports a failed result for a claimed task.
func reportFailed(t *testing.T, env *testEnv, taskID, token string) {
	t.Helper()
	if err := env.o.ReportResult(context.Background(), taskID, token, ResultReq{
		Status: "failed",
		Error:  &ResultError{Stage: "makepkg", Summary: "boom"},
	}); err != nil {
		t.Fatalf("ReportResult failed: %v", err)
	}
}

// TestRetryBudget covers the retry policy: agent-reported build failures
// re-queue the same task (same version and source) up to retry_max times,
// the fail counter counts, and the next failure reaches the failed
// terminal state with the package cooldown marker.
func TestRetryBudget(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Worker.RetryMax = 3
	taskID := env.enqueue(t, "foo", "foo", "maint@example.org")
	env.registerWorker(t, "w1", "host", "host", 1)

	firstTask, err := env.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	buildBefore, err := env.store.GetBuild(context.Background(), firstTask.BuildID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}

	for attempt := 1; attempt <= 3; attempt++ {
		claimed, token := env.claim(t, "w1")
		if claimed != taskID {
			t.Fatalf("attempt %d claimed %s, want %s", attempt, claimed, taskID)
		}
		reportFailed(t, env, claimed, token)

		task, err := env.store.GetTask(context.Background(), taskID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if task.State != "queued" {
			t.Fatalf("attempt %d state = %q, want queued (retry %d)", attempt, task.State, attempt)
		}
		if task.FailCount != attempt {
			t.Errorf("attempt %d fail_count = %d, want %d", attempt, task.FailCount, attempt)
		}
		if task.BuildID != buildBefore.ID {
			t.Errorf("attempt %d build id changed: %s -> %s (version/source must be preserved)", attempt, buildBefore.ID, task.BuildID)
		}
	}

	// Fourth failure: the retry budget is exhausted, the task is terminal.
	claimed, token := env.claim(t, "w1")
	if claimed != taskID {
		t.Fatalf("final claim %s, want %s", claimed, taskID)
	}
	reportFailed(t, env, claimed, token)

	task, err := env.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "failed" {
		t.Fatalf("final state = %q, want failed", task.State)
	}
	if task.FailCount != 3 {
		t.Errorf("fail_count = %d, want 3 at the terminal failure", task.FailCount)
	}
	build, err := env.store.GetBuild(context.Background(), task.BuildID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if build.Status != "failed" {
		t.Errorf("build status = %q, want failed", build.Status)
	}
	pkg, err := env.store.GetPackageByBase(context.Background(), "foo")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if pkg.LastFailedAt == nil {
		t.Error("package last_failed_at = nil, want the cooldown marker set at the terminal failure")
	}
	// Only the terminal failure notifies the maintainers.
	if len(env.not.calls) != 1 {
		t.Errorf("notifications = %+v, want exactly one (terminal failure)", env.not.calls)
	}
}

// TestRetryDisabledByZero covers retry_max=0: the first failure is
// terminal, matching the pre-retry behavior.
func TestRetryDisabledByZero(t *testing.T) {
	env := newTestEnv(t) // RetryMax defaults to 0 in the test harness
	taskID := env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, token := env.claim(t, "w1")
	if claimed != taskID {
		t.Fatalf("claimed %s", claimed)
	}
	reportFailed(t, env, claimed, token)
	task, err := env.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "failed" {
		t.Fatalf("state = %q, want failed with retry_max=0", task.State)
	}
}

// TestVerifyFailureNotRetried covers the non-retryable stages: a manifest
// verification failure is terminal on the first failure even with a retry
// budget configured, and stamps the cooldown marker.
func TestVerifyFailureNotRetried(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Worker.RetryMax = 3
	taskID := env.enqueue(t, "foo", "foo", "maint@example.org")
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, token := env.claim(t, "w1")
	if claimed != taskID {
		t.Fatalf("claimed %s", claimed)
	}
	// An empty manifest fails verification before any retry decision.
	if err := env.o.ReportResult(context.Background(), claimed, token,
		ResultReq{Status: "succeeded", Artifacts: nil}); err != nil {
		t.Fatalf("ReportResult: %v", err)
	}
	task, err := env.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "failed" {
		t.Fatalf("state = %q, want failed (verify failures stay terminal)", task.State)
	}
	if task.FailCount != 0 {
		t.Errorf("fail_count = %d, want 0 (no retry budget consumed)", task.FailCount)
	}
	pkg, err := env.store.GetPackageByBase(context.Background(), "foo")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if pkg.LastFailedAt == nil {
		t.Error("package last_failed_at = nil, want the cooldown marker on the verify failure")
	}
}

// TestSuccessClearsCooldown covers the success side of the marker: after a
// failed build stamps last_failed_at, a later successful build clears it.
func TestSuccessClearsCooldown(t *testing.T) {
	env := newTestEnv(t)
	_ = env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, token := env.claim(t, "w1")
	reportFailed(t, env, claimed, token) // terminal (retry_max=0), stamps the marker

	if err := env.o.Enqueue(context.Background(), detectChange("foo", "foo"), true); err != nil {
		t.Fatalf("Enqueue rebuild: %v", err)
	}
	rebuildID := env.activeTaskFor(t, "foo")
	artifacts := testArtifacts("foo", "1.0-1")
	for _, a := range artifacts {
		env.stage(t, rebuildID, a.File)
	}
	claimed, token = env.claim(t, "w1")
	if claimed != rebuildID {
		t.Fatalf("claimed %s, want %s", claimed, rebuildID)
	}
	if err := env.o.ReportResult(context.Background(), claimed, token,
		ResultReq{Status: "succeeded", Commit: "deadbeef", Artifacts: artifacts}); err != nil {
		t.Fatalf("ReportResult succeeded: %v", err)
	}
	pkg, err := env.store.GetPackageByBase(context.Background(), "foo")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if pkg.LastFailedAt != nil {
		t.Errorf("last_failed_at = %v, want nil after a successful build", pkg.LastFailedAt)
	}
}
