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
	"log"
	"sync"
	"time"
)

// Run drives the host node: register with exponential backoff (5s → 60s
// cap) until success or ctx cancellation, start the capacity poll loops and
// the heartbeat loop, then on ctx cancellation (SIGTERM) stop claiming new
// tasks, drain the running containers (each monitor's per-task build
// timeout force-kills the stragglers), deregister (a normal exit always
// deregisters) and return nil.
func (r *Runner) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("host: Run requires a non-nil context")
	}
	if r.rt == nil || r.client == nil || r.name == "" {
		return errors.New("host: runner is not initialized (NewRunner failed)")
	}

	if err := r.register(ctx); err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	workers := effectiveConcurrency(r.cfg)
	var loops sync.WaitGroup
	loops.Add(1 + workers)
	go func() {
		defer loops.Done()
		r.heartbeatLoop(runCtx)
	}()
	for i := 0; i < workers; i++ {
		go func() {
			defer loops.Done()
			r.pollWorker(runCtx)
		}()
	}

	<-ctx.Done()
	cancel() // stop claiming new tasks; monitors keep draining
	loops.Wait()

	r.drain()
	r.deregister()
	return nil
}

// register registers the node, retrying with exponential backoff from
// registerBackoff up to registerBackoffMax until success or ctx
// cancellation.
func (r *Runner) register(ctx context.Context) error {
	backoff := r.registerBackoff
	for {
		if _, err := r.client.Register(ctx, r.registerReq()); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > r.registerBackoffMax {
			backoff = r.registerBackoffMax
		}
	}
}

// drain waits for the running containers to finish. Each monitor exits
// when its container exits or its per-task build timeout force-kills it,
// so the drain terminates within the container timeouts; the containers
// map empties as the monitors untrack them.
func (r *Runner) drain() {
	for {
		r.mu.Lock()
		n := len(r.containers)
		r.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(r.drainInterval)
	}
}

// deregister removes the node record. The context is detached from the
// cancelled shutdown context so the call can complete; a failure is logged
// and left to the controller's stale-heartbeat scan to mark the node
// offline.
func (r *Runner) deregister() {
	dctx, cancel := context.WithTimeout(context.Background(), r.deregisterTimeout)
	defer cancel()
	if err := r.client.Deregister(dctx, r.name); err != nil {
		log.Printf("host: deregister %s: %v (heartbeat scan will mark the node offline)", r.name, err)
	}
}
