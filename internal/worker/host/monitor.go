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
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"git.0x0f.dev/varve/internal/api"
)

// pollWorker is one of the capacity poll loops (DETAIL §11.6): it acquires
// a slot, polls once, and either hands the slot to a container monitor or
// releases it and retries after pollInterval (D8: no-task 5s retry). While
// its container runs, the worker parks on the next slot acquisition, so
// the buffered channel bounds concurrent containers to the node capacity.
func (r *Runner) pollWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case r.slots <- struct{}{}:
		}
		if r.processOne(ctx) {
			// The slot is now owned by the monitor goroutine.
			continue
		}
		r.slotRelease()
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.pollInterval):
		}
	}
}

// processOne polls once and handles the claimed task. It reports true when
// a task was claimed: the slot it holds is then owned by the container
// monitor goroutine (which must release it).
func (r *Runner) processOne(ctx context.Context) bool {
	resp, err := r.client.Poll(ctx, api.PollReq{Name: r.name, Arch: r.cfg.WorkerArch})
	if err != nil {
		if needsReregister(err) {
			r.reregister(ctx)
		} else {
			log.Printf("host: poll: %v", err)
		}
		return false
	}
	if len(resp.CancelledTaskIDs) > 0 {
		r.cancelTasks(ctx, resp.CancelledTaskIDs)
	}
	if resp.Task == nil {
		return false
	}
	go r.handleTask(ctx, resp.Task, resp.ClaimToken)
	return true
}

// handleTask launches the one-shot agent container for a claimed task
// (DETAIL §11.4 item 2): pull the image first (VARVE_PULL_IMAGE, default
// true) unless a container is already running for it, then run it with the
// one-shot environment, and hand the slot to the monitor. Any launch
// failure is reported as failed(stage=container) and the slot is released.
func (r *Runner) handleTask(ctx context.Context, task *api.TaskDetail, claimToken string) {
	// Container lifecycle operations must survive a shutdown that starts
	// while the task is being launched.
	cctx := context.WithoutCancel(ctx)
	if ctx.Err() != nil {
		log.Printf("host: dropping task %s claimed at shutdown", task.ID)
		r.slotRelease()
		return
	}
	if r.cfg.PullImage {
		if err := r.rt.Pull(cctx, r.cfg.Image); err != nil {
			r.reportResult(cctx, task.ID, claimToken,
				failedResult("container", "pull image: "+err.Error()))
			r.slotRelease()
			return
		}
	}
	id, err := r.rt.Run(cctx, r.cfg.Image, r.containerEnv(task.ID, claimToken), 0, "")
	if err != nil {
		r.reportResult(cctx, task.ID, claimToken,
			failedResult("container", "run container: "+err.Error()))
		r.slotRelease()
		return
	}
	go r.monitor(ctx, task, claimToken, id)
}

// containerEnv is the one-shot agent environment (DETAIL §11.4 item 2).
// The shared VARVE_TOKEN is deliberately never injected (decisions A10/A26:
// task endpoints authenticate by the claim token only).
func (r *Runner) containerEnv(taskID, claimToken string) []string {
	return []string{
		"VARVE_ROLE=agent",
		"VARVE_ONE_SHOT=1",
		"VARVE_TASK_ID=" + taskID,
		"VARVE_TASK_TOKEN=" + claimToken,
		"VARVE_CONTROLLER_URL=" + r.cfg.ControllerURL,
	}
}

// monitor runs one container to completion, owns the acquired slot until
// the container is gone, and reports the outcome (DETAIL §11.4 item 2-4).
// It waits for the container exit (on a context that survives shutdown so
// a running container can finish during the graceful drain), then reads
// the exit code/OOMKilled via Inspect and classifies: exit 0 is left to
// the in-container agent, non-zero is reported failed(stage=container),
// a cancellation kill is reported cancelled, and a run past the per-task
// build timeout is killed and reported failed(stage=timeout).
func (r *Runner) monitor(ctx context.Context, task *api.TaskDetail, claimToken, containerID string) {
	defer r.slotRelease()
	defer r.untrack(task.ID)
	r.track(task.ID, containerID)

	waitCtx := context.WithoutCancel(ctx)
	waitDone := make(chan waitResult, 1)
	go func() {
		code, err := r.rt.Wait(waitCtx, containerID)
		waitDone <- waitResult{exitCode: code, err: err}
	}()

	var deadline time.Time
	if task.Build.TimeoutSeconds > 0 {
		deadline = r.now().Add(time.Duration(task.Build.TimeoutSeconds) * time.Second)
	}

	res, timedOut := r.awaitExit(ctx, waitDone, deadline)
	if timedOut {
		// Build timeout: force kill, then wait briefly for the exit code
		// so the container is truly gone before reporting.
		log.Printf("host: task %s exceeded its build timeout, killing container %s", task.ID, containerID)
		if err := r.rt.Kill(waitCtx, containerID); err != nil {
			log.Printf("host: kill container %s: %v", containerID, err)
		}
		select {
		case res = <-waitDone:
			if res.err != nil {
				log.Printf("host: wait after kill %s: %v", containerID, res.err)
			}
		case <-time.After(r.timeoutCheck * 10):
		}
		r.reportResult(waitCtx, task.ID, claimToken,
			failedResult("timeout", fmt.Sprintf("build exceeded the timeout of %d seconds", task.Build.TimeoutSeconds)))
		return
	}
	r.classify(ctx, task, claimToken, containerID, res)
}

// waitResult carries the outcome of rt.Wait.
type waitResult struct {
	exitCode int
	err      error
}

// awaitExit waits for the container to exit, enforcing the per-task
// deadline. When ctx is cancelled (shutdown) it switches to a drain wait:
// the container may finish on its own, and the deadline still force-kills
// it as the fallback (DETAIL §11.4 item 5).
func (r *Runner) awaitExit(ctx context.Context, waitDone chan waitResult, deadline time.Time) (waitResult, bool) {
	tick := time.NewTicker(r.timeoutCheck)
	defer tick.Stop()
	check := func() bool {
		return !deadline.IsZero() && r.now().After(deadline)
	}
	for {
		select {
		case res := <-waitDone:
			return res, false
		case <-tick.C:
			if check() {
				return waitResult{}, true
			}
		case <-ctx.Done():
			for {
				select {
				case res := <-waitDone:
					return res, false
				case <-tick.C:
					if check() {
						return waitResult{}, true
					}
				}
			}
		}
	}
}

// classify reports the outcome of a finished container (DETAIL §11.4
// item 2): a cancelled container is reported cancelled, exit 0 is left to
// the in-container agent (the host does nothing), any other exit is
// reported failed(stage=container) with the exit code/signal and OOM in
// the summary.
func (r *Runner) classify(ctx context.Context, task *api.TaskDetail, claimToken, containerID string, res waitResult) {
	cctx := context.WithoutCancel(ctx)
	if r.isCancelled(task.ID) {
		r.reportResult(cctx, task.ID, claimToken, api.ResultReq{Status: "cancelled"})
		return
	}
	st, err := r.rt.Inspect(cctx, containerID)
	if err != nil {
		// --rm containers vanish on exit; fall back to the exit code Wait
		// observed, or report the wait failure itself.
		if res.err != nil {
			r.reportResult(cctx, task.ID, claimToken,
				failedResult("container", "wait: "+res.err.Error()))
			return
		}
		st = ContainerStatus{ExitCode: res.exitCode}
	}
	if st.ExitCode == 0 {
		return // the in-container agent already reported success
	}
	r.reportResult(cctx, task.ID, claimToken,
		failedResult("container", containerSummary(st)))
}

// containerSummary builds the failure summary: exit code, signal when the
// code encodes one (128+signum), and "OOM" for OOMKilled.
func containerSummary(st ContainerStatus) string {
	s := fmt.Sprintf("container exited with code %d", st.ExitCode)
	if st.ExitCode >= 128 {
		s += fmt.Sprintf(" (killed by signal %d)", st.ExitCode-128)
	}
	if st.OOMKilled {
		s += " (OOM)"
	}
	return s
}

// failedResult builds a failed result payload.
func failedResult(stage, summary string) api.ResultReq {
	return api.ResultReq{
		Status: "failed",
		Error:  &api.ResultError{Stage: stage, Summary: summary},
	}
}

// reportResult posts a task result. A 409 conflict means the controller
// already accepted a result for this task (the in-container agent reported
// first); it is ignored (DETAIL §11.4 item 2).
func (r *Runner) reportResult(ctx context.Context, taskID, token string, res api.ResultReq) {
	if err := r.client.ReportResult(ctx, taskID, token, res); err != nil {
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
			return
		}
		log.Printf("host: report result for task %s: %v", taskID, err)
	}
}

// slotRelease returns one capacity slot.
func (r *Runner) slotRelease() {
	<-r.slots
}
