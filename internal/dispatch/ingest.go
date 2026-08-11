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
	"time"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/detect/srcinfo"
	"git.0x0f.dev/varve/internal/mail"
	"git.0x0f.dev/varve/internal/objname"
	"git.0x0f.dev/varve/internal/repo"
	"git.0x0f.dev/varve/internal/storage"
)

// ReportResult handles the final result report of a task. succeeded runs
// the full verification + ingest orchestration; failed and cancelled
// finalize the task, notify (failed only) and clean the staging area. The
// task must not be terminal and, once cancellation was requested, only a
// cancelled report is accepted (cancellation wins). Claim-token
// protected. Concurrently safe.
func (o *OrchestratorImpl) ReportResult(ctx context.Context, taskID, token string, res ResultReq) error {
	if err := o.checkToken(ctx, taskID, token); err != nil {
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
		return ErrConflict // cancellation wins over any late report
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

// ingestTimeout bounds the verification + ingest chain of a succeeded
// result report. The chain must complete even after the client already
// disconnected (the worker's result POST can time out while a large
// package is still being ingested), so it runs on a context detached
// from the request's cancellation. It is deliberately much larger than
// settleTimeout, which only covers quick terminal SQLite writes: moving
// a large package (hundreds of MiB) into the repository takes minutes.
// 15m sits at the same order of magnitude as the worker's build budget,
// so a legitimate slow ingest is not cut short while a wedged store
// still cannot block a report forever. It is a variable so tests can
// shorten it.
var ingestTimeout = 15 * time.Minute

// ingestCtx returns the context the verification + ingest chain runs on:
// the request's values are kept, its cancellation is dropped (the client
// may have disconnected mid-ingest) and the chain is bounded by
// ingestTimeout.
func ingestCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), ingestTimeout)
}

// beginResult marks a task's succeeded result report as in flight. It
// reports false when another report of the same task is already being
// ingested; the caller turns that into ErrConflict (409), so a duplicate
// report never queues a second ingest behind the first.
func (o *OrchestratorImpl) beginResult(taskID string) bool {
	o.resultMu.Lock()
	defer o.resultMu.Unlock()
	if _, ok := o.inFlightResults[taskID]; ok {
		return false
	}
	o.inFlightResults[taskID] = struct{}{}
	return true
}

// endResult releases the in-flight marker of a succeeded result report.
func (o *OrchestratorImpl) endResult(taskID string) {
	o.resultMu.Lock()
	defer o.resultMu.Unlock()
	delete(o.inFlightResults, taskID)
}

// handleSucceeded runs the ingest orchestration in order: manifest
// verification → repo.Ingest → SQLite transaction (FinalizeTask +
// UpdatePackageAfterBuild) → staging cleanup. The whole sequence holds
// the single-repo ingest mutex and runs on a context detached from the
// request (ingestCtx), so a client disconnect mid-ingest cannot abort
// it. Any failure in verification finalizes failed(verify) with staging
// cleanup; any failure in ingest finalizes failed(ingest) and preserves
// the staging area for a retry.
func (o *OrchestratorImpl) handleSucceeded(ctx context.Context, task *db.Task, res ResultReq) error {
	if !o.beginResult(task.ID) {
		return ErrConflict // another report is already ingesting this task
	}
	defer o.endResult(task.ID)

	o.ingestMu.Lock()
	defer o.ingestMu.Unlock()

	// The chain below must survive a client disconnect: the worker's
	// result POST can time out (and the request context die) while a
	// large package is still being verified and ingested, and a
	// half-finished ingest would leave the repository inconsistent
	// (database updated, sidecar missing). It therefore runs on a
	// detached context with its own generous bound; the terminal SQLite
	// writes keep their dedicated settleCtx.
	ictx, icancel := ingestCtx(ctx)
	defer icancel()

	// Re-admit the report under the ingest lock: the task snapshot taken
	// in ReportResult may be stale, because a concurrent report (or a
	// cancel) can finalize the task while this one waited for the ingest
	// mutex. A terminal task is never ingested again; the stale report
	// sees the same ErrConflict as a late report. The check runs on the
	// detached context so a canceled request cannot abort it.
	current, err := o.store.GetTask(ictx, task.ID)
	if err != nil {
		return err
	}
	if isTerminal(current.State) || current.CancelRequested {
		return ErrConflict
	}

	// 1. Manifest verification: every entry exists and its sha256
	// recomputation matches; with signing enabled, package signatures
	// are verified with gpg.
	if err := o.verifyManifest(ictx, task.ID, res.Artifacts); err != nil {
		o.failTask(ictx, task, "verify", err.Error(), toDBArtifacts(res.Artifacts))
		o.cleanupStaging(ictx, task.ID, o.stagedFiles(res.Artifacts))
		return nil // the result was processed: the task is now failed
	}

	build, err := o.store.GetBuild(ictx, task.BuildID)
	if err != nil {
		return err
	}
	pkg, err := o.store.GetPackageByID(ictx, task.PackageID)
	if err != nil {
		return err
	}
	// The actual checked-out commit, falling back to the dispatched one.
	// For a pkgbuild_source task the reported commit is the external
	// repository head that was built; it rides build.PkgbuildRef so the
	// branch commit (the build.Commit record) keeps tracking the branch
	// trigger for last_commit.
	if res.Commit != "" {
		if o.pkgbuildTask(ictx, pkg) {
			build.PkgbuildRef = res.Commit
		} else {
			build.Commit = res.Commit
		}
	}

	// 2. Ingest into the repository (move, old-version cleanup, side file,
	// repo-add). The worker display name resolves through the database.
	workerName := o.workerName(ictx, task.WorkerID)
	if err := o.updater.Ingest(ictx, task, build, workerName, res.Artifacts); err != nil {
		// The failed build row records the reported artifacts, so the
		// page matches the repository state even when the ingest could
		// not finish. The staging area is preserved until the hourly
		// sweep; recovery comes from the detect cooldown, which
		// re-enqueues the package on a later round.
		o.failTask(ictx, task, "ingest", err.Error(), toDBArtifacts(res.Artifacts))
		return nil
	}

	// 3. One SQLite transaction: finalize succeeded + update the package
	// record (after repo-add, before staging cleanup). The tail of a
	// successful report must complete even when the client already
	// disconnected (the worker's result POST can time out while the ingest
	// finishes), so it runs on a settled context: a finalize that is lost
	// with the canceled request would leave the task stuck "running".
	stx, scancel := settleCtx(ctx)
	defer scancel()
	samples := o.finalSamples(stx, task.BuildID, res.ResourceUsage)
	currentVersion, pkgdesc, srcinfoHash, url, licenses, conflicts, provides, pkgname, source, pkgver, pkgrel, epoch := o.packageUpdateFields(stx, task.ID, res.Artifacts)
	err = o.store.WithTx(stx, func(tx *db.Tx) error {
		if err := tx.FinalizeTask(stx, task.ID, "succeeded", "", o.now().UTC(),
			toDBArtifacts(res.Artifacts), samples); err != nil {
			return err
		}
		return tx.UpdatePackageAfterBuild(stx, pkg.Pkgbase, db.PackageUpdate{
			CurrentVersion: currentVersion,
			Pkgdesc:        pkgdesc,
			SrcinfoHash:    srcinfoHash,
			UpstreamRef:    build.UpstreamRef,
			PkgbuildRef:    build.PkgbuildRef,
			BuildID:        task.BuildID,
			URL:            url,
			Licenses:       licenses,
			Conflicts:      conflicts,
			Provides:       provides,
			Pkgname:        pkgname,
			Source:         source,
			Pkgver:         pkgver,
			Pkgrel:         pkgrel,
			Epoch:          epoch,
			// The commit actually built (the reported checkout commit,
			// falling back to the dispatched one): without it the
			// package's last_commit never advances and detection
			// re-enqueues the unchanged branch forever.
			Commit: build.Commit,
		})
	})
	if err != nil {
		// The ingest itself already moved artifacts; a failed transaction
		// leaves them in the repository but the task is recorded failed
		// with the reported artifacts. Recovery comes from the detect
		// cooldown re-enqueue, not from a retry of this report (the task
		// is terminal now); the preserved staging area is swept after
		// 24h.
		o.failTask(stx, task, "ingest", err.Error(), toDBArtifacts(res.Artifacts))
		return nil
	}

	// 4. Staging cleanup.
	o.cleanupStaging(ictx, task.ID, o.stagedFiles(res.Artifacts))
	o.clearSigner(task.ID)

	// 5. AUR publishing: when the branch opted in and this change carried
	// a branch commit, mirror it into the AUR package repository. The
	// ingest already succeeded; a push failure is recorded and notified
	// but never fails the build.
	o.publishAUR(stx, pkg, build)
	return nil
}

// publishAUR pushes the built branch commit to AUR when every trigger
// condition holds: the controller has AUR publishing enabled (an SSH key),
// the branch dotfile opted in ([aur].submit with a package name) and the
// change carried a branch commit (build.Commit differs from the
// pre-ingest last successful commit; a pure upstream or manual rebuild
// leaves the commit unchanged and is not pushed).
//
// The outcome is always recorded on the package row and a failure
// additionally notifies the maintainers. The push runs synchronously
// inside the ingest lock, so a push is never reordered against later
// ingests of the same repository.
func (o *OrchestratorImpl) publishAUR(ctx context.Context, pkg *db.Package, build *db.Build) {
	if o.aurPusher == nil || pkg.AURName == "" || !pkg.AURSubmit {
		return
	}
	if o.cfg.AUR.KeyFile == "" {
		return // AUR publishing disabled by the controller configuration
	}
	if build.Commit == pkg.LastCommit {
		return // no branch commit in this change
	}
	err := o.aurPusher.Push(ctx, o.mirrorDir, build.Branch, pkg.AURName)
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	if rerr := o.store.RecordAURPush(ctx, pkg.Pkgbase, build.Commit, o.now().UTC(), errMsg); rerr != nil {
		log.Printf("dispatch: record AUR push for %s: %v", pkg.Pkgbase, rerr)
	}
	if err != nil {
		o.notifyAURFailure(ctx, pkg, build, err)
	}
}

// notifyAURFailure sends an AUR push failure notification to the package
// maintainers' email addresses. Send failures are only logged: they never
// affect the build or the recorded push outcome.
func (o *OrchestratorImpl) notifyAURFailure(ctx context.Context, pkg *db.Package, build *db.Build, pushErr error) {
	if o.notifier == nil {
		return
	}
	emails := db.MaintainerEmails(pkg.Maintainers)
	if len(emails) == 0 {
		return
	}
	info := mail.AURPushInfo{
		Pkgbase: pkg.Pkgbase,
		Branch:  build.Branch,
		AURName: pkg.AURName,
		Commit:  build.Commit,
		Error:   pushErr.Error(),
	}
	if err := o.notifier.SendAURFailure(ctx, emails, info); err != nil {
		log.Printf("dispatch: notify AUR push failure for %s: %v", pkg.Pkgbase, err)
	}
}

// failTask finalizes a task as failed through the terminal path (verify
// and ingest failures), notifies its maintainers and clears the token and
// key material. The reported artifacts ride onto the failed build row so
// the page matches the repository state. These stages are not retried: a
// bad checksum or a failed ingest has its own recovery (staging preserved
// until the hourly sweep; the detect cooldown re-enqueues the change on a
// later round). A race with another finalizer is tolerated: the
// already-terminal state wins and only the notification is skipped.
func (o *OrchestratorImpl) failTask(ctx context.Context, task *db.Task, stage, summary string, artifacts []db.Artifact) {
	if err := o.finalizeFailure(ctx, task, stage, summary, artifacts, nil); err != nil {
		if errors.Is(err, ErrConflict) {
			log.Printf("dispatch: task %s already terminal during %s failure handling", task.ID, stage)
			return
		}
		log.Printf("dispatch: finalize %s failed: %v", task.ID, err)
	}
}

// handleFailed applies the retry policy to an agent-reported build
// failure: while the task's fail counter is below the configured retry
// budget it is atomically re-queued for another attempt (same package
// version and source, fresh claim token); at the budget it is finalized
// failed with the package cooldown marker, a maintainer notification and
// staging cleanup.
func (o *OrchestratorImpl) handleFailed(ctx context.Context, task *db.Task, res ResultReq) error {
	stage, summary := "report", "build failed"
	if res.Error != nil {
		stage, summary = res.Error.Stage, res.Error.Summary
		if stage == "" {
			stage = "report"
		}
	}
	if err := o.failOrRetry(ctx, task, stage, summary, res); err != nil {
		return err
	}
	// Staging is cleaned on both branches: a retried attempt re-uploads
	// every file, and the old token/key material dies so the new attempt
	// exports fresh key material.
	o.cleanupStaging(ctx, task.ID, o.stagedFiles(res.Artifacts))
	o.clearSigner(task.ID)
	return nil
}

// failOrRetry picks the retry or the terminal branch for one
// agent-reported failure. The retry branch re-queues the task (the stale
// claim token dies with the requeue, so the re-claimed task can only be
// driven by the new token); the terminal branch finalizes with the
// cooldown marker and notification.
func (o *OrchestratorImpl) failOrRetry(ctx context.Context, task *db.Task, stage, summary string, res ResultReq) error {
	if task.FailCount < o.cfg.Worker.RetryMax {
		stx, cancel := settleCtx(ctx)
		defer cancel()
		if err := o.store.RequeueFailedTask(stx, task.ID); err != nil {
			return err
		}
		o.clearToken(stx, task.ID)
		o.releaseDispatch(stx, task.ID)
		log.Printf("dispatch: task %s failed (%s), retry %d/%d", task.ID, stage, task.FailCount+1, o.cfg.Worker.RetryMax)
		return nil
	}
	return o.finalizeFailure(ctx, task, stage, summary, toDBArtifacts(res.Artifacts), res.ResourceUsage)
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

// verifyManifest validates every manifest entry against the staging
// area: existence (Stat), sha256 recomputation (Get) and, when signing
// is enabled, gpg detached-signature verification of package entries.
func (o *OrchestratorImpl) verifyManifest(ctx context.Context, taskID string, manifest []repo.Artifact) error {
	if len(manifest) == 0 {
		return errors.New("empty manifest")
	}
	signing := o.cfg.Repo.Sign != "off"
	for _, a := range manifest {
		if a.Kind == "package" && !objname.ValidPkgname(a.Pkgname) {
			return fmt.Errorf("invalid pkgname %q for %q", a.Pkgname, a.File)
		}
		name := o.storage.StagingPath(taskID, a.File)
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
	// verify cleanly instead of dereferencing a nil signer.
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
	if err := o.signer.VerifyDetached(ctx, sigPath, pkgPath); err != nil {
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
	err = o.storage.Get(ctx, o.storage.StagingPath(taskID, name), f)
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
func (o *OrchestratorImpl) finalSamples(ctx context.Context, buildID string, reported []db.Sample) []db.Sample {
	build, err := o.store.GetBuild(ctx, buildID)
	if err != nil {
		log.Printf("dispatch: read build %s samples: %v", buildID, err)
		return reported
	}
	return mergeSamples(build.ResourceUsage, reported)
}

// packageUpdateFields derives the package record updates from the manifest:
// current_version (first package artifact), pkgdesc (from the staged
// .SRCINFO snapshot), srcinfo_hash (the srcinfo entry's verified hash) and
// the .SRCINFO metadata (url, licenses, conflicts, provides, pkgname,
// source, pkgver, epoch, pkgrel) so the verified build refreshes the
// package page fields.
func (o *OrchestratorImpl) packageUpdateFields(ctx context.Context, taskID string, manifest []repo.Artifact) (currentVersion, pkgdesc, srcinfoHash, url string, licenses, conflicts, provides, pkgname, source []string, pkgver, pkgrel string, epoch int) {
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
		if err := o.storage.Get(ctx, o.storage.StagingPath(taskID, src.File), &buf); err == nil {
			if info, perr := srcinfo.Parse([]byte(buf.String())); perr == nil {
				pkgdesc = info.Pkgdesc
				url = info.URL
				licenses = info.Licenses
				conflicts = info.Conflicts
				provides = info.Provides
				pkgname = info.Pkgname
				source = info.Source
				pkgver = info.Pkgver
				pkgrel = info.Pkgrel
				epoch = info.Epoch
			} else {
				log.Printf("dispatch: parse .SRCINFO for package update: %v", perr)
			}
		} else {
			log.Printf("dispatch: read .SRCINFO for package update: %v", err)
		}
	}
	return currentVersion, pkgdesc, srcinfoHash, url, licenses, conflicts, provides, pkgname, source, pkgver, pkgrel, epoch
}
