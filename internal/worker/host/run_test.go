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
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/api"
)

// TestRegisterRequestShape verifies the registration payload: role=host,
// mode=host, capacity and the stable name.
func TestRegisterRequestShape(t *testing.T) {
	cfg := testCfg()
	cfg.Concurrency = 3
	c := newFakeClient()
	r := testRunner(t, cfg, c, newFakeRuntime())

	if err := r.register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	req := c.registerCall(0)
	if req.Name != "test-node" || req.Role != "host" || req.Mode != "host" ||
		req.Capacity != 3 || req.Arch != "" || req.Version != version {
		t.Errorf("register req = %+v, want host/host/capacity 3", req)
	}
}

// TestRegisterBackoffRetry verifies registration retries with exponential
// backoff until success.
func TestRegisterBackoffRetry(t *testing.T) {
	c := newFakeClient()
	c.registerErrs = 3 // fail the first three attempts
	r := testRunner(t, nil, c, newFakeRuntime())

	if err := r.register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := c.registerCount(); got != 4 {
		t.Errorf("register attempts = %d, want 4 (3 failures + success)", got)
	}
}

// TestRegisterCancelledDuringBackoff verifies register aborts with the
// context error when cancelled between retries.
func TestRegisterCancelledDuringBackoff(t *testing.T) {
	c := newFakeClient()
	c.registerErrs = 100
	r := testRunner(t, nil, c, newFakeRuntime())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := r.register(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("register err = %v, want context.Canceled", err)
	}
}

// TestRunShutdownDrainsAndDeregisters covers the graceful exit: on ctx
// cancellation the runner stops claiming new tasks, waits for the running
// container, then deregisters and returns nil.
func TestRunShutdownDrainsAndDeregisters(t *testing.T) {
	rt := newFakeRuntime()
	rt.blocked = make(chan struct{}) // the container stays alive until released
	c := newFakeClient()
	c.pollRepeat = &api.PollResp{Task: testTask("t1"), ClaimToken: "tok-1"}
	r := testRunner(t, nil, c, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// A container starts and the poller parks behind the capacity slot.
	waitFor(t, 3*time.Second, func() bool { return rt.runCount() >= 1 })
	time.Sleep(200 * time.Millisecond)
	pollsAtCancel := c.pollCount()

	cancel()
	time.Sleep(300 * time.Millisecond)
	snap1 := c.pollCount()
	time.Sleep(300 * time.Millisecond)
	snap2 := c.pollCount()
	if snap2 != snap1 {
		t.Errorf("polling continued after shutdown: %d then %d", snap1, snap2)
	}
	if snap1 > pollsAtCancel+1 {
		t.Errorf("poll count grew after cancel (%d → %d), want at most +1 in flight", pollsAtCancel, snap1)
	}

	// Releasing the container lets the monitor finish the drain.
	close(rt.blocked)
	waitFor(t, 3*time.Second, func() bool { return c.deregisterCount() >= 1 })
	if got := c.deregisterCount(); got != 1 {
		t.Fatalf("deregisters = %d, want exactly 1", got)
	}
	if c.deregisters[0] != "test-node" {
		t.Errorf("deregistered name = %q, want test-node", c.deregisters[0])
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the drain")
	}
}

// TestRunRegisterFailsPermanently verifies Run returns the error when
// registration never succeeds and the context is cancelled.
func TestRunRegisterFailsPermanently(t *testing.T) {
	c := newFakeClient()
	c.registerErrs = 100
	r := testRunner(t, nil, c, newFakeRuntime())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run err = %v, want context.Canceled", err)
	}
	if c.deregisterCount() != 0 {
		t.Errorf("deregisters = %d, want 0 (never registered)", c.deregisterCount())
	}
}
