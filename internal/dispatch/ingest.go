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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/detect/srcinfo"
	"git.0x0f.dev/varve/internal/repo"
	"git.0x0f.dev/varve/internal/storage"
)

// ReportResult handles the final result report of a task. succeeded runs
// the full verification + ingest orchestration (D7 order); failed and
// cancelled finalize the task, notify (failed only) and clean the staging
// area. The task must not be terminal and, once cancellation was requested,
// only a cancelled report is accepted (cancellation wins, D4). Claim-token
// protected. Concurrently safe.
func (o *OrchestratorImpl) ReportResult(ctx context.Context, taskID, token string, res ResultReq) error {
	if err := o.checkToken(taskID, token); err != nil {
		return err
	}
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if isTerminal(task.State) {
		return ErrConflict
	}
	if task.CancelRequested && res.Status != "cancelled" {
		return ErrConflict // cancellation wins over any late report (D4)
	}

	switch res.Status {
	case "succeeded":
		return o.handleSucceeded(ctx, task, res)
	case "failed":
		return o.handleFailed(ctx, task, res)
	case "cancelled":
		return o.handleCancelled(ctx, task)
	default:
		return fmt.Errorf("dispatch: result %s: unknown status %q", taskID, res.Status)
	}
}

// handleSucceeded runs the ingest orchestration in the D7 order:
// manifest verification → repo.Ingest → SQLite transaction (FinalizeTask +
// UpdatePackageAfterBuild) → staging cleanup. The whole sequence holds the
// single-repo ingest mutex. Any failure in verification finalizes
// failed(verify) with staging cleanup; any failure in ingest finalizes
// failed(ingest) and preserves the staging area for a retry.
func (o *OrchestratorImpl) handleSucceeded(ctx context.Context, task *db.Task, res ResultReq) error {
	o.ingestMu.Lock()
	defer o.ingestMu.Unlock()

	// 1. Manifest verification: every entry exists and its sha256
	// recomputation matches; with signing enabled, package signatures are
	// verified with gpg (DESIGN §5.5, §7.5 step 1).
	if err := o.verifyManifest(ctx, task.ID, res.Artifacts); err != nil {
		o.failTask(ctx, task, "verify", err.Error())
		o.cleanupStaging(ctx, task.ID, o.stagedFiles(res.Artifacts))
		return nil // the result was processed: the task is now failed
	}

	build, err := o.store.GetBuild(ctx, task.BuildID)
	if err != nil {
		return err
	}
	pkg, err := o.store.GetPackageByID(ctx, task.PackageID)
	if err != nil {
		return err
	}
	// D1: the actual checked-out commit, falling back to the dispatched one.
	if res.Commit != "" {
		build.Commit = res.Commit
	}

	// 2. Ingest into the repository (move, old-version cleanup, side file,
	// repo-add). The worker display name resolves through the database
	// (confirmed decision).
	workerName := o.workerName(ctx, task.WorkerID)
	if err := o.updater.Ingest(ctx, task, build, workerName, res.Artifacts); err != nil {
		o.failTask(ctx, task, "ingest", err.Error())
		// Staging is deliberately preserved so the ingest can be retried.
		return nil
	}

	// 3. One SQLite transaction: finalize succeeded + update the package
	// record (D7 order: after repo-add, before staging cleanup).
	samples := o.finalSamples(ctx, task.BuildID, res.ResourceUsage)
	currentVersion, pkgdesc, srcinfoHash := o.packageUpdateFields(ctx, task.ID, res.Artifacts)
	err = o.store.WithTx(ctx, func(tx *db.Tx) error {
		if err := tx.FinalizeTask(ctx, task.ID, "succeeded", "", o.now().UTC(),
			toDBArtifacts(res.Artifacts), samples); err != nil {
			return err
		}
		return tx.UpdatePackageAfterBuild(ctx, pkg.Pkgbase, currentVersion, pkgdesc,
			srcinfoHash, build.UpstreamRef, task.BuildID)
	})
	if err != nil {
		// The ingest itself already moved artifacts; a failed transaction
		// leaves them in the repository but the task is recorded failed and
		// retried from the preserved staging area on the next report.
		o.failTask(ctx, task, "ingest", err.Error())
		return nil
	}

	// 4. Staging cleanup.
	o.cleanupStaging(ctx, task.ID, o.stagedFiles(res.Artifacts))
	o.clearSigner(task.ID)
	return nil
}

// failTask finalizes a task as failed, notifies its maintainers and clears
// the token and key material. A race with another finalizer (agent report,
// cancel, scheduler scan) is tolerated: the already-terminal state wins and
// only the notification is skipped.
func (o *OrchestratorImpl) failTask(ctx context.Context, task *db.Task, stage, summary string) {
	err := o.finalizeTask(ctx, task.ID, "failed", stage+": "+summary, nil, nil)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			log.Printf("dispatch: task %s already terminal during %s failure handling", task.ID, stage)
			return
		}
		log.Printf("dispatch: finalize %s failed: %v", task.ID, err)
		return
	}
	build, berr := o.store.GetBuild(ctx, task.BuildID)
	if berr != nil {
		log.Printf("dispatch: read build %d for notification: %v", task.BuildID, berr)
		return
	}
	o.notifyFailure(ctx, task, build, stage, summary)
	o.clearSigner(task.ID)
}

// handleFailed finalizes the task as failed with the agent's stage and
// summary, notifies the maintainers and cleans the staging area (DESIGN
// §7.5 failure path).
func (o *OrchestratorImpl) handleFailed(ctx context.Context, task *db.Task, res ResultReq) error {
	stage, summary := "report", "build failed"
	if res.Error != nil {
		stage, summary = res.Error.Stage, res.Error.Summary
		if stage == "" {
			stage = "report"
		}
	}
	err := o.finalizeTask(ctx, task.ID, "failed", stage+": "+summary, toDBArtifacts(res.Artifacts), res.ResourceUsage)
	if err != nil {
		return err
	}
	build, berr := o.store.GetBuild(ctx, task.BuildID)
	if berr == nil {
		o.notifyFailure(ctx, task, build, stage, summary)
	} else {
		log.Printf("dispatch: read build %d for notification: %v", task.BuildID, berr)
	}
	o.cleanupStaging(ctx, task.ID, o.stagedFiles(res.Artifacts))
	o.clearSigner(task.ID)
	return nil
}

// handleCancelled finalizes the task as cancelled (agent confirmed the
// cancellation) and cleans the staging area; no notification is sent.
func (o *OrchestratorImpl) handleCancelled(ctx context.Context, task *db.Task) error {
	if err := o.finalizeTask(ctx, task.ID, "cancelled", "", nil, nil); err != nil {
		return err
	}
	o.cleanupStaging(ctx, task.ID, o.stagedFiles(nil))
	o.clearSigner(task.ID)
	return nil
}

// verifyManifest validates every manifest entry against the staging area:
// existence (Stat), sha256 recomputation (Get) and, when signing is
// enabled, gpg detached-signature verification of package entries
// (DESIGN §5.5, §7.5 step 1).
func (o *OrchestratorImpl) verifyManifest(ctx context.Context, taskID string, manifest []repo.Artifact) error {
	if len(manifest) == 0 {
		return errors.New("empty manifest")
	}
	signing := o.cfg.Repo.Sign != "off"
	for _, a := range manifest {
		name := storage.StagingPath(taskID, a.File)
		if _, err := o.storage.Stat(ctx, name); err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return fmt.Errorf("missing artifact %q", a.File)
			}
			return fmt.Errorf("stat %q: %w", a.File, err)
		}
		h := sha256.New()
		if err := o.storage.Get(ctx, name, h); err != nil {
			return fmt.Errorf("read %q: %w", a.File, err)
		}
		if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), a.SHA256) {
			return fmt.Errorf("sha256 mismatch for %q", a.File)
		}
		if signing && a.Kind == "package" {
			sig := o.signatureEntry(manifest, a.File)
			if sig == nil {
				return fmt.Errorf("missing signature for %q", a.File)
			}
			if err := o.verifySignature(ctx, taskID, &a, sig); err != nil {
				return err
			}
		}
	}
	return nil
}

// signatureEntry returns the signature artifact of a package file, if the
// manifest lists one.
func (o *OrchestratorImpl) signatureEntry(manifest []repo.Artifact, file string) *repo.Artifact {
	for i := range manifest {
		if manifest[i].File == file+".sig" {
			return &manifest[i]
		}
	}
	return nil
}

// verifySignature materializes the package and its detached signature into
// a temp directory and delegates to signer.VerifyDetached (gpg needs real
// local files; the storage backend may be remote).
func (o *OrchestratorImpl) verifySignature(ctx context.Context, taskID string, pkg, sig *repo.Artifact) error {
	// Defensive: signing is requested by the config but no signer is
	// wired (a caller passed nil with repo.sign != "off"). Fail the
	// verify cleanly instead of dereferencing a nil signer (bug fix M4).
	if o.signer == nil {
		return errors.New("signing enabled but no signer configured")
	}
	dir, err := os.MkdirTemp("", "varve-verify-*")
	if err != nil {
		return fmt.Errorf("verify %q: %w", pkg.File, err)
	}
	defer os.RemoveAll(dir)
	pkgPath := filepath.Join(dir, filepath.Base(pkg.File))
	sigPath := pkgPath + ".sig"
	if err := o.writeStaged(ctx, taskID, sig.File, sigPath); err != nil {
		return fmt.Errorf("verify %q: %w", pkg.File, err)
	}
	if err := o.writeStaged(ctx, taskID, pkg.File, pkgPath); err != nil {
		return fmt.Errorf("verify %q: %w", pkg.File, err)
	}
	if err := o.signer.VerifyDetached(sigPath, pkgPath); err != nil {
		return fmt.Errorf("signature verification failed for %q: %w", pkg.File, err)
	}
	return nil
}

// writeStaged copies one staged object into a local file.
func (o *OrchestratorImpl) writeStaged(ctx context.Context, taskID, name, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	err = o.storage.Get(ctx, storage.StagingPath(taskID, name), f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("stage %q: %w", name, err)
	}
	return nil
}

// finalSamples merges the samples accumulated through heartbeats and log
// progress with the result report's list so FinalizeTask never clobbers
// the streaming channel.
func (o *OrchestratorImpl) finalSamples(ctx context.Context, buildID int64, reported []db.Sample) []db.Sample {
	build, err := o.store.GetBuild(ctx, buildID)
	if err != nil {
		log.Printf("dispatch: read build %d samples: %v", buildID, err)
		return reported
	}
	return mergeSamples(build.ResourceUsage, reported)
}

// packageUpdateFields derives the package record updates from the manifest:
// current_version (first package artifact), pkgdesc (from the staged
// .SRCINFO snapshot) and srcinfo_hash (the srcinfo entry's verified hash).
func (o *OrchestratorImpl) packageUpdateFields(ctx context.Context, taskID string, manifest []repo.Artifact) (currentVersion, pkgdesc, srcinfoHash string) {
	var src *repo.Artifact
	for i := range manifest {
		if manifest[i].Kind == "package" && currentVersion == "" {
			currentVersion = manifest[i].Version
		}
		if manifest[i].Kind == "srcinfo" {
			src = &manifest[i]
			srcinfoHash = manifest[i].SHA256
		}
	}
	if src != nil {
		var buf strings.Builder
		if err := o.storage.Get(ctx, storage.StagingPath(taskID, src.File), &buf); err == nil {
			if info, perr := srcinfo.Parse([]byte(buf.String())); perr == nil {
				pkgdesc = info.Pkgdesc
			} else {
				log.Printf("dispatch: parse .SRCINFO for package update: %v", perr)
			}
		} else {
			log.Printf("dispatch: read .SRCINFO for package update: %v", err)
		}
	}
	return currentVersion, pkgdesc, srcinfoHash
}
