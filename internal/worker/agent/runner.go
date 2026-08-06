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

// Package agent implements the worker agent that runs inside build
// containers. It claims tasks, executes the canonical 9-step build flow
// (hooks + makepkg → collect → sign → upload → report), buffers build logs,
// samples container cgroup stats and handles controller-requested
// cancellation. One-shot mode handles a single task and exits; pool mode
// registers as a capacity-1 node, polls for tasks and idles out after
// VARVE_POOL_IDLE_TIMEOUT. The agent never parses dotfiles: the controller
// dispatches the resolved configuration in TaskDetail.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"git.0x0f.dev/varve/internal/api"
	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/db"
)

// Stage enumeration used in failure reports, build logs and progress
// payloads.
const (
	stagePrepare   = "prepare"
	stagePreBuild  = "hook:pre_build"
	stageMakepkg   = "makepkg"
	stagePostBuild = "hook:post_build"
	stageCollect   = "collect"
	stageSign      = "sign"
	stageUpload    = "upload"
	stageReport    = "report"
	stageOnFailure = "hook:on_failure"
	stageOnSuccess = "hook:on_success"
)

// Result status values.
const (
	statusSucceeded = "succeeded"
	statusFailed    = "failed"
	statusCancelled = "cancelled"
)

// Outcome kinds produced by the cancellation watcher.
const (
	outcomeTimeout   = "timeout"
	outcomeCancelled = "cancelled"
)

// version is the worker version advertised at node registration.
const version = "0.1.0"

// client is the narrowed controller client consumed by the agent.
// *api.Client satisfies it. The db import exists solely for the db.Sample
// element type of api.ResultReq.ResourceUsage (the wire payloads are
// defined in dispatch and re-exported by api).
type client interface {
	Register(ctx context.Context, req api.RegisterReq) (*api.RegisterResp, error)
	Heartbeat(ctx context.Context, req api.HeartbeatReq) (*api.HeartbeatResp, error)
	Poll(ctx context.Context, req api.PollReq) (*api.PollResp, error)
	GetTask(ctx context.Context, id, token string) (*api.TaskDetail, error)
	AppendLog(ctx context.Context, id, token string, seg api.LogSegment) (*api.LogAck, error)
	ReportResult(ctx context.Context, id, token string, res api.ResultReq) error
	GetSigningKey(ctx context.Context, id, token string) (*api.KeyMaterial, error)
	UploadFile(ctx context.Context, id, token, name string, r io.Reader, size, offset int64) (*api.FileMeta, error)
	DownloadFile(ctx context.Context, id, token, name string) (io.ReadCloser, error)
	Deregister(ctx context.Context, name string) error
}

var _ client = (*api.Client)(nil)

// mode selects the agent lifecycle.
type mode int

const (
	modeOneShot mode = iota
	modePool
)

// Runner drives the agent lifecycle. It is not safe for concurrent use:
// Run must be called once per process.
//
// execCommand, the log-buffer parameters, the sampler paths, the pool
// intervals and the clock are injectable for same-package tests.
type Runner struct {
	cfg       *config.WorkerConfig
	client    client
	mode      mode
	taskID    string
	taskToken string

	// execCommand constructs every external command (git/makepkg/hooks/
	// gpg/tar); tests replace it with a recorder.
	execCommand func(ctx context.Context, name string, arg ...string) *exec.Cmd
	// workDir is <VARVE_DATA_DIR>/work; each task gets work/<task-id>/.
	workDir string
	// sampler reads container cgroup v2 stats on demand.
	sampler *CgroupSampler
	// now is the injectable clock used for pool idle timing.
	now func() time.Time
	// state tracks the running task for heartbeats, log progress and
	// cancellation (shared between the executor, the log goroutine and
	// the pool heartbeat goroutine).
	state *taskState

	// Log-buffer tunables (injectable for tests): a batch is flushed on the
	// earlier of logThreshold bytes or logInterval.
	logThreshold int
	logInterval  time.Duration

	// Pool tunables (injectable for tests).
	pollInterval      time.Duration
	heartbeatInterval time.Duration
	// killGrace is the SIGTERM→SIGKILL escalation delay for a running
	// build command.
	killGrace time.Duration
	// registerBackoff computes the exponential backoff for a failed node
	// registration (5s doubling to a 60s cap).
	registerBackoff func(failures int) time.Duration
	// procDir is the /proc mount used for heartbeat system metrics
	// (injectable for tests).
	procDir string
	prevCPU cpuCounters
}

// NewRunner builds a Runner for the given worker configuration. The mode
// follows cfg.OneShot: one-shot agents claim their single task directly
// without registering a node; pool agents register as a capacity-1 node.
func NewRunner(cfg *config.WorkerConfig, client client) *Runner {
	r := &Runner{
		cfg:               cfg,
		client:            client,
		taskID:            cfg.TaskID,
		taskToken:         cfg.TaskToken,
		execCommand:       exec.CommandContext,
		workDir:           cfg.DataDir + "/work",
		sampler:           NewCgroupSampler(),
		now:               time.Now,
		state:             &taskState{},
		logThreshold:      64 * 1024,
		logInterval:       time.Second,
		pollInterval:      5 * time.Second,
		heartbeatInterval: 30 * time.Second,
		killGrace:         5 * time.Second,
		registerBackoff:   backoffDelay,
		procDir:           "/proc",
	}
	if cfg.OneShot {
		r.mode = modeOneShot
	} else {
		r.mode = modePool
	}
	return r
}

// Run drives the agent lifecycle. Task-level failures are reported to the
// controller and never returned as errors; only fatal errors surface (e.g.
// a one-shot GetTask failure, which leaves the container with a non-zero
// exit so the host can report on our behalf).
func (r *Runner) Run(ctx context.Context) error {
	if r.mode == modeOneShot {
		return r.runOneShot(ctx)
	}
	return r.runPool(ctx)
}

// runOneShot claims the configured task and executes it, then exits. Nodes
// are never registered.
func (r *Runner) runOneShot(ctx context.Context) error {
	if r.taskID == "" || r.taskToken == "" {
		return errors.New("agent: one-shot mode requires a task id and claim token")
	}
	task, err := r.client.GetTask(ctx, r.taskID, r.taskToken)
	if err != nil {
		// Invalidated token (403/404) or any other claim failure is
		// fatal: log it and exit non-zero, the host reports the failure.
		log.Printf("agent: get task %s: %v", r.taskID, err)
		return fmt.Errorf("agent: get task %s: %w", r.taskID, err)
	}
	r.executeTask(ctx, task, r.taskToken)
	return nil
}

// taskState tracks the running task: its id and stage (heartbeat/log
// progress payloads), the resource samples collected so far (final result)
// and the per-task cancellation channel (channel 1). All methods are safe
// for concurrent use: the task executor, the log flush goroutine and the
// pool heartbeat goroutine share one instance.
type taskState struct {
	mu      sync.Mutex
	id      string
	stage   string
	samples []db.Sample
	cancel  chan struct{}
}

// begin marks the given task as running and returns its cancellation
// channel (closed when the controller cancels the task, channel 1).
func (s *taskState) begin(id string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.id = id
	s.stage = stagePrepare
	s.samples = nil
	s.cancel = make(chan struct{})
	return s.cancel
}

// end clears the running task and releases the cancellation watcher by
// closing the per-task channel.
func (s *taskState) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		select {
		case <-s.cancel:
		default:
			close(s.cancel)
		}
	}
	s.id, s.stage, s.samples, s.cancel = "", "", nil, nil
}

// setStage updates the progress stage.
func (s *taskState) setStage(stage string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stage = stage
}

// currentStage returns the current progress stage.
func (s *taskState) currentStage() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stage
}

// addSample appends one resource sample.
func (s *taskState) addSample(sm db.Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, sm)
}

// snapshotSamples returns a copy of the accumulated samples.
func (s *taskState) snapshotSamples() []db.Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]db.Sample(nil), s.samples...)
}

// cancelTask closes the cancellation channel if id is the running task.
// It reports whether the signal was delivered.
func (s *taskState) cancelTask(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id != id || s.cancel == nil {
		return false
	}
	select {
	case <-s.cancel:
	default:
		close(s.cancel)
	}
	return true
}

// heartbeatTasks returns the heartbeat payload for the running task: one
// progress entry with a fresh resource sample (empty when idle).
func (s *taskState) heartbeatTasks(sampler *CgroupSampler) []api.TaskProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == "" {
		return []api.TaskProgress{}
	}
	sm := sampler.Sample()
	s.samples = append(s.samples, sm)
	return []api.TaskProgress{{
		TaskID:      s.id,
		Stage:       s.stage,
		CPUTimeNS:   sm.CPUTimeNS,
		MemoryBytes: sm.MemoryBytes,
		At:          sm.At,
	}}
}

// backoffDelay returns the exponential registration backoff for the given
// number of consecutive failures: 5s doubling, capped at 60s.
func backoffDelay(failures int) time.Duration {
	d := 5 * time.Second
	for i := 0; i < failures; i++ {
		d *= 2
		if d > 60*time.Second {
			return 60 * time.Second
		}
	}
	return d
}

// isTokenError reports whether err is an identity/token failure (401/403/
// 404): the one-shot agent exits on them and the pool agent re-registers.
func isTokenError(err error) bool {
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

// isConflict reports whether err is a 409 conflict (late duplicate
// reports are ignored).
func isConflict(err error) bool {
	var apiErr *api.APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict
}

// truncateSummary caps the failure summary at max characters, keeping the
// tail (summary ≤ 2000 chars).
func truncateSummary(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[len(r)-max:])
}
