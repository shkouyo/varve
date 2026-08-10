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

// Package host implements the varve-worker host mode: a node that
// registers with the controller, heartbeats with system metrics and
// running-container state, polls for tasks, runs each task in its own
// one-shot agent container, monitors the container exit code (classifying
// normal / killed / OOM / timeout outcomes), reports results on the
// agent's behalf when it cannot, and deregisters on graceful shutdown. The
// host itself never runs makepkg.
//
// Tests in this package replace package-level stubs (execCommand) and must
// not run t.Parallel.
package host

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"git.0x0f.dev/varve/internal/api"
	"git.0x0f.dev/varve/internal/config"
)

// version is the node version reported at registration. cmd/varve-worker
// may override it later if a build-time version is introduced.
const version = "0.1.0"

// client is the narrowed worker protocol surface consumed by this module
// (the consumer defines the interface). *api.Client satisfies it.
type client interface {
	Register(ctx context.Context, req api.RegisterReq) (*api.RegisterResp, error)
	Heartbeat(ctx context.Context, req api.HeartbeatReq) (*api.HeartbeatResp, error)
	Poll(ctx context.Context, req api.PollReq) (*api.PollResp, error)
	ReportResult(ctx context.Context, taskID, token string, res api.ResultReq) error
	Deregister(ctx context.Context, name string) error
}

// runtime is the container-runtime surface consumed by this module
// (docker/podman CLI). It is the test substitute point for all container
// operations.
type runtime interface {
	Pull(ctx context.Context, image string) error
	// Run starts a detached container ("run -d") and returns its ID.
	Run(ctx context.Context, image string, env []string, cpuLimit int, memLimit string) (containerID string, err error)
	Kill(ctx context.Context, id string) error
	// Inspect reads the container's final state after exit.
	Inspect(ctx context.Context, id string) (ContainerStatus, error)
	// Wait blocks until the container exits and returns its exit code.
	Wait(ctx context.Context, id string) (exitCode int, err error)
}

// ContainerStatus is the post-exit state read by Inspect.
type ContainerStatus struct {
	ExitCode  int
	OOMKilled bool
	Running   bool
}

// containerRun tracks one running container of the node, keyed by task ID.
type containerRun struct {
	taskID    string
	id        string
	cancelled bool // set when the controller's cancel signal killed it
}

// Runner is a host-mode worker node. It is constructed via NewRunner and
// driven by Run; the zero value is not usable.
//
// Concurrency: after NewRunner, the public entry point is Run, which owns
// the node's lifecycle. All internal state is guarded by mu; slots is a
// buffered channel counting the containers in flight (capacity semaphore).
type Runner struct {
	cfg     *config.WorkerConfig
	client  client
	rt      runtime
	name    string
	dataDir string

	slots   chan struct{}  // capacity semaphore: one token per running container
	metrics *metricsReader // system metrics sampler (/proc, path injectable)

	now func() time.Time

	mu         sync.Mutex
	containers map[string]*containerRun // taskID → running container

	// Intervals and timeouts; injectable by same-package tests (clock
	// injection).
	pollInterval       time.Duration // retry delay when a poll yields no task (5s)
	heartbeatInterval  time.Duration // heartbeat period (30s)
	timeoutCheck       time.Duration // monitor deadline check granularity
	drainInterval      time.Duration // shutdown drain poll interval
	drainCap           time.Duration // force-kill bound for containers without a build timeout
	registerBackoff    time.Duration // initial register retry delay
	registerBackoffMax time.Duration // register retry delay cap
	deregisterTimeout  time.Duration // per-call timeout for the final deregister
}

var (
	_ client  = (*api.Client)(nil)
	_ runtime = (*containerRuntime)(nil)
)

// NewRunner constructs a host runner for cfg. Preconditions: cfg.Role=host
// and cfg.Image non-empty. It probes the container runtime
// (VARVE_CONTAINER_RUNTIME override, else docker → podman via "command
// -v"), resolves the stable node name (VARVE_WORKER_NAME or a persisted
// auto-generated name) and builds the capacity semaphore. It returns an
// error (the *Runner-only signature cannot express it) when no container
// runtime is available or the node name cannot be resolved, so the caller
// fails startup.
func NewRunner(cfg *config.WorkerConfig, client client) (*Runner, error) {
	if cfg.Role != "host" {
		return nil, fmt.Errorf("host: NewRunner requires cfg.Role=host, got %q", cfg.Role)
	}
	if cfg.Image == "" {
		return nil, errors.New("host: NewRunner requires VARVE_WORKER_IMAGE")
	}
	bin, err := detectRuntime(cfg.ContainerRuntime)
	if err != nil {
		return nil, err
	}
	name, err := resolveName(cfg)
	if err != nil {
		return nil, fmt.Errorf("host: resolve worker name: %w", err)
	}
	return newRunner(cfg, client, &containerRuntime{bin: bin}, name, cfg.DataDir), nil
}

// newRunner is the fully-wired constructor used by NewRunner and tests.
func newRunner(cfg *config.WorkerConfig, client client, rt runtime, name, dataDir string) *Runner {
	cap := cfg.Concurrency
	if cap < 1 {
		cap = 1
	}
	return &Runner{
		cfg:                cfg,
		client:             client,
		rt:                 rt,
		name:               name,
		dataDir:            dataDir,
		slots:              make(chan struct{}, cap),
		metrics:            newMetricsReader("/proc", dataDir),
		now:                time.Now,
		containers:         make(map[string]*containerRun),
		pollInterval:       5 * time.Second,
		heartbeatInterval:  30 * time.Second,
		timeoutCheck:       100 * time.Millisecond,
		drainInterval:      100 * time.Millisecond,
		drainCap:           30 * time.Second,
		registerBackoff:    5 * time.Second,
		registerBackoffMax: 60 * time.Second,
		deregisterTimeout:  30 * time.Second,
	}
}

// needsReregister reports whether a node-level error means the node was
// deleted or its credentials rotated: heartbeat/poll then re-register
// (idempotent upsert).
func needsReregister(err error) bool {
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusNotFound
}

// reregister re-registers the node after a 401/404 (best effort).
func (r *Runner) reregister(ctx context.Context) {
	log.Printf("host: node %q re-registering after 401/404", r.name)
	if _, err := r.client.Register(ctx, r.registerReq()); err != nil {
		log.Printf("host: re-register %s: %v", r.name, err)
	}
}

func (r *Runner) registerReq() api.RegisterReq {
	return api.RegisterReq{
		Name:     r.name,
		Role:     "host",
		Mode:     "host",
		Arch:     r.cfg.WorkerArch,
		Capacity: r.cfg.Concurrency,
		Version:  version,
	}
}
