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
	"io"
	"sort"
	"strings"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/detect"
)

// RebuildPackage force-enqueues a rebuild of an existing package (admin,
// reason "manual"). The D6 name comparison is skipped (force) but the
// partial unique index still rejects a package with an active task.
// Concurrently safe.
func (o *OrchestratorImpl) RebuildPackage(ctx context.Context, pkgbase string) error {
	pkg, err := o.store.GetPackageByBase(ctx, pkgbase)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	c := detect.Change{
		Package: detect.Package{
			Pkgbase: pkg.Pkgbase,
			Branch:  pkg.Branch,
			VCSKind: pkg.VCSKind,
			Arch:    pkg.Arch,
		},
		Maintainers: pkg.Maintainers,
		UpstreamRef: pkg.LastUpstreamRef,
		Reason:      detect.ReasonManual,
	}
	return o.Enqueue(ctx, c, true)
}

// DisableWorker stops new work from being assigned to a node: its status
// becomes "disabled" and Poll refuses to claim for it (db.ClaimTask itself
// does not check status; the check lives here). The node keeps its history
// and may be re-enabled by re-registering. Concurrently safe.
func (o *OrchestratorImpl) DisableWorker(ctx context.Context, name string) error {
	if _, err := o.store.GetWorkerByName(ctx, name); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return o.store.SetWorkerStatus(ctx, name, "disabled")
}

// RemoveWorker deletes a node record. A node with active tasks cannot be
// removed (ErrConflict); a node referenced by build history fails the
// underlying foreign key and the error surfaces for the admin. Concurrently
// safe.
func (o *OrchestratorImpl) RemoveWorker(ctx context.Context, name string) error {
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
		return fmt.Errorf("%w: worker %q has %d active tasks", ErrConflict, name, active)
	}
	return o.store.DeleteWorker(ctx, name)
}

// Stats aggregates the dashboard data (DESIGN §2.4): queue length, build
// counts by status, the newest builds and the worker list. Concurrently
// safe.
func (o *OrchestratorImpl) Stats(ctx context.Context) (*Stats, error) {
	active, err := o.store.ListActiveTasks(ctx)
	if err != nil {
		return nil, err
	}
	queueLen := 0
	for _, t := range active {
		if t.State == "queued" {
			queueLen++
		}
	}
	all, _, err := o.store.ListBuilds(ctx, 1, maxScanBuilds, false)
	if err != nil {
		return nil, err
	}
	byStatus := make(map[string]int)
	for _, b := range all {
		byStatus[b.Status]++
	}
	recent := all
	if len(recent) > 20 {
		recent = recent[:20]
	}
	workers, err := o.store.ListWorkers(ctx)
	if err != nil {
		return nil, err
	}
	return &Stats{QueueLen: queueLen, ByStatus: byStatus, RecentBuilds: recent, Workers: workers}, nil
}

// ValidateConflicts scans the full packages and queue state for D6 name
// collisions: a pkgbase that equals a pkgname produced by the last build of
// another package. The first conflict aborts the scan, but the returned
// error lists every conflict found. cmd/varve calls this at startup to
// refuse to serve a conflicted repository (D6, proposal §7.5).
// Concurrently safe.
func (o *OrchestratorImpl) ValidateConflicts(ctx context.Context) error {
	produced, err := o.producedPkgnames(ctx)
	if err != nil {
		return err
	}
	pkgs, _, err := o.store.ListPackages(ctx, "", 1, maxScanBuilds)
	if err != nil {
		return err
	}
	base := make(map[int64]*db.Package, len(pkgs))
	for i := range pkgs {
		base[pkgs[i].ID] = &pkgs[i]
	}
	var conflicts []string
	seen := make(map[string]bool)
	add := func(pkgbase string) {
		owner, ok := produced[pkgbase]
		if ok && owner != pkgbase && !seen[pkgbase] {
			seen[pkgbase] = true
			conflicts = append(conflicts,
				fmt.Sprintf("pkgbase %q collides with a pkgname produced by package %q", pkgbase, owner))
		}
	}
	for _, p := range pkgs {
		add(p.Pkgbase)
	}
	active, err := o.store.ListActiveTasks(ctx)
	if err != nil {
		return err
	}
	for _, t := range active {
		if p, ok := base[t.PackageID]; ok {
			add(p.Pkgbase)
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return fmt.Errorf("%w: %s", ErrConflict, strings.Join(conflicts, "; "))
	}
	return nil
}

// ReadLog returns the full log of a build (web consumption). ErrNotFound
// when the log does not exist. Concurrently safe.
func (o *OrchestratorImpl) ReadLog(ctx context.Context, buildID string) ([]byte, error) {
	return o.logs.Read(buildID)
}

// TailLog streams the incremental part of a build log from offset onwards
// (SSE) and returns the new offset. ErrNotFound when the log does not
// exist. Concurrently safe.
func (o *OrchestratorImpl) TailLog(ctx context.Context, buildID string, offset int64, w io.Writer) (int64, error) {
	return o.logs.TailFrom(buildID, offset, w)
}
