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
	"sync"
	"testing"
	"time"
)

// TestStopBoundsSchedulerHang covers the Stop safety net: a scheduler
// that never halts (a pass wedged in an external call) makes Stop return
// after stopTimeout instead of blocking the shutdown forever.
func TestStopBoundsSchedulerHang(t *testing.T) {
	o := &OrchestratorImpl{
		stopOnce:    sync.Once{},
		schedCancel: func() {},
		schedDone:   make(chan struct{}), // never closed
	}
	old := stopTimeout
	stopTimeout = 50 * time.Millisecond
	defer func() { stopTimeout = old }()

	done := make(chan struct{})
	go func() {
		o.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the scheduler failed to halt")
	}
}

// TestStopDrainsIngest covers the graceful shutdown contract: Stop blocks
// while an ingest orchestration is in flight and returns once it drains.
func TestStopDrainsIngest(t *testing.T) {
	env := newTestEnv(t)
	artifacts := testArtifacts("foo", "1.0-1")
	taskID := env.enqueue(t, "foo", "foo")
	for _, a := range artifacts {
		env.stage(t, taskID, a.File)
	}
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, token := env.claim(t, "w1")

	env.up.entered = make(chan struct{})
	env.up.block = make(chan struct{})

	var reportErr error
	reportDone := make(chan struct{})
	go func() {
		reportErr = env.o.ReportResult(context.Background(), claimed, token,
			ResultReq{Status: "succeeded", Artifacts: artifacts})
		close(reportDone)
	}()

	// Wait until the ingest is inside the updater, then request Stop.
	select {
	case <-env.up.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("ingest never started")
	}
	stopDone := make(chan struct{})
	go func() {
		env.o.Stop()
		close(stopDone)
	}()

	// Stop must not return while the ingest is still in flight.
	select {
	case <-stopDone:
		t.Fatal("Stop returned before the ingest drained")
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked
	}

	close(env.up.block) // release the ingest
	select {
	case <-reportDone:
	case <-time.After(5 * time.Second):
		t.Fatal("report did not finish after releasing the ingest")
	}
	if reportErr != nil {
		t.Fatalf("report: %v", reportErr)
	}
	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the ingest drained")
	}
	// A second Stop is safe (idempotent).
	env.o.Stop()
}
