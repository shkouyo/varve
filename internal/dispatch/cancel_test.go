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
	"errors"
	"testing"
)

// TestCancelQueued covers the immediate cancellation of a queued task: it
// becomes terminal cancelled and any staged archive is cleaned up.
func TestCancelQueued(t *testing.T) {
	env := newTestEnv(t)
	taskID := env.enqueue(t, "foo", "foo")
	if err := env.o.CancelTask(ctx(), taskID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	task, err := env.store.GetTask(ctx(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "cancelled" {
		t.Errorf("state = %q, want cancelled", task.State)
	}
	if task.CancelRequested {
		t.Error("queued cancel should not persist cancel_requested")
	}
	// Cancelling a terminal task is a no-op.
	if err := env.o.CancelTask(ctx(), taskID); err != nil {
		t.Errorf("CancelTask(terminal) = %v, want nil", err)
	}
	if err := env.o.CancelTask(ctx(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("CancelTask(unknown) = %v, want ErrNotFound", err)
	}
}

// TestCancelRunningPersists covers the assigned/running path: the durable
// cancel_requested flag is persisted and the worker learns about it through
// both channels.
func TestCancelRunningPersists(t *testing.T) {
	env := newTestEnv(t)
	taskID := env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, token := env.claim(t, "w1")
	if claimed != taskID {
		t.Fatalf("claimed %s", claimed)
	}

	if err := env.o.CancelTask(ctx(), claimed); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	task, err := env.store.GetTask(ctx(), claimed)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !task.CancelRequested || task.State != "assigned" {
		t.Errorf("task = state %q cancel_requested %v, want assigned + persisted flag", task.State, task.CancelRequested)
	}
	// Channel 2: the log ack reports cancelled.
	ack, err := env.o.AppendLog(ctx(), claimed, token, LogSegment{Offset: 0, Data: "x"})
	if err != nil {
		t.Fatalf("AppendLog: %v", err)
	}
	if !ack.Cancelled {
		t.Error("log ack did not carry the cancel signal")
	}
	// Channel 1: the heartbeat carries it (per-worker list).
	hb, err := env.o.Heartbeat(ctx(), HeartbeatReq{Name: "w1"})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if len(hb.CancelledTaskIDs) != 1 || hb.CancelledTaskIDs[0] != claimed {
		t.Errorf("heartbeat cancels = %v, want [%s]", hb.CancelledTaskIDs, claimed)
	}
}
