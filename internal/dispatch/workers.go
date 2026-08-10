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
	"fmt"
	"log"

	"git.0x0f.dev/varve/internal/db"
)

// Register upserts a worker keyed by its stable name, status=online with
// a fresh heartbeat, and returns its id and name. Concurrently safe.
func (o *OrchestratorImpl) Register(ctx context.Context, reg RegisterReq) (*RegisterResp, error) {
	if reg.Name == "" {
		return nil, fmt.Errorf("dispatch: register: empty worker name")
	}
	now := o.now().UTC()
	w := &db.Worker{
		Name:          reg.Name,
		Role:          reg.Role,
		Mode:          reg.Mode,
		Arch:          reg.Arch,
		Capacity:      reg.Capacity,
		Status:        "online",
		LastHeartbeat: &now,
		Version:       reg.Version,
	}
	if err := o.store.RegisterWorker(ctx, w); err != nil {
		return nil, fmt.Errorf("dispatch: register %q: %w", reg.Name, err)
	}
	return &RegisterResp{ID: w.ID, Name: w.Name}, nil
}

// Heartbeat refreshes the node's heartbeat, applies the reported task
// progress (TouchTaskProgress + resource samples) and returns the
// cancellation signals of this node's active tasks, always read from the
// database. Metrics and containers are informational and are not
// persisted. A disabled worker keeps its status (db.Heartbeat would reset
// it to online), so its heartbeat only refreshes the last-seen time.
// Concurrently safe.
func (o *OrchestratorImpl) Heartbeat(ctx context.Context, hb HeartbeatReq) (*HeartbeatResp, error) {
	w, err := o.store.GetWorkerByName(ctx, hb.Name)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	now := o.now().UTC()
	if w.Status != "disabled" {
		if err := o.store.Heartbeat(ctx, hb.Name, now); err != nil {
			return nil, err
		}
	}
	for i := range hb.Tasks {
		// Progress may only be reported by the worker that owns the
		// task: otherwise any registered node could keep another
		// worker's task alive (stall recovery never fires) and inject
		// resource samples into its build.
		if _, err := o.checkProgressOwner(ctx, w.ID, hb.Tasks[i].TaskID); err != nil {
			log.Printf("dispatch: heartbeat %s: progress: %v", hb.Name, err)
			continue
		}
		if err := o.processProgress(ctx, hb.Tasks[i]); err != nil {
			log.Printf("dispatch: heartbeat %s: progress: %v", hb.Name, err)
		}
	}
	cancelled, err := o.cancelledTaskIDs(ctx, w.ID)
	if err != nil {
		return nil, err
	}
	return &HeartbeatResp{CancelledTaskIDs: cancelled, ServerTime: now}, nil
}

// Poll doubles as a heartbeat and atomically claims the FIFO head task
// for the node within its free capacity. The claimed task is returned
// together with a fresh 32-byte hex claim token (persisted by the claim
// transaction and mirrored in memory) and the cancellation signals. No
// claimable task yields task=null, not an error. Workers that are not online (disabled/offline) are never
// assigned new work; a disabled worker's heartbeat is not refreshed
// either, so the admin's disable state survives its polls (db.Heartbeat
// would reset status to online). Concurrently safe.
func (o *OrchestratorImpl) Poll(ctx context.Context, poll PollReq) (*PollResp, error) {
	w, err := o.store.GetWorkerByName(ctx, poll.Name)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	now := o.now().UTC()

	var claimed *db.Task
	token := ""
	if w.Status == "online" {
		if err := o.store.Heartbeat(ctx, w.Name, now); err != nil {
			return nil, err
		}
		if token, err = randomToken(); err != nil {
			return nil, err
		}
		claimed, err = o.store.ClaimTask(ctx, w.ID, w.Capacity, token)
		if err != nil && !errors.Is(err, db.ErrNoTask) {
			return nil, err
		}
		if claimed != nil {
			o.cacheToken(claimed.ID, token)
		} else {
			token = ""
		}
	}

	cancelled, err := o.cancelledTaskIDs(ctx, w.ID)
	if err != nil {
		return nil, err
	}

	resp := &PollResp{ClaimToken: token, CancelledTaskIDs: cancelled}
	if claimed != nil {
		detail, err := o.taskDetail(ctx, claimed)
		if err != nil {
			return nil, fmt.Errorf("dispatch: poll: build task detail %s: %w", claimed.ID, err)
		}
		resp.Task = detail
	}
	return resp, nil
}

// Deregister removes the node record: a worker exiting normally deletes
// itself. The builds it executed keep the node's display name as plain
// text (worker_name), so history survives the deletion. A node with
// active tasks cannot deregister: the caller must drain them first
// (ErrConflict). A node that dies without deregistering is marked offline
// by the heartbeat scan and deleted after 24h when it is an agent.
// Concurrently safe.
func (o *OrchestratorImpl) Deregister(ctx context.Context, name string) error {
	w, err := o.store.GetWorkerByName(ctx, name)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	active, err := o.activeTasksOf(ctx, w.ID)
	if err != nil {
		return err
	}
	if active > 0 {
		return fmt.Errorf("%w: worker %q has %d active tasks; drain them before deregistering", ErrConflict, name, active)
	}
	return o.store.DeleteWorker(ctx, name)
}

// checkProgressOwner resolves the task of a progress report and verifies
// it belongs to the reporting worker. The heartbeat path passes its own
// worker id; the log-segment progress path passes 0 because its identity
// is the claim token, which already binds the caller to the task.
func (o *OrchestratorImpl) checkProgressOwner(ctx context.Context, workerID int64, taskID string) (*db.Task, error) {
	if taskID == "" {
		return nil, fmt.Errorf("dispatch: progress without task id")
	}
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task.WorkerID != workerID {
		return nil, fmt.Errorf("dispatch: task %s belongs to worker %d, not %d", taskID, task.WorkerID, workerID)
	}
	return task, nil
}

// processProgress applies one heartbeat/log progress report: it refreshes
// the task's last_progress_at and appends the cgroup sample to its build
// (deduplicated by timestamp inside the store). Only assigned/running
// tasks accept progress: a report on a terminal or queued task is an
// error and touches nothing (a terminal task's stall state is
// intentionally not refreshable).
func (o *OrchestratorImpl) processProgress(ctx context.Context, p TaskProgress) error {
	task, err := o.store.GetTask(ctx, p.TaskID)
	if err != nil {
		return err
	}
	if task.State != "assigned" && task.State != "running" {
		return fmt.Errorf("dispatch: progress on task %s in state %q ignored", p.TaskID, task.State)
	}
	if err := o.store.TouchTaskProgress(ctx, p.TaskID, o.now().UTC()); err != nil {
		return err
	}
	sample := db.Sample{
		At:                 p.At,
		CPUTimeNS:          p.CPUTimeNS,
		MemoryBytes:        p.MemoryBytes,
		DiskTotalBytes:     p.DiskTotalBytes,
		DiskAvailableBytes: p.DiskAvailableBytes,
		DiskUsedBytes:      p.DiskUsedBytes,
	}
	if err := o.store.AppendResourceSamples(ctx, task.BuildID, []db.Sample{sample}); err != nil {
		return err
	}
	return nil
}

// activeTasksOf counts the assigned/running tasks of a worker.
func (o *OrchestratorImpl) activeTasksOf(ctx context.Context, workerID int64) (int, error) {
	tasks, err := o.store.ListTasksByWorker(ctx, workerID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range tasks {
		if t.State == "assigned" || t.State == "running" {
			n++
		}
	}
	return n, nil
}
