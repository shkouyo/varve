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
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/sign"
)

// TestTypedNilSignerNoPanic reproduces the typed-nil signer bug: a typed
// nil *sign.Signer threaded through the signVerifier interface — exactly
// the shape cmd/varve/serve.go produced with repo.sign="off" — must be
// treated as "no signer" instead of crashing task finalization. Every
// terminal transition runs clearSigner: failed via the scheduler scan
// (scanStalled -> finalizeFailed -> clearSigner), succeeded via ingest,
// and cancelled via admin cancel; IssueSigningKey must report the
// disabled state. Before the fix each of these panicked with a nil
// pointer dereference in (*Signer).ClearTask.
func TestTypedNilSignerNoPanic(t *testing.T) {
	env := newTestEnv(t)
	env.o.Stop() // halt the fakeSigner-backed scheduler before replacing it

	// The production failure shape: a nil *sign.Signer stored in the
	// interface — the interface is non-nil, the concrete pointer is nil.
	var typedNil *sign.Signer
	env.o = NewOrchestrator(env.cfg, env.store, env.fs, typedNil, env.up, env.not, env.logs)
	env.o.now = func() time.Time { return env.now }
	env.o.execCommand = fakeGitFor(t, env.log, &gitState{Commit: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"})
	env.o.mirrorDir = "/data/source/fake.git"

	// The typed nil must be normalized to a true nil at construction, so
	// every later o.signer nil check (clearSigner, IssueSigningKey) is
	// sound.
	if env.o.signer != nil {
		t.Fatalf("signer = %#v, want nil (typed nil must be normalized)", env.o.signer)
	}

	env.registerWorker(t, "w1", "host", "host", 1)

	// -- failed terminalization: scanStalled -> finalizeFailed ->
	// clearSigner. The first stall re-queues (attempts 0 -> 1); the
	// second stall finalizes failed.
	failTask := env.enqueue(t, "fail", "fail", "maint@example.org")
	claimed, token := env.claim(t, "w1")
	if claimed != failTask {
		t.Fatalf("claimed %s, want %s", claimed, failTask)
	}
	// Signing is disabled: IssueSigningKey must report ErrConflict, not
	// call into the nil signer.
	if _, err := env.o.IssueSigningKey(context.Background(), claimed, token); !errors.Is(err, ErrConflict) {
		t.Fatalf("IssueSigningKey = %v, want ErrConflict (signing disabled)", err)
	}
	env.advance(env.cfg.Worker.StallTimeout + time.Minute)
	if err := env.o.scanStalled(context.Background()); err != nil {
		t.Fatalf("scanStalled (requeue): %v", err)
	}
	claimed, _ = env.claimAndStall(t, "w1")
	if claimed != failTask {
		t.Fatalf("re-claimed %s, want %s", claimed, failTask)
	}
	if err := env.o.scanStalled(context.Background()); err != nil {
		t.Fatalf("scanStalled (fail): %v", err)
	}
	if got := env.taskState(t, claimed); got != "failed" {
		t.Fatalf("state = %q, want failed", got)
	}

	// -- succeeded terminalization: ReportResult -> ingest -> clearSigner.
	okTask := env.enqueue(t, "ok", "ok")
	artifacts := testArtifacts("ok", "1.0-1")
	for _, a := range artifacts {
		env.stage(t, okTask, a.File)
	}
	claimed, token = env.claim(t, "w1")
	if claimed != okTask {
		t.Fatalf("claimed %s, want %s", claimed, okTask)
	}
	if err := env.reportSucceeded(t, claimed, token, artifacts, "deadbeef"); err != nil {
		t.Fatalf("ReportResult: %v", err)
	}
	if got := env.taskState(t, claimed); got != "succeeded" {
		t.Fatalf("state = %q, want succeeded", got)
	}

	// -- cancelled terminalization: admin cancel + scheduler finalize
	// (finalizeCancelled -> clearSigner; no notification).
	cancelTask := env.enqueue(t, "cancel", "cancel")
	claimed, _ = env.claim(t, "w1")
	if claimed != cancelTask {
		t.Fatalf("claimed %s, want %s", claimed, cancelTask)
	}
	if err := env.o.CancelTask(context.Background(), claimed); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	env.advance(env.cfg.Worker.StallTimeout + time.Minute)
	if err := env.o.scanStalled(context.Background()); err != nil {
		t.Fatalf("scanStalled (cancel): %v", err)
	}
	if got := env.taskState(t, claimed); got != "cancelled" {
		t.Fatalf("state = %q, want cancelled", got)
	}
	// The only notification is the stalled failure above; the cancelled
	// task must not have notified.
	if len(env.not.calls) != 1 || env.not.calls[0].Stage != "stalled" {
		t.Errorf("notifications = %+v, want exactly the stalled failure", env.not.calls)
	}
}

// taskState reads the current state of a task.
func (e *testEnv) taskState(t *testing.T, taskID string) string {
	t.Helper()
	task, err := e.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask %s: %v", taskID, err)
	}
	return task.State
}
