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
	"log"
	"time"

	"git.0x0f.dev/varve/internal/api"
)

// heartbeatLoop sends heartbeats every heartbeatInterval (30s) until ctx
// is cancelled.
func (r *Runner) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(r.heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.heartbeat(ctx)
		}
	}
}

// heartbeat sends one heartbeat: system metrics + tasks: [] (the host has
// no own task progress; the in-container agent reports progress via log
// segments) + the running containers. The response's cancelled_task_ids
// kill the matching containers (channel 1); a 401/404 triggers
// re-registration (idempotent upsert).
func (r *Runner) heartbeat(ctx context.Context) {
	resp, err := r.client.Heartbeat(ctx, api.HeartbeatReq{
		Name:       r.name,
		Metrics:    r.metrics.sample(),
		Tasks:      []api.TaskProgress{},
		Containers: r.containerStates(),
	})
	if err != nil {
		if needsReregister(err) {
			r.reregister(ctx)
		} else {
			log.Printf("host: heartbeat: %v", err)
		}
		return
	}
	if len(resp.CancelledTaskIDs) > 0 {
		r.cancelTasks(ctx, resp.CancelledTaskIDs)
	}
}

// cancelTasks kills the containers of the given tasks (cancellation
// channel 1): each one is flagged cancelled so the monitor reports
// cancelled instead of failed.
func (r *Runner) cancelTasks(ctx context.Context, taskIDs []string) {
	var ids []string
	r.mu.Lock()
	for _, taskID := range taskIDs {
		if run, ok := r.containers[taskID]; ok {
			run.cancelled = true
			ids = append(ids, run.id)
		}
	}
	r.mu.Unlock()
	for _, id := range ids {
		if err := r.rt.Kill(context.WithoutCancel(ctx), id); err != nil {
			log.Printf("host: kill container %s (cancel): %v", id, err)
		}
	}
}

// containerStates snapshots the running containers for the heartbeat
// payload.
func (r *Runner) containerStates() []api.ContainerState {
	r.mu.Lock()
	defer r.mu.Unlock()
	states := make([]api.ContainerState, 0, len(r.containers))
	for _, run := range r.containers {
		states = append(states, api.ContainerState{
			TaskID:   run.taskID,
			Status:   "running",
			ExitCode: nil,
		})
	}
	return states
}

// track records a running container by task ID.
func (r *Runner) track(taskID, containerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.containers[taskID] = &containerRun{taskID: taskID, id: containerID}
}

// untrack forgets a finished container.
func (r *Runner) untrack(taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.containers, taskID)
}

// isCancelled reports whether the task's container was killed by a
// cancellation signal.
func (r *Runner) isCancelled(taskID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.containers[taskID]
	return ok && run.cancelled
}
