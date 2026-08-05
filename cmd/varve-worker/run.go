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

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"git.0x0f.dev/varve/internal/api"
	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/worker/agent"
	"git.0x0f.dev/varve/internal/worker/host"
)

// runner is the lifecycle surface every worker mode exposes; both
// host.Runner and agent.Runner satisfy it (DETAIL §14.2).
type runner interface {
	Run(ctx context.Context) error
}

// newHostRunner and newAgentRunner are the runner constructors used by
// run. They are package-level variables so tests can replace them with
// recorders (DETAIL §14.3); the defaults wire the real modules.
var (
	newHostRunner  = defaultHostRunner
	newAgentRunner = defaultAgentRunner
)

// defaultHostRunner builds a host-mode runner. It returns an error when
// the container runtime or the node name cannot be resolved, failing
// startup (DETAIL §11.5).
func defaultHostRunner(cfg *config.WorkerConfig, client *api.Client) (runner, error) {
	return host.NewRunner(cfg, client)
}

// defaultAgentRunner builds an agent runner (one-shot or pool, selected
// by cfg.OneShot, DETAIL §12.2).
func defaultAgentRunner(cfg *config.WorkerConfig, client *api.Client) runner {
	return agent.NewRunner(cfg, client)
}

// run is the testable entry point of the worker binary (DETAIL §14.2).
//
// It loads the worker configuration — exported environment variables,
// the optional CWD .env file and built-in defaults, in that precedence
// (DESIGN §8.3) — and dispatches to the host runner (default) or the
// agent runner by VARVE_ROLE. Required-field validation (ControllerURL
// always; Token and Image for host; TaskID/TaskToken for one-shot
// agents without Token, decision A26; Token for pool agents) happens
// inside config.LoadWorker, so a missing controller URL fails startup
// immediately (proposal §17.3). A SIGTERM/SIGINT cancels the runner
// context: the host drains running containers and deregisters, the pool
// agent idles out — the runners return nil on that graceful path.
//
// args is currently unused: the worker binary takes no subcommands; the
// parameter keeps run a stable entry point for tests and future use.
func run(args []string) error {
	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := api.NewClient(cfg.ControllerURL, cfg.Token)
	switch cfg.Role {
	case "host":
		r, err := newHostRunner(cfg, client)
		if err != nil {
			return err
		}
		return r.Run(ctx)
	case "agent":
		return newAgentRunner(cfg, client).Run(ctx)
	default:
		// Unreachable: config.LoadWorker rejects any other VARVE_ROLE
		// (DETAIL §1.4 rule 4). Guarded for clarity.
		return fmt.Errorf("varve-worker: unsupported role %q", cfg.Role)
	}
}
