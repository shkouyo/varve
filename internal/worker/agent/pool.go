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

package agent

import (
	"context"
	"log"
	"time"

	"git.0x0f.dev/varve/internal/api"
)

// runPool implements the pool lifecycle: register as a capacity-1 agent
// node (name from VARVE_WORKER_NAME or freshly auto-generated on every
// run), then loop polling every pollInterval. Claimed tasks are executed in
// place; the node deregisters and exits after PoolIdleTimeout of idleness
// (measured from startup or the end of the last task) or when the context
// is cancelled.
func (r *Runner) runPool(ctx context.Context) error {
	name := r.cfg.WorkerName
	if name == "" {
		name = generateName()
	}
	log.Printf("agent: pool node name %s", name)
	if err := r.registerWithBackoff(ctx, name); err != nil {
		return err
	}

	// Heartbeat goroutine: every heartbeatInterval, send system metrics +
	// the running task's progress (containers always empty) and deliver
	// cancellation signals (channel 1).
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go r.heartbeatLoop(hbCtx, name)

	idleSince := r.now()
	for {
		resp, err := r.client.Poll(ctx, api.PollReq{Name: name, Arch: r.cfg.WorkerArch})
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if isTokenError(err) {
				r.reRegister(ctx, name, err)
			} else {
				log.Printf("agent: poll: %v", err)
			}
			if !sleep(ctx, r.pollInterval) {
				break
			}
			continue
		}
		for _, id := range resp.CancelledTaskIDs {
			r.state.cancelTask(id)
		}
		if resp.Task != nil {
			idleSince = r.now()
			r.executeTask(ctx, resp.Task, resp.ClaimToken)
			idleSince = r.now()
			continue
		}
		if r.now().Sub(idleSince) >= r.cfg.PoolIdleTimeout {
			r.deregister(name)
			return nil
		}
		if !sleep(ctx, r.pollInterval) {
			break
		}
	}
	// Context cancelled: deregister best-effort and exit.
	r.deregister(name)
	return nil
}

// registerWithBackoff registers the node, retrying with an exponential
// backoff (5s doubling to 60s) until success or context cancellation.
func (r *Runner) registerWithBackoff(ctx context.Context, name string) error {
	for attempt := 0; ; attempt++ {
		_, err := r.client.Register(ctx, api.RegisterReq{
			Name:     name,
			Role:     "agent",
			Mode:     "pool",
			Arch:     r.cfg.WorkerArch,
			Capacity: 1,
			Version:  version,
		})
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("agent: register %s: %v (attempt %d)", name, err, attempt+1)
		if !sleep(ctx, r.registerBackoff(attempt)) {
			return ctx.Err()
		}
	}
}

// reRegister re-establishes the node after an identity failure (401/404)
// on poll or heartbeat; the upsert is idempotent.
func (r *Runner) reRegister(ctx context.Context, name string, cause error) {
	log.Printf("agent: %s: %v; re-registering node", name, cause)
	if err := r.registerWithBackoff(ctx, name); err != nil && ctx.Err() == nil {
		log.Printf("agent: re-register %s: %v", name, err)
	}
}

// deregister removes the node on normal shutdown (the controller deletes
// the workers row; executed builds keep the display name as plain text).
func (r *Runner) deregister(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.client.Deregister(ctx, name); err != nil {
		log.Printf("agent: deregister %s: %v", name, err)
	}
}

// heartbeatLoop sends heartbeats on the configured interval.
func (r *Runner) heartbeatLoop(ctx context.Context, name string) {
	t := time.NewTicker(r.heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sendHeartbeat(ctx, name)
		}
	}
}

// sendHeartbeat sends one heartbeat: system metrics + the running task's
// progress and delivers the cancellation signal list (channel 1).
func (r *Runner) sendHeartbeat(ctx context.Context, name string) {
	metrics, counters := readMetrics(r.procDir, r.prevCPU)
	r.prevCPU = counters
	resp, err := r.client.Heartbeat(ctx, api.HeartbeatReq{
		Name:       name,
		Metrics:    metrics,
		Tasks:      r.state.heartbeatTasks(r.sampler),
		Containers: []api.ContainerState{},
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		if isTokenError(err) {
			r.reRegister(ctx, name, err)
		} else {
			log.Printf("agent: heartbeat: %v", err)
		}
		return
	}
	for _, id := range resp.CancelledTaskIDs {
		r.state.cancelTask(id)
	}
}

// sleep waits for d or until ctx is cancelled, reporting whether the
// interval elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
