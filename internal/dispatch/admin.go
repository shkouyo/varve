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
// reason "manual"). The name-conflict comparison is skipped (force) but
// the partial unique index still rejects a package with an active task;
// that conflict is reported as success (the rebuild is already queued
// or running), so repeated submissions never produce a misleading
// failure. A package whose last build is terminal gets a fresh task.
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
		URL:         pkg.URL,
		Licenses:    pkg.Licenses,
		Conflicts:   pkg.Conflicts,
		Provides:    pkg.Provides,
		UpstreamRef: pkg.LastUpstreamRef,
		PkgbuildRef: pkg.PkgbuildRef,
		Reason:      detect.ReasonManual,
	}
	// A pkgbuild_source package is force-rebuilt from its external
	// repository: the enqueue path needs the source pointer to read the
	// dispatch-time .SRCINFO hash from there.
	if dot, err := o.loadDotfile(ctx, pkg.Branch); err == nil && dot.PkgbuildSource != nil {
		c.PkgbuildSource = dot.PkgbuildSource
	}
	if err := o.Enqueue(ctx, c, true); err != nil {
		if errors.Is(err, ErrConflict) {
			return nil // an active task already covers this rebuild
		}
		return err
	}
	return nil
}

// DisableWorker stops new work from being assigned to a node: its status
// becomes "disabled" and Poll refuses to claim for it (db.ClaimTask itself
// does not check status; the check lives here). The node keeps its history
// and may be re-enabled with EnableWorker. A missing worker is reported
// as success (an absent node trivially receives no work), so repeated
// submissions (e.g. after a removal) are idempotent. Concurrently safe.
func (o *OrchestratorImpl) DisableWorker(ctx context.Context, name string) error {
	if _, err := o.store.GetWorkerByName(ctx, name); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil // absent worker is trivially disabled
		}
		return err
	}
	return o.store.SetWorkerStatus(ctx, name, "disabled")
}

// EnableWorker reverses DisableWorker: the node's status becomes "online"
// again and Poll resumes claiming for it once it heartbeats. A missing
// worker is reported as success (the worker is gone, so the disabled
// state it was in no longer exists). Concurrently safe.
func (o *OrchestratorImpl) EnableWorker(ctx context.Context, name string) error {
	if _, err := o.store.GetWorkerByName(ctx, name); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil // absent worker is trivially enabled
		}
		return err
	}
	return o.store.SetWorkerStatus(ctx, name, "online")
}

// RemoveWorker deletes a node record. A node with active tasks cannot be
// removed (ErrConflict); builds keep the display name as plain text
// (worker_name), so history survives the deletion. A missing worker is
// reported as success (the desired state is already reached), so a
// repeated submission is idempotent. Concurrently safe.
func (o *OrchestratorImpl) RemoveWorker(ctx context.Context, name string) error {
	w, err := o.store.GetWorkerByName(ctx, name)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil // already removed
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

// Stats aggregates the dashboard data: queue length, build counts by
// status, the newest builds and the worker list. Concurrently safe.
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
	if n := o.cfg.Web.RecentBuilds; n > 0 && len(recent) > n {
		recent = recent[:n]
	}
	workers, err := o.store.ListWorkers(ctx)
	if err != nil {
		return nil, err
	}
	return &Stats{QueueLen: queueLen, ByStatus: byStatus, RecentBuilds: recent, Workers: workers}, nil
}

// ValidateConflicts scans the full packages and queue state for name
// collisions: a pkgbase that equals a pkgname produced by the last build
// of another package. The first conflict aborts the scan, but the
// returned error lists every conflict found. cmd/varve calls this at
// startup to refuse to serve a conflicted repository. Concurrently safe.
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

// Size returns the current byte length of a build log (0 when the log
// does not exist). The SSE handler uses it to clamp resume offsets to
// the truncation cap without reading the log body. Concurrently safe.
func (o *OrchestratorImpl) Size(ctx context.Context, buildID string) (int64, error) {
	return o.logs.Size(buildID)
}
