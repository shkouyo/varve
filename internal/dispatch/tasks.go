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
	"strings"
	"time"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/detect"
)

// GetTask returns the task detail for a one-shot agent. The first call
// (state=assigned) transitions the task to running with started_at=now;
// subsequent calls are idempotent. A queued task is claimed directly
// when the caller holds a pre-issued dispatch token (actions one-shot
// runner: transient runners never register a node). Terminal tasks
// conflict. Claim-token protected. Concurrently safe.
func (o *OrchestratorImpl) GetTask(ctx context.Context, taskID, token string) (*TaskDetail, error) {
	if err := o.checkToken(ctx, taskID, token); err != nil {
		return nil, err
	}
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if task.State == "queued" {
		// Pre-issued dispatch token: claim the task straight to running.
		if err := o.store.ClaimTaskToken(ctx, taskID, token, o.now().UTC()); err != nil {
			return nil, err
		}
		task, err = o.store.GetTask(ctx, taskID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, ErrNotFound
			}
			return nil, err
		}
	}
	switch task.State {
	case "assigned":
		if err := o.store.MarkRunning(ctx, taskID, o.now().UTC()); err != nil {
			return nil, err
		}
	case "running":
		// idempotent re-fetch
	case "succeeded", "failed", "cancelled":
		return nil, ErrConflict
	default:
		return nil, fmt.Errorf("dispatch: get task %s: unexpected state %q", taskID, task.State)
	}
	return o.taskDetail(ctx, task)
}

// AppendLog appends one buffered log segment with strict offset
// semantics: the client's offset must equal the current log size
// (ErrConflict with the current offset otherwise). An optional progress
// report is applied and the response carries the durable cancellation
// flag, read from the database. Claim-token protected. Concurrently
// safe.
func (o *OrchestratorImpl) AppendLog(ctx context.Context, taskID, token string, seg LogSegment) (*LogAck, error) {
	if err := o.checkToken(ctx, taskID, token); err != nil {
		return nil, err
	}
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if isTerminal(task.State) && !o.grantPostTerminalSegment(taskID) {
		// The task is terminal and its one post-terminal segment is
		// spent: further log data is refused so a holder of the (still
		// valid) token cannot grow the permanently kept failed log
		// without bound.
		return nil, ErrConflict
	}
	buildID := task.BuildID
	size, err := o.logs.Size(buildID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		size = 0 // first segment
	}
	if seg.Offset != size {
		return nil, &OffsetError{Current: size}
	}
	if err := o.logs.Append(buildID, []byte(seg.Data)); err != nil {
		return nil, err
	}
	if seg.Progress != nil {
		if err := o.processProgress(ctx, *seg.Progress); err != nil {
			log.Printf("dispatch: append log %s: progress: %v", taskID, err)
		}
	}
	// The cancel flag is always re-read from the database (no in-memory
	// cancel state).
	current, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return &LogAck{Offset: size + int64(len(seg.Data)), Cancelled: current.CancelRequested}, nil
}

// taskDetail assembles the full TaskDetail handed to a worker. Hooks,
// collect rules and the pkgbuild_source are re-parsed from the branch
// dotfile in the mirror (the database deliberately keeps no per-task
// copy, so the detail stays reconstructable after a controller restart);
// a missing or unparsable dotfile degrades to an empty config with a
// warning. source.mode is "archive" when the private-source snapshot is
// configured, "clone" otherwise.
func (o *OrchestratorImpl) taskDetail(ctx context.Context, t *db.Task) (*TaskDetail, error) {
	pkg, err := o.store.GetPackageByID(ctx, t.PackageID)
	if err != nil {
		return nil, err
	}
	build, err := o.store.GetBuild(ctx, t.BuildID)
	if err != nil {
		return nil, err
	}
	dot, err := o.loadDotfile(ctx, pkg.Branch)
	if err != nil {
		log.Printf("dispatch: task %s: dotfile warning: %v", t.ID, err)
		dot = &detect.Dotfile{}
	}

	mode := "clone"
	archive := ""
	if o.archiveMode() {
		mode = "archive"
		archive = o.storage.StagingPath(t.ID, sourceArchiveName)
	}
	deadline := time.Time{}
	if t.AssignedAt != nil {
		deadline = t.AssignedAt.Add(o.cfg.Worker.BuildTimeout)
	}
	signing := o.cfg.Repo.Sign
	detail := &TaskDetail{
		ID: t.ID,
		Package: TaskPackage{
			Pkgbase: pkg.Pkgbase,
			Branch:  pkg.Branch,
			VCSKind: pkg.VCSKind,
			Arch:    pkg.Arch,
		},
		Source: SourceInfo{
			Mode:    mode,
			URL:     o.cfg.Source.URL,
			Branch:  pkg.Branch,
			Commit:  build.Commit,
			Archive: archive,
		},
		Hooks: HooksInfo{
			PreBuild:  dot.Hooks.PreBuild,
			PostBuild: dot.Hooks.PostBuild,
			OnSuccess: dot.Hooks.OnSuccess,
			OnFailure: dot.Hooks.OnFailure,
		},
		Collect: CollectInfo{Exclude: dot.Collect.Exclude},
		Signing: SigningInfo{Required: signing != "off", Mode: signing},
		Build: BuildInfo{
			TimeoutSeconds: int64(o.cfg.Worker.BuildTimeout.Seconds()),
			Deadline:       deadline,
			CPULimit:       o.cfg.Worker.CPULimit,
			MemoryLimit:    o.cfg.Worker.MemoryLimit,
		},
		Packager: o.cfg.Worker.Packager,
	}
	if dot.PkgbuildSource != nil {
		detail.PkgbuildSource = &PkgbuildSource{
			URL:       dot.PkgbuildSource.URL,
			Branch:    dot.PkgbuildSource.Branch,
			Directory: dot.PkgbuildSource.Directory,
		}
	}
	return detail, nil
}

// loadDotfile re-reads and re-parses the branch dotfile from the mirror
// (the controller is the only dotfile parser). Extras are resolved
// through git show of the same branch.
func (o *OrchestratorImpl) loadDotfile(ctx context.Context, branch string) (*detect.Dotfile, error) {
	data, err := o.gitShow(ctx, branch, ".varve.toml")
	if err != nil {
		return nil, err
	}
	return detect.ParseDotfileWithExtras(func(path string) ([]byte, error) {
		return o.gitShow(ctx, branch, path)
	}, data)
}

// pkgbuildTask reports whether a package's branch builds from an external
// pkgbuild_source repository (re-parsed from the branch dotfile, the same
// source taskDetail uses). The ingest path calls it to route the reported
// checkout commit onto the build's external-head record.
func (o *OrchestratorImpl) pkgbuildTask(ctx context.Context, pkg *db.Package) bool {
	dot, err := o.loadDotfile(ctx, pkg.Branch)
	if err != nil {
		log.Printf("dispatch: task dotfile warning: %v", err)
		return false
	}
	return dot.PkgbuildSource != nil
}

// gitShow reads one file from the branch tree via "git show".
func (o *OrchestratorImpl) gitShow(ctx context.Context, branch, path string) ([]byte, error) {
	cmd := o.execCommand(ctx, "git", "-C", o.mirrorDir, "show", branch+":"+path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git show %s:%s: %w: %s", branch, path, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
