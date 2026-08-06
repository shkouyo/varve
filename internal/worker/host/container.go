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
	"os/exec"
	"strconv"
	"strings"
)

// execCommand is the command constructor for every external call: the
// container runtime CLI and the runtime probe ("command -v"). Same-package
// tests replace it with a recorder.
var execCommand = exec.CommandContext

// containerRuntime runs the docker/podman CLI. All container lifecycle
// commands go through execCommand so tests can stub them; the container
// itself is never executed by the test suite.
type containerRuntime struct {
	bin string // "docker" | "podman"
}

// Pull fetches the image ("pull <image>").
func (r *containerRuntime) Pull(ctx context.Context, image string) error {
	return execCommand(ctx, r.bin, pullArgs(image)...).Run()
}

// Run starts a detached one-shot agent container ("run -d") with the given
// environment and resource limits and returns its ID. --rm is always
// present so the container is destroyed even when it fails.
func (r *containerRuntime) Run(ctx context.Context, image string, env []string, cpuLimit int, memLimit string) (string, error) {
	args := append(runArgs(env, cpuLimit, memLimit), image)
	out, err := execCommand(ctx, r.bin, args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Kill stops a container.
func (r *containerRuntime) Kill(ctx context.Context, id string) error {
	return execCommand(ctx, r.bin, killArgs(id)...).Run()
}

// Inspect reads the container's final state (exit code + OOMKilled) after
// it exited.
func (r *containerRuntime) Inspect(ctx context.Context, id string) (ContainerStatus, error) {
	out, err := execCommand(ctx, r.bin, inspectArgs(id)...).Output()
	if err != nil {
		return ContainerStatus{}, err
	}
	return parseInspect(string(out))
}

// Wait blocks until the container exits and returns its exit code.
func (r *containerRuntime) Wait(ctx context.Context, id string) (int, error) {
	out, err := execCommand(ctx, r.bin, waitArgs(id)...).Output()
	if err != nil {
		return 0, err
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("host: parse exit code %q: %w", strings.TrimSpace(string(out)), err)
	}
	return code, nil
}

// detectRuntime resolves the container runtime binary: VARVE_CONTAINER_
// RUNTIME overrides auto-detection; otherwise docker is probed before
// podman via "command -v". Both missing → error.
func detectRuntime(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	for _, bin := range []string{"docker", "podman"} {
		if runtimeAvailable(bin) {
			return bin, nil
		}
	}
	return "", errors.New("host: no container runtime found (docker and podman are both unavailable); install one or set VARVE_CONTAINER_RUNTIME")
}

// runtimeAvailable reports whether bin is on PATH, probed with
// "command -v" so the probe is itself stubbable via execCommand.
func runtimeAvailable(bin string) bool {
	return execCommand(context.Background(), "command", "-v", bin).Run() == nil
}

// pullArgs builds "pull <image>".
func pullArgs(image string) []string {
	return []string{"pull", image}
}

// runArgs builds the "run -d" argument list (without the trailing image):
// --rm is always present (destroy even on failure); each env entry becomes
// "--env K=V"; cpuLimit > 0 → --cpus=N; memLimit != "" → --memory=<v>.
func runArgs(env []string, cpuLimit int, memLimit string) []string {
	args := []string{"run", "-d", "--rm"}
	for _, kv := range env {
		args = append(args, "--env", kv)
	}
	if cpuLimit > 0 {
		args = append(args, "--cpus", strconv.Itoa(cpuLimit))
	}
	if memLimit != "" {
		args = append(args, "--memory", memLimit)
	}
	return args
}

// killArgs builds "kill <id>".
func killArgs(id string) []string {
	return []string{"kill", id}
}

// inspectArgs formats the state via a pipe-separated Go template so
// parseInspect can split the output (docker and podman both support
// --format templates over .State).
func inspectArgs(id string) []string {
	return []string{
		"inspect",
		"--format", "{{.State.ExitCode}}|{{.State.OOMKilled}}|{{.State.Running}}",
		id,
	}
}

// waitArgs builds "wait <id>", which prints the container exit code.
func waitArgs(id string) []string {
	return []string{"wait", id}
}

// parseInspect parses the "exit|oom|running" output of inspectArgs.
func parseInspect(out string) (ContainerStatus, error) {
	parts := strings.Split(strings.TrimSpace(out), "|")
	if len(parts) != 3 {
		return ContainerStatus{}, fmt.Errorf("host: unexpected inspect output %q", out)
	}
	code, err := strconv.Atoi(parts[0])
	if err != nil {
		return ContainerStatus{}, fmt.Errorf("host: parse inspect exit code %q: %w", parts[0], err)
	}
	oom, err := strconv.ParseBool(parts[1])
	if err != nil {
		return ContainerStatus{}, fmt.Errorf("host: parse inspect oom %q: %w", parts[1], err)
	}
	running, err := strconv.ParseBool(parts[2])
	if err != nil {
		return ContainerStatus{}, fmt.Errorf("host: parse inspect running %q: %w", parts[2], err)
	}
	return ContainerStatus{ExitCode: code, OOMKilled: oom, Running: running}, nil
}
