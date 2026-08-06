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
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/storage"
)

// runScheduler is the single periodic goroutine: a 30s scan for
// stalled/timed-out/cancelled tasks and an hourly maintenance pass for
// log retention, node cleanup and stale staging sweep. It exits when its
// context is cancelled (Stop). Tests invoke scanStalled and
// hourlyMaintenance directly with an injected clock.
func (o *OrchestratorImpl) runScheduler(ctx context.Context) {
	defer close(o.schedDone)
	stall := time.NewTicker(o.stallInterval)
	defer stall.Stop()
	hourly := time.NewTicker(o.hourlyInterval)
	defer hourly.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stall.C:
			if err := o.scanStalled(ctx); err != nil {
				log.Printf("dispatch: scheduler scan: %v", err)
			}
			o.autoscaleWorkers(ctx)
		case <-hourly.C:
			if err := o.hourlyMaintenance(ctx); err != nil {
				log.Printf("dispatch: hourly maintenance: %v", err)
			}
		}
	}
}

// scanStalled runs the 30s sweep:
//
//  1. stalled tasks (last_progress_at older than stall_timeout): tasks with
//     a durable cancel request are finalized as cancelled (cancellation
//     wins and is never re-queued); first-time stalls (attempts < 1) are
//     re-queued with created_at preserved; otherwise the task fails with
//     stage "stalled" and a notification;
//  2. timed-out tasks (assigned_at + build_timeout past): failed with stage
//     "timeout" and a notification (cancelled requests still win). The
//     executing agent kills makepkg at its own deadline; this controller
//     pass is the safety net.
//
// Concurrency: runs on the single scheduler goroutine; finalization races
// with agent reports are absorbed (ErrConflict is tolerated).
func (o *OrchestratorImpl) scanStalled(ctx context.Context) error {
	now := o.now().UTC()

	// 1. Stall recovery.
	stalled, err := o.store.ListStalledTasks(ctx, now.Add(-o.cfg.Worker.StallTimeout), "assigned", "running")
	if err != nil {
		return err
	}
	for i := range stalled {
		t := &stalled[i]
		switch {
		case t.CancelRequested:
			// Cancellation wins over recovery.
			o.finalizeCancelled(ctx, t)
		case t.Attempts < 1:
			if err := o.store.RequeueTask(ctx, t.ID); err != nil {
				if !errors.Is(err, db.ErrConflict) && !errors.Is(err, db.ErrNotFound) {
					log.Printf("dispatch: requeue stalled task %s: %v", t.ID, err)
				}
				continue
			}
			o.clearToken(t.ID) // the stale container's token dies
			log.Printf("dispatch: stalled task %s re-queued (attempt 2)", t.ID)
		default:
			o.finalizeFailed(ctx, t, "stalled", "no progress for "+o.cfg.Worker.StallTimeout.String())
		}
	}

	// 2. Timeout.
	timedOut, err := o.store.ListTimedOutTasks(ctx, now.Add(-o.cfg.Worker.BuildTimeout))
	if err != nil {
		return err
	}
	for i := range timedOut {
		t := &timedOut[i]
		if isTerminal(t.State) {
			continue // handled above (e.g. stalled) or by an agent report
		}
		if t.CancelRequested {
			o.finalizeCancelled(ctx, t)
			continue
		}
		o.finalizeFailed(ctx, t, "timeout", "build deadline exceeded ("+o.cfg.Worker.BuildTimeout.String()+")")
	}
	return nil
}

// finalizeCancelled finalizes a cancelled task (no notification).
func (o *OrchestratorImpl) finalizeCancelled(ctx context.Context, t *db.Task) {
	if err := o.finalizeTask(ctx, t.ID, "cancelled", "", nil, nil); err != nil {
		if !errors.Is(err, ErrConflict) {
			log.Printf("dispatch: finalize cancelled %s: %v", t.ID, err)
		}
		return
	}
	o.clearSigner(t.ID)
}

// finalizeFailed finalizes a failed task, notifies the maintainers and
// cleans the staging area.
func (o *OrchestratorImpl) finalizeFailed(ctx context.Context, t *db.Task, stage, summary string) {
	if err := o.finalizeTask(ctx, t.ID, "failed", stage+": "+summary, nil, nil); err != nil {
		if !errors.Is(err, ErrConflict) {
			log.Printf("dispatch: finalize failed %s: %v", t.ID, err)
		}
		return
	}
	build, err := o.store.GetBuild(ctx, t.BuildID)
	if err != nil {
		log.Printf("dispatch: read build %d for notification: %v", t.BuildID, err)
		return
	}
	o.notifyFailure(ctx, t, build, stage, summary)
	o.clearSigner(t.ID)
}

// hourlyMaintenance runs the hourly pass: successful logs are rolled by
// retention and max_builds, stale agents are removed, and staging
// directories older than 24h are swept.
func (o *OrchestratorImpl) hourlyMaintenance(ctx context.Context) error {
	o.sweepLogs(ctx)
	o.sweepWorkers(ctx)
	o.sweepStaging(ctx)
	return nil
}

// sweepLogs deletes logs of succeeded builds that exceed the retention
// window or the max_builds cap (newest kept); failed/cancelled logs are
// permanent. Deletion is by build id; missing logs are tolerated.
func (o *OrchestratorImpl) sweepLogs(ctx context.Context) {
	if o.logs == nil {
		return
	}
	all, _, err := o.store.ListBuilds(ctx, 1, maxScanBuilds, false)
	if err != nil {
		log.Printf("dispatch: log sweep: list builds: %v", err)
		return
	}
	now := o.now().UTC()
	kept := 0
	for _, b := range all { // newest first
		if b.Status != "succeeded" || b.FinishedAt == nil {
			continue
		}
		kept++
		tooOld := now.Sub(*b.FinishedAt) > o.cfg.Logs.Retention
		tooMany := kept > o.cfg.Logs.MaxBuilds
		if tooOld || tooMany {
			if err := o.logs.Delete(strconv.FormatInt(b.ID, 10)); err != nil {
				log.Printf("dispatch: log sweep: delete build %d: %v", b.ID, err)
			}
		}
	}
}

// sweepWorkers marks nodes whose heartbeat is stale (heartbeat_timeout)
// as offline, then deletes agent nodes offline for more than 24h. Host
// nodes are never auto-deleted. A worker referenced by build history
// cannot be deleted (foreign key); the sweep logs and skips it.
func (o *OrchestratorImpl) sweepWorkers(ctx context.Context) {
	workers, err := o.store.ListWorkers(ctx)
	if err != nil {
		log.Printf("dispatch: worker sweep: list: %v", err)
		return
	}
	now := o.now().UTC()
	for i := range workers {
		w := &workers[i]
		if w.Status == "disabled" {
			continue
		}
		if w.LastHeartbeat != nil && now.Sub(*w.LastHeartbeat) > o.cfg.Worker.HeartbeatTimeout && w.Status != "offline" {
			if err := o.store.SetWorkerStatus(ctx, w.Name, "offline"); err != nil {
				log.Printf("dispatch: worker sweep: mark %s offline: %v", w.Name, err)
			}
			w.Status = "offline"
		}
		if w.Role == "agent" && w.Status == "offline" &&
			w.LastHeartbeat != nil && now.Sub(*w.LastHeartbeat) > 24*time.Hour {
			if err := o.store.DeleteWorker(ctx, w.Name); err != nil {
				log.Printf("dispatch: worker sweep: delete %s: %v (skipped; likely referenced by history)", w.Name, err)
			}
		}
	}
}

// sweepStaging removes staging directories older than 24h (residue from
// failed ingests and crashes; the ingest path preserves staging on purpose
// but only until a retry or this sweep). The local backend is enumerated
// through the filesystem; on s3 the sweep is skipped — object-store residue
// is bounded by operator-side lifecycle rules.
func (o *OrchestratorImpl) sweepStaging(ctx context.Context) {
	if o.cfg.Storage.Backend != "local" {
		log.Printf("dispatch: staging sweep skipped for backend %q", o.cfg.Storage.Backend)
		return
	}
	stagingRoot := filepath.Join(o.cfg.Storage.Local.Root, "staging")
	entries, err := os.ReadDir(stagingRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		log.Printf("dispatch: staging sweep: read %s: %v", stagingRoot, err)
		return
	}
	now := o.now().UTC()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) <= 24*time.Hour {
			continue
		}
		taskID := e.Name()
		dir := filepath.Join(stagingRoot, taskID)
		files, err := os.ReadDir(dir)
		if err != nil {
			log.Printf("dispatch: staging sweep: read %s: %v", dir, err)
			continue
		}
		for _, f := range files {
			if err := o.storage.Delete(ctx, storage.StagingPath(taskID, f.Name())); err != nil {
				log.Printf("dispatch: staging sweep: delete %s/%s: %v", taskID, f.Name(), err)
			}
		}
		if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("dispatch: staging sweep: remove %s: %v", dir, err)
		}
	}
}
