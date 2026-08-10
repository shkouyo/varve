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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"git.0x0f.dev/varve/internal/api"
	"git.0x0f.dev/varve/internal/db"
)

// executeTask runs the canonical 9-step build flow for one task: prepare →
// hook:pre_build → makepkg → hook:post_build → collect → sign → upload →
// report, then the on_success/on_failure hooks. Task-level failures are
// reported to the controller and never propagated as errors; only fatal
// runner errors surface.
//
// The main flow is a single goroutine; the log flush goroutine, the
// cancellation watcher and (in pool mode) the heartbeat goroutine run
// alongside, all coordinated through the task context.
func (r *Runner) executeTask(ctx context.Context, task *api.TaskDetail, token string) {
	tail := newTailBuffer(4096)
	stateCh := r.state.begin(task.ID)
	defer r.state.end()

	// Apply the configured PACKAGER identity to every build command of
	// this task (makepkg reads it for the built-in packaging identity).
	r.setTaskPackager(task.Packager)
	defer r.setTaskPackager("")

	// Log buffer: batched 1-2s/64KiB; one-shot segments carry a resource
	// sample in their progress field.
	var progress progressFn
	if r.mode == modeOneShot {
		progress = func() *api.TaskProgress {
			sm := r.sampler.Sample()
			r.state.addSample(sm)
			return &api.TaskProgress{
				TaskID:             task.ID,
				Stage:              r.state.currentStage(),
				CPUTimeNS:          sm.CPUTimeNS,
				MemoryBytes:        sm.MemoryBytes,
				DiskTotalBytes:     sm.DiskTotalBytes,
				DiskAvailableBytes: sm.DiskAvailableBytes,
				DiskUsedBytes:      sm.DiskUsedBytes,
				At:                 sm.At,
			}
		}
	}
	lb := NewLogBuffer(func(seg api.LogSegment) (*api.LogAck, error) {
		return r.client.AppendLog(ctx, task.ID, token, seg)
	}, r.logThreshold, r.logInterval, progress)
	defer lb.Close()

	// Merged cancellation signal: channel 1 (pool heartbeat/poll) and
	// channel 2 (log acknowledgements) both drive the same watcher.
	cancelCh := make(chan struct{})
	go func() {
		select {
		case <-stateCh:
		case <-lb.Cancelled():
		}
		close(cancelCh)
	}()

	// Build deadline: derived from the dispatched absolute deadline or
	// build.timeout_seconds.
	taskCtx, cancelTask := taskContext(ctx, task)
	defer cancelTask()

	buildLog := io.MultiWriter(lb, tail)

	taskDir := filepath.Join(r.workDir, task.ID)
	if err := os.RemoveAll(taskDir); err != nil {
		r.failStep(ctx, task, token, taskDir, taskCtx, cancelCh, "", stagePrepare,
			"clean work dir: "+err.Error(), buildLog)
		return
	}
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		r.failStep(ctx, task, token, taskDir, taskCtx, cancelCh, "", stagePrepare,
			"create work dir: "+err.Error(), buildLog)
		return
	}

	// ① prepare: clone (single-branch shallow) or download+extract the
	// source archive; require .SRCINFO; record the actual checkout commit.
	r.state.setStage(stagePrepare)
	commit, err := r.prepare(taskCtx, task, token, taskDir, buildLog)
	if err != nil {
		r.failStep(ctx, task, token, taskDir, taskCtx, cancelCh, commit, stagePrepare, err.Error(), buildLog)
		return
	}

	// ② pre_build hooks (cwd = checkout, builder identity): any failure
	// fails the task.
	r.state.setStage(stagePreBuild)
	if err := r.runHooks(taskCtx, taskDir, task.Hooks.PreBuild, buildLog); err != nil {
		r.failStep(ctx, task, token, taskDir, taskCtx, cancelCh, commit, stagePreBuild, err.Error(), buildLog)
		return
	}

	// ③ makepkg: run directly as builder (dependency installation goes
	// through sudo NOPASSWD); bounded by the deadline, with cancellation
	// handling.
	r.state.setStage(stageMakepkg)
	kind, exit, merr := r.runMakepkg(taskCtx, taskDir, buildLog, cancelCh)
	switch kind {
	case outcomeCancelled:
		r.reportCancelled(ctx, task, token)
		r.runHooks(ctx, taskDir, task.Hooks.OnFailure, buildLog)
		return
	case outcomeTimeout:
		r.failTask(ctx, task, token, "timeout", "build deadline exceeded", commit)
		r.runHooks(ctx, taskDir, task.Hooks.OnFailure, buildLog)
		return
	}
	if merr != nil || exit != 0 {
		r.failTask(ctx, task, token, stageMakepkg, makepkgSummary(exit, merr, tail), commit)
		r.runHooks(ctx, taskDir, task.Hooks.OnFailure, buildLog)
		return
	}

	// ④ post_build hooks: failures warn but never abort.
	r.state.setStage(stagePostBuild)
	if err := r.runHooks(taskCtx, taskDir, task.Hooks.PostBuild, buildLog); err != nil {
		log.Printf("agent: task %s: %s hook failed (ignored): %v", task.ID, stagePostBuild, err)
	}

	// ⑤ collect: *.pkg.tar.zst minus collect.exclude; empty fails.
	r.state.setStage(stageCollect)
	src := readSrcinfo(filepath.Join(taskDir, ".SRCINFO"))
	pkgs, err := collect(taskDir, task.Collect.Exclude, pkgNames(src))
	if err != nil {
		r.failStep(ctx, task, token, taskDir, taskCtx, cancelCh, commit, stageCollect, err.Error(), buildLog)
		return
	}

	// ⑥ sign: claim the one-shot key and detach-sign every package into a
	// temporary GNUPGHOME outside the build tree (a sibling of the task
	// dir, like the pkgbuild_source clone), so the repository-supplied
	// on_success/on_failure hooks never see the key material inside the
	// checkout. The home is removed right after signing, before the
	// upload and the success hooks run; the deferred removal is the
	// failure backstop and the container teardown the last resort.
	var sigs []string
	if task.Signing.Required {
		r.state.setStage(stageSign)
		gnupgHome := filepath.Join(r.workDir, ".gnupg-"+task.ID)
		sigs, err = r.signPackages(taskCtx, task, token, pkgs, gnupgHome, buildLog)
		if err != nil {
			_ = os.RemoveAll(gnupgHome)
			r.failStep(ctx, task, token, taskDir, taskCtx, cancelCh, commit, stageSign, err.Error(), buildLog)
			return
		}
		// Remove the key home before upload and the on_success hooks:
		// nothing after signing may read the key material.
		if err := os.RemoveAll(gnupgHome); err != nil {
			log.Printf("agent: task %s: remove gnupg home: %v", task.ID, err)
		}
		defer os.RemoveAll(gnupgHome)
	}

	// ⑦ upload: packages, signatures, then the .SRCINFO snapshot.
	r.state.setStage(stageUpload)
	uploads := make([]string, 0, len(pkgs)+len(sigs)+1)
	uploads = append(uploads, pkgs...)
	uploads = append(uploads, sigs...)
	uploads = append(uploads, filepath.Join(taskDir, ".SRCINFO"))
	manifest, err := r.uploadFiles(taskCtx, task, token, uploads, src)
	if err != nil {
		r.failStep(ctx, task, token, taskDir, taskCtx, cancelCh, commit, stageUpload, err.Error(), buildLog)
		return
	}

	// ⑧ report: succeeded with the full manifest, resource usage and the
	// actual checkout commit.
	r.state.setStage(stageReport)
	r.report(ctx, task, token, api.ResultReq{
		Status:        statusSucceeded,
		Artifacts:     manifest,
		ResourceUsage: r.finalSamples(),
		Commit:        commit,
	})

	// ⑨ on_success hooks.
	r.state.setStage(stageOnSuccess)
	if err := r.runHooks(ctx, taskDir, task.Hooks.OnSuccess, buildLog); err != nil {
		log.Printf("agent: task %s: %s hook failed: %v", task.ID, stageOnSuccess, err)
	}
}

// taskContext derives the build context from the task deadline: the
// absolute deadline when present, otherwise build.timeout_seconds.
func taskContext(parent context.Context, task *api.TaskDetail) (context.Context, context.CancelFunc) {
	if !task.Build.Deadline.IsZero() {
		return context.WithDeadline(parent, task.Build.Deadline)
	}
	if task.Build.TimeoutSeconds > 0 {
		return context.WithTimeout(parent, time.Duration(task.Build.TimeoutSeconds)*time.Second)
	}
	return context.WithCancel(parent)
}

// prepare checks out the task source: single-branch shallow clone for
// mode=clone, download+extract of the controller-staged tar.zst snapshot
// for mode=archive, or the external pkgbuild_source repository when the
// task carries one (see prepareExternal). A checkout without .SRCINFO has
// it rendered from the PKGBUILD via "makepkg --printsrcinfo" (the file is
// a generated artifact); a generation failure fails the step. It records the actually
// checked-out commit; an unknown commit (archive snapshots carry no git
// metadata) stays empty and the controller falls back to the dispatched
// source commit.
func (r *Runner) prepare(ctx context.Context, task *api.TaskDetail, token, taskDir string, w io.Writer) (string, error) {
	if task.PkgbuildSource != nil {
		return r.prepareExternal(ctx, task, taskDir, w)
	}
	switch task.Source.Mode {
	case "archive":
		if task.Source.Archive == "" {
			return "", errors.New("source.mode=archive without an archive name")
		}
		rc, err := r.client.DownloadFile(ctx, task.ID, token, task.Source.Archive)
		if err != nil {
			return "", fmt.Errorf("download source archive: %w", err)
		}
		defer rc.Close()
		archivePath := filepath.Join(taskDir, filepath.Base(task.Source.Archive))
		f, err := os.Create(archivePath)
		if err != nil {
			return "", fmt.Errorf("write source archive: %w", err)
		}
		if _, err := io.Copy(f, rc); err != nil {
			f.Close()
			return "", fmt.Errorf("write source archive: %w", err)
		}
		if err := f.Close(); err != nil {
			return "", fmt.Errorf("write source archive: %w", err)
		}
		exit, err := runCmd(ctx, r.command, taskDir, w, nil, "tar", "--zstd", "-xf", archivePath)
		if err != nil || exit != 0 {
			return "", fmt.Errorf("extract source archive: exit %d: %w", exit, err)
		}
	case "clone", "":
		if task.Source.URL == "" {
			return "", errors.New("source.mode=clone without a source url")
		}
		args := []string{"clone", "--depth", "1"}
		if task.Source.Branch != "" {
			args = append(args, "--branch", task.Source.Branch)
		}
		args = append(args, task.Source.URL, taskDir)
		exit, err := runCmd(ctx, r.command, taskDir, w, nil, "git", args...)
		if err != nil || exit != 0 {
			return "", fmt.Errorf("clone source: exit %d: %w", exit, err)
		}
	default:
		return "", fmt.Errorf("unknown source mode %q", task.Source.Mode)
	}

	if _, err := os.Stat(filepath.Join(taskDir, ".SRCINFO")); err != nil {
		// The source snapshot may legitimately carry no .SRCINFO: render
		// it from the PKGBUILD so the build metadata and the uploaded
		// snapshot stay available.
		if err := r.generateSrcinfo(ctx, taskDir); err != nil {
			return "", err
		}
	}

	out, err := captureCmd(ctx, r.command, taskDir, "git", "rev-parse", "HEAD")
	if err != nil {
		// No git metadata (archive mode) or rev-parse failure: the commit
		// stays empty and the controller falls back.
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

// generateSrcinfo renders the checkout's .SRCINFO from its PKGBUILD via
// "makepkg --printsrcinfo" and writes it into the checkout. Only stdout
// feeds the file (makepkg diagnostics go to stderr) so the artifact stays
// parseable; a failed or empty invocation fails the step.
func (r *Runner) generateSrcinfo(ctx context.Context, taskDir string) error {
	cmd := r.command(ctx, "makepkg", "--printsrcinfo")
	cmd.Dir = taskDir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("makepkg --printsrcinfo: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	if strings.TrimSpace(out.String()) == "" {
		return errors.New("makepkg --printsrcinfo produced no output")
	}
	return os.WriteFile(filepath.Join(taskDir, ".SRCINFO"), out.Bytes(), 0o644)
}

// prepareExternal checks out a pkgbuild_source task: the branch tree only
// carries the dotfile, so the PKGBUILD comes from the external repository
// named by the task. The repository is shallow-cloned into a sibling
// directory and the optional directory subpath is moved to the checkout
// root, so the rest of the flow (makepkg, hooks, collection) runs
// unchanged. Cloning straight into the checkout root would leave every
// unrelated entry of the external repository in the build tree and
// collide whenever the root and the subpath share a name (a common
// LICENSE in a monorepo), so the clone never touches the build root. The
// reported commit is the external repository head actually built; the
// controller routes it onto the build's pkgbuild_ref record.
func (r *Runner) prepareExternal(ctx context.Context, task *api.TaskDetail, taskDir string, w io.Writer) (string, error) {
	src := task.PkgbuildSource
	branch := src.Branch
	if branch == "" {
		branch = defaultPkgbuildBranch
	}
	extDir := taskDir + ".ext"
	if err := os.RemoveAll(extDir); err != nil {
		return "", fmt.Errorf("clean pkgbuild source dir: %w", err)
	}
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		return "", fmt.Errorf("create pkgbuild source dir: %w", err)
	}
	defer os.RemoveAll(extDir)
	args := []string{"clone", "--depth", "1", "--branch", branch, src.URL, extDir}
	exit, err := runCmd(ctx, r.command, extDir, w, nil, "git", args...)
	if err != nil || exit != 0 {
		return "", fmt.Errorf("clone pkgbuild source: exit %d: %w", exit, err)
	}
	buildRoot := extDir
	if src.Directory != "" {
		sub := filepath.Join(extDir, filepath.FromSlash(src.Directory))
		if _, err := os.Stat(sub); err != nil {
			return "", fmt.Errorf("pkgbuild source has no directory %q: %w", src.Directory, err)
		}
		buildRoot = sub
	}
	// Capture the external head before the clone metadata is discarded
	// with the sibling directory: an empty commit makes the controller
	// fall back to the dispatched source commit.
	out, err := captureCmd(ctx, r.command, extDir, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", nil
	}
	commit := strings.TrimSpace(out)
	if err := moveTreeUp(buildRoot, taskDir); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(taskDir, ".SRCINFO")); err != nil {
		if err := r.generateSrcinfo(ctx, taskDir); err != nil {
			return "", err
		}
	}
	return commit, nil
}

// moveTreeUp moves every entry of sub into root, erroring when a name
// already exists at root, then removes the emptied subdirectory. It is how
// a pkgbuild_source directory subpath becomes the build root while keeping
// the rest of the flow rooted at the work dir.
func moveTreeUp(sub, root string) error {
	entries, err := os.ReadDir(sub)
	if err != nil {
		return fmt.Errorf("read pkgbuild source directory %s: %w", sub, err)
	}
	for _, e := range entries {
		dst := filepath.Join(root, e.Name())
		if _, err := os.Lstat(dst); err == nil {
			return fmt.Errorf("pkgbuild source entry %q collides with the checkout root", e.Name())
		}
		if err := os.Rename(filepath.Join(sub, e.Name()), dst); err != nil {
			return fmt.Errorf("move pkgbuild source entry %q: %w", e.Name(), err)
		}
	}
	return os.RemoveAll(sub)
}

// runHooks executes shell hooks with cwd = the checkout dir. Any hook
// failing (non-zero exit) returns an error.
func (r *Runner) runHooks(ctx context.Context, taskDir string, hooks []string, w io.Writer) error {
	for _, hook := range hooks {
		if strings.TrimSpace(hook) == "" {
			continue
		}
		exit, err := runCmd(ctx, r.command, taskDir, w, nil, "sh", "-c", hook)
		if err != nil {
			return fmt.Errorf("hook %q: %w", hook, err)
		}
		if exit != 0 {
			return fmt.Errorf("hook %q exited with status %d", hook, exit)
		}
	}
	return nil
}

// runMakepkg runs "makepkg -s --noconfirm" in the checkout, streaming
// merged output to w. The process runs in its own process group; the
// cancellation watcher terminates it with SIGTERM escalated to SIGKILL
// after killGrace. kind is empty on a normal exit, or
// outcomeCancelled/outcomeTimeout when the watcher had to stop the build.
func (r *Runner) runMakepkg(taskCtx context.Context, taskDir string, w io.Writer, cancelCh <-chan struct{}) (kind string, exit int, err error) {
	cmd := r.command(context.Background(), "makepkg", "-s", "--noconfirm")
	cmd.Dir = taskDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		return "", -1, fmt.Errorf("start makepkg: %w", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	select {
	case <-done:
		// makepkg finished on its own.
	case <-taskCtx.Done():
		if errors.Is(taskCtx.Err(), context.DeadlineExceeded) {
			kind = outcomeTimeout
		} else {
			kind = outcomeCancelled
		}
		r.terminate(cmd, done)
	case <-cancelCh:
		kind = outcomeCancelled
		r.terminate(cmd, done)
	}
	if kind != "" {
		// The watcher waited for the process; the exit code is irrelevant
		// for the report.
		return kind, 0, nil
	}
	if cmd.ProcessState == nil {
		return "", -1, errors.New("makepkg finished without a process state")
	}
	return "", cmd.ProcessState.ExitCode(), nil
}

// makepkgSummary builds the makepkg failure summary including the tail of
// the build log.
func makepkgSummary(exit int, err error, tail *tailBuffer) string {
	if err != nil {
		return "makepkg failed: " + err.Error()
	}
	return fmt.Sprintf("makepkg exited with status %d; tail:\n%s", exit, tail.Last(1500))
}

// failStep reports a failed step, mapping the cause: controller
// cancellation → cancelled, deadline expiry → failed(timeout), otherwise a
// plain step failure. The on_failure hooks run afterwards.
func (r *Runner) failStep(ctx context.Context, task *api.TaskDetail, token, taskDir string,
	taskCtx context.Context, cancelCh <-chan struct{}, commit, stage, summary string, w io.Writer) {
	switch r.outcomeKind(taskCtx, cancelCh) {
	case outcomeCancelled:
		r.reportCancelled(ctx, task, token)
	case outcomeTimeout:
		r.failTask(ctx, task, token, "timeout", "build deadline exceeded", commit)
	default:
		r.failTask(ctx, task, token, stage, summary, commit)
	}
	r.runHooks(ctx, taskDir, task.Hooks.OnFailure, w)
}

// outcomeKind classifies the cause of an interrupted step.
func (r *Runner) outcomeKind(taskCtx context.Context, cancelCh <-chan struct{}) string {
	select {
	case <-cancelCh:
		return outcomeCancelled
	default:
	}
	if taskCtx.Err() != nil {
		if errors.Is(taskCtx.Err(), context.DeadlineExceeded) {
			return outcomeTimeout
		}
		return outcomeCancelled
	}
	return ""
}

// failTask reports a failed result with the given stage and summary.
func (r *Runner) failTask(ctx context.Context, task *api.TaskDetail, token, stage, summary, commit string) {
	r.report(ctx, task, token, api.ResultReq{
		Status:        statusFailed,
		Error:         &api.ResultError{Stage: stage, Summary: truncateSummary(summary)},
		ResourceUsage: r.finalSamples(),
		Commit:        commit,
	})
}

// reportCancelled reports a controller-requested cancellation. Late
// duplicate reports (409) are ignored by the controller.
func (r *Runner) reportCancelled(ctx context.Context, task *api.TaskDetail, token string) {
	log.Printf("agent: task %s: cancelled", task.ID)
	r.report(ctx, task, token, api.ResultReq{Status: statusCancelled, ResourceUsage: r.finalSamples()})
}

// report sends the final result; a 409 (late duplicate) is logged and
// ignored, any other failure is logged best-effort. A successful send
// (or a 409, which means the controller already holds a result) marks
// the report as acknowledged; runOneShot turns a missing acknowledgement
// into a non-zero container exit so the host can report the loss.
func (r *Runner) report(ctx context.Context, task *api.TaskDetail, token string, res api.ResultReq) {
	if err := r.client.ReportResult(ctx, task.ID, token, res); err != nil {
		if isConflict(err) {
			log.Printf("agent: task %s: late result ignored (409)", task.ID)
		} else {
			log.Printf("agent: task %s: report result: %v", task.ID, err)
			return
		}
	}
	r.state.markReportAcked()
}

// finalSamples snapshots the resource samples accumulated during the task,
// appending one final sample.
func (r *Runner) finalSamples() []db.Sample {
	sm := r.sampler.Sample()
	r.state.addSample(sm)
	return r.state.snapshotSamples()
}

// runCmd runs an external command with merged stdout/stderr streamed to w.
// It returns the exit code (0 on success) and an error only for failures
// other than a non-zero exit.
func runCmd(ctx context.Context, execFn func(ctx context.Context, name string, arg ...string) *exec.Cmd,
	dir string, w io.Writer, env []string, name string, arg ...string) (int, error) {
	cmd := execFn(ctx, name, arg...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// runCmdIn runs an external command like runCmd, feeding the given text
// to the child's stdin. It exists for commands whose sensitive input
// (signing keys, passphrases) must travel through a pipe instead of a
// file or the argument list.
func runCmdIn(ctx context.Context, execFn func(ctx context.Context, name string, arg ...string) *exec.Cmd,
	dir string, w io.Writer, env []string, stdin string, name string, arg ...string) (int, error) {
	cmd := execFn(ctx, name, arg...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	cmd.Stdout = w
	cmd.Stderr = w
	cmd.Stdin = strings.NewReader(stdin)
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// captureCmd runs an external command and returns its combined output.
func captureCmd(ctx context.Context, execFn func(ctx context.Context, name string, arg ...string) *exec.Cmd,
	dir, name string, arg ...string) (string, error) {
	cmd := execFn(ctx, name, arg...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
