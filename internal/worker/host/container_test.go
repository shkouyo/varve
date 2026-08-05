// SPDX-License-Identifier: AGPL-3.0-or-later
//
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
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// TestRunArgsMatrix verifies the "run -d" argument construction across the
// --cpus/--memory matrix: 0/"" → no flag; --rm is always present (H4).
func TestRunArgsMatrix(t *testing.T) {
	env := []string{"VARVE_ROLE=agent", "VARVE_TASK_ID=t1"}
	cases := []struct {
		name     string
		cpu      int
		mem      string
		wantArgs []string
	}{
		{"no limits", 0, "", []string{"run", "-d", "--rm",
			"--env", "VARVE_ROLE=agent", "--env", "VARVE_TASK_ID=t1"}},
		{"cpu only", 4, "", []string{"run", "-d", "--rm",
			"--env", "VARVE_ROLE=agent", "--env", "VARVE_TASK_ID=t1",
			"--cpus", "4"}},
		{"memory only", 0, "8GiB", []string{"run", "-d", "--rm",
			"--env", "VARVE_ROLE=agent", "--env", "VARVE_TASK_ID=t1",
			"--memory", "8GiB"}},
		{"both", 2, "4GiB", []string{"run", "-d", "--rm",
			"--env", "VARVE_ROLE=agent", "--env", "VARVE_TASK_ID=t1",
			"--cpus", "2", "--memory", "4GiB"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runArgs(env, tc.cpu, tc.mem)
			if !reflect.DeepEqual(got, tc.wantArgs) {
				t.Errorf("runArgs = %v, want %v", got, tc.wantArgs)
			}
		})
	}
}

// TestRunArgsAlwaysRem asserts --rm survives every limit combination.
func TestRunArgsAlwaysRem(t *testing.T) {
	for _, args := range [][]string{
		runArgs(nil, 0, ""),
		runArgs([]string{"A=1"}, 8, "16GiB"),
	} {
		found := false
		for _, a := range args {
			if a == "--rm" {
				found = true
			}
		}
		if !found {
			t.Errorf("runArgs = %v must always contain --rm", args)
		}
	}
}

// TestSimpleArgs verifies the pull/kill/wait command shapes.
func TestSimpleArgs(t *testing.T) {
	if got := pullArgs("img:latest"); !reflect.DeepEqual(got, []string{"pull", "img:latest"}) {
		t.Errorf("pullArgs = %v", got)
	}
	if got := killArgs("c1"); !reflect.DeepEqual(got, []string{"kill", "c1"}) {
		t.Errorf("killArgs = %v", got)
	}
	if got := waitArgs("c1"); !reflect.DeepEqual(got, []string{"wait", "c1"}) {
		t.Errorf("waitArgs = %v", got)
	}
	if got := inspectArgs("c1"); !reflect.DeepEqual(got, []string{
		"inspect", "--format", "{{.State.ExitCode}}|{{.State.OOMKilled}}|{{.State.Running}}", "c1",
	}) {
		t.Errorf("inspectArgs = %v", got)
	}
}

// TestParseInspect verifies the inspect output parsing (exit code, OOM,
// running) and its error path.
func TestParseInspect(t *testing.T) {
	st, err := parseInspect("0|false|false\n")
	if err != nil {
		t.Fatalf("parseInspect: %v", err)
	}
	if st.ExitCode != 0 || st.OOMKilled || st.Running {
		t.Errorf("parseInspect = %+v, want clean exit", st)
	}

	st, err = parseInspect("137|true|true")
	if err != nil {
		t.Fatalf("parseInspect: %v", err)
	}
	if st.ExitCode != 137 || !st.OOMKilled || !st.Running {
		t.Errorf("parseInspect = %+v, want 137/OOM/running", st)
	}

	if _, err := parseInspect("bogus"); err == nil {
		t.Error("parseInspect(bogus) must error")
	}
}

// probeExec builds a fake execCommand: "command -v <bin>" succeeds only
// for the binaries in available, in the given order; every other command
// succeeds (harmless /bin/true).
func probeExec(available map[string]bool, probes *[]string) func(context.Context, string, ...string) *exec.Cmd {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "command" && len(args) == 2 && args[0] == "-v" {
			*probes = append(*probes, args[1])
			if available[args[1]] {
				return exec.CommandContext(ctx, "/bin/true")
			}
			return exec.CommandContext(ctx, "/bin/false")
		}
		return exec.CommandContext(ctx, "/bin/true")
	}
}

// TestDetectRuntimeOrder verifies the docker → podman probe order and the
// VARVE_CONTAINER_RUNTIME override (H1).
func TestDetectRuntimeOrder(t *testing.T) {
	old := execCommand
	t.Cleanup(func() { execCommand = old })

	t.Run("override wins", func(t *testing.T) {
		bin, err := detectRuntime("podman")
		if err != nil || bin != "podman" {
			t.Errorf("detectRuntime(podman) = %q, %v; want podman", bin, err)
		}
	})

	t.Run("docker first", func(t *testing.T) {
		var probes []string
		execCommand = probeExec(map[string]bool{"docker": true}, &probes)
		bin, err := detectRuntime("")
		if err != nil || bin != "docker" {
			t.Errorf("detectRuntime = %q, %v; want docker", bin, err)
		}
		if len(probes) != 1 || probes[0] != "docker" {
			t.Errorf("probes = %v, want [docker] only", probes)
		}
	})

	t.Run("docker missing falls back to podman", func(t *testing.T) {
		var probes []string
		execCommand = probeExec(map[string]bool{"podman": true}, &probes)
		bin, err := detectRuntime("")
		if err != nil || bin != "podman" {
			t.Errorf("detectRuntime = %q, %v; want podman", bin, err)
		}
		if len(probes) != 2 || probes[0] != "docker" || probes[1] != "podman" {
			t.Errorf("probes = %v, want [docker podman]", probes)
		}
	})

	t.Run("both missing errors", func(t *testing.T) {
		var probes []string
		execCommand = probeExec(map[string]bool{}, &probes)
		if _, err := detectRuntime(""); err == nil {
			t.Error("detectRuntime must error when docker and podman are both unavailable")
		}
	})
}

// TestDetectRuntimeRecordsCommandV asserts the probe uses "command -v"
// (the shape the fake relies on).
func TestDetectRuntimeRecordsCommandV(t *testing.T) {
	old := execCommand
	t.Cleanup(func() { execCommand = old })
	var calls []string
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return exec.CommandContext(ctx, "/bin/false")
	}
	detectRuntime("")
	if len(calls) != 2 || calls[0] != "command -v docker" || calls[1] != "command -v podman" {
		t.Errorf("probe calls = %v, want [command -v docker command -v podman]", calls)
	}
}
