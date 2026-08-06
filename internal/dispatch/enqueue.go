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
	"sort"
	"strings"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/detect"
	"git.0x0f.dev/varve/internal/detect/srcinfo"
	"git.0x0f.dev/varve/internal/storage"
)

// maxScanBuilds bounds the paged scans used by the conflict checks and the
// dashboard (packages and builds tables).
const maxScanBuilds = 10000

// Enqueue validates one detected change and creates the task plus its
// mirrored build row in one transaction. It is the detect.Sink
// implementation (force=false). force=true skips the name-conflict
// comparison and is used by the admin rebuild path.
//
// Order: name-conflict check → branch HEAD resolution (TaskDetail
// source.commit and the archive snapshot both need it) → .SRCINFO hash →
// package upsert (creates the row for a first-time pkgbase) → CreateTask
// (the partial unique index is the dedupe backstop) → archive snapshot in
// archive mode.
//
// An archive failure finalizes the task as failed (stage "ingest") and
// logs the problem but never blocks the queue: other branches continue.
// Concurrently safe.
func (o *OrchestratorImpl) Enqueue(ctx context.Context, c detect.Change, force bool) error {
	if c.Package.Pkgbase == "" {
		return fmt.Errorf("dispatch: enqueue: empty pkgbase")
	}
	if !force {
		if err := o.checkNameConflict(ctx, c); err != nil {
			return err
		}
	}
	// Queue gate: a package whose declared architectures have no
	// intersection with the architectures this deployment can build is
	// skipped (with a warning from the detect pipeline) instead of being
	// queued where no worker could ever claim it.
	if err := o.checkSupportedArch(ctx, c.Package.Arch); err != nil {
		return err
	}
	commit, err := o.branchHead(ctx, c.Package.Branch)
	if err != nil {
		return fmt.Errorf("dispatch: enqueue %s: %w", c.Package.Pkgbase, err)
	}
	srcinfoHash, err := o.srcinfoHash(ctx, c.Package.Branch)
	if err != nil {
		return fmt.Errorf("dispatch: enqueue %s: %w", c.Package.Pkgbase, err)
	}
	now := o.now().UTC()
	taskID := uuidV4()

	pkg := db.Package{
		Pkgbase:     c.Package.Pkgbase,
		Branch:      c.Package.Branch,
		VCSKind:     c.Package.VCSKind,
		Arch:        c.Package.Arch,
		Maintainers: c.Maintainers,
	}
	if err := o.store.UpsertPackage(ctx, &pkg); err != nil {
		return fmt.Errorf("dispatch: enqueue %s: %w", c.Package.Pkgbase, err)
	}
	t := &db.Task{ID: taskID, PackageID: pkg.ID, State: "queued", CreatedAt: now, LastProgressAt: now}
	b := &db.Build{
		PackageID:   pkg.ID,
		Branch:      c.Package.Branch,
		Commit:      commit,
		UpstreamRef: c.UpstreamRef,
		SrcinfoHash: srcinfoHash,
	}
	if err := o.store.CreateTask(ctx, t, b); err != nil {
		// The partial unique index is the dedupe backstop; surface it as the
		// dispatch-level conflict sentinel for the API mapping.
		if errors.Is(err, db.ErrConflict) {
			return ErrConflict
		}
		return err
	}

	if o.archiveMode() {
		if err := o.archiveSource(ctx, t.ID, commit); err != nil {
			// Snapshot failure: the task is recorded as failed with stage
			// "ingest" so the failure is visible and notified, and the queue
			// keeps flowing.
			log.Printf("dispatch: enqueue %s: archive failed: %v", t.ID, err)
			_ = o.finalizeTask(ctx, t.ID, "failed", "ingest: "+err.Error(), nil, nil)
			o.notifyFailure(ctx, t, b, "ingest", err.Error())
			o.clearSigner(t.ID)
			return nil
		}
	}

	o.roundMu.Lock()
	o.roundSet[c.Package.Pkgbase] = now
	o.roundMu.Unlock()
	return nil
}

// archBaseline is the architecture every varve deployment is built around
// (it mirrors the packages/workers column defaults and the worker config
// default). Keeping it in the supported set means a fresh deployment with
// no registered workers still accepts baseline-arch packages instead of
// skipping everything.
const archBaseline = "x86_64"

// checkSupportedArch rejects a change whose declared architecture set has
// no intersection with the architectures the deployment can build: the
// static baseline plus every architecture registered by a worker
// (registration alone — any status — makes an architecture buildable, so
// this never depends on whether a worker happens to be online). "any"
// (architecture-independent) packages are always supported. Skipping
// unsupported packages keeps them from sitting in the queue forever with
// no worker able to claim them.
func (o *OrchestratorImpl) checkSupportedArch(ctx context.Context, arch string) error {
	if arch == "" || arch == "any" {
		return nil
	}
	supported, err := o.store.DistinctWorkerArches(ctx)
	if err != nil {
		return err
	}
	known := map[string]bool{archBaseline: true}
	for _, a := range supported {
		if a != "" {
			known[a] = true
		}
	}
	for _, elem := range strings.Split(arch, "|") {
		if known[elem] {
			return nil
		}
	}
	list := make([]string, 0, len(known))
	for a := range known {
		list = append(list, a)
	}
	sort.Strings(list)
	return fmt.Errorf("%w: package architectures %q not buildable (supported: %s)",
		ErrArchUnsupported, arch, strings.Join(list, ", "))
}

// checkNameConflict implements the pre-check for one change: the incoming
// pkgbase must not collide with a pkgname produced by the last build of
// another package, and must not be a duplicate of a change already
// enqueued in this detection round. Conflicts return ErrConflict so
// detect skips the branch without blocking the other branches.
func (o *OrchestratorImpl) checkNameConflict(ctx context.Context, c detect.Change) error {
	// Round set: prune entries older than one poll interval (a detection
	// round), then look for a duplicate pkgbase.
	o.roundMu.Lock()
	cutoff := o.now().Add(-o.cfg.Source.PollInterval)
	for k, ts := range o.roundSet {
		if ts.Before(cutoff) {
			delete(o.roundSet, k)
		}
	}
	_, dup := o.roundSet[c.Package.Pkgbase]
	o.roundMu.Unlock()
	if dup {
		return fmt.Errorf("%w: %s already enqueued in this detection round", ErrConflict, c.Package.Pkgbase)
	}

	// Packages table: another package's last build produced a pkgname
	// equal to the incoming pkgbase.
	produced, err := o.producedPkgnames(ctx)
	if err != nil {
		return fmt.Errorf("dispatch: conflict check: %w", err)
	}
	if owner, ok := produced[c.Package.Pkgbase]; ok && owner != c.Package.Pkgbase {
		return fmt.Errorf("%w: pkgbase %q collides with pkgname produced by package %q",
			ErrConflict, c.Package.Pkgbase, owner)
	}
	return nil
}

// producedPkgnames returns pkgname → producing pkgbase derived from the
// newest build record of every package (only successful ingests carry
// artifacts). Used by Enqueue and ValidateConflicts.
func (o *OrchestratorImpl) producedPkgnames(ctx context.Context) (map[string]string, error) {
	pkgs, _, err := o.store.ListPackages(ctx, "", 1, maxScanBuilds)
	if err != nil {
		return nil, fmt.Errorf("dispatch: list packages: %w", err)
	}
	base := make(map[int64]string, len(pkgs))
	for _, p := range pkgs {
		base[p.ID] = p.Pkgbase
	}
	all, _, err := o.store.ListBuilds(ctx, 1, maxScanBuilds, false)
	if err != nil {
		return nil, fmt.Errorf("dispatch: list builds: %w", err)
	}
	produced := make(map[string]string)
	seen := make(map[int64]bool, len(all))
	for _, b := range all { // newest first: first build seen per package is the last build
		if seen[b.PackageID] {
			continue
		}
		seen[b.PackageID] = true
		for _, a := range b.Artifacts {
			if a.Kind == "package" && a.Pkgname != "" {
				produced[a.Pkgname] = base[b.PackageID]
			}
		}
	}
	return produced, nil
}

// branchHead resolves the current HEAD commit of a mirror branch with the
// same canonical command detect uses ("git rev-parse
// refs/heads/<branch>"). It feeds TaskDetail.source.commit (fallback)
// and the archive snapshot.
func (o *OrchestratorImpl) branchHead(ctx context.Context, branch string) (string, error) {
	cmd := o.execCommand(ctx, "git", "-C", o.mirrorDir, "rev-parse", "refs/heads/"+branch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolve branch %s head: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// srcinfoHash computes the .SRCINFO hash of a branch (the same
// detect/srcinfo.Hash detect computes at detection time); it is recorded on
// the build row and compared on the next round.
func (o *OrchestratorImpl) srcinfoHash(ctx context.Context, branch string) (string, error) {
	cmd := o.execCommand(ctx, "git", "-C", o.mirrorDir, "show", branch+":.SRCINFO")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read .SRCINFO of branch %s: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	return srcinfo.Hash(out), nil
}

// archiveSource snapshots the branch at commit into
// staging/<taskID>/source.tar.zst by streaming "git archive
// --format=tar.zst <commit>" through storage.Put. Git performs the zstd
// compression itself, so no external compressor is needed. The snapshot
// is written immediately after task creation; a worker that claims the
// task in the tiny window before the write finishes fails with a download
// error and the next detection round re-enqueues it (recoverable).
func (o *OrchestratorImpl) archiveSource(ctx context.Context, taskID, commit string) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := o.execCommand(cctx, "git", "-C", o.mirrorDir, "archive", "--format=tar.zst", commit)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("git archive: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git archive: %w", err)
	}
	name := storage.StagingPath(taskID, sourceArchiveName)
	if err := o.storage.Put(cctx, name, stdout, -1); err != nil {
		cancel() // kill git so Wait cannot block on a full pipe
		_ = cmd.Wait()
		return fmt.Errorf("stage archive: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git archive: %w", err)
	}
	return nil
}

// clearSigner drops the one-shot key material of a finished task; it is
// safe when no signer is configured (NewOrchestrator normalizes an
// interface-wrapped typed nil pointer to a true nil, so this interface
// nil check is sound).
func (o *OrchestratorImpl) clearSigner(taskID string) {
	if o.signer != nil {
		o.signer.ClearTask(taskID)
	}
}
