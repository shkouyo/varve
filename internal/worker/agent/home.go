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

package agent

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// command builds an external command with the agent environment applied
// (a writable HOME, see childEnv) plus the current task's extra
// environment entries (the configured PACKAGER identity, see
// setTaskPackager). Every external command in the agent flow goes
// through here so a later call site cannot bypass the fix.
func (r *Runner) command(ctx context.Context, name string, arg ...string) *exec.Cmd {
	cmd := r.execCommand(ctx, name, arg...)
	if env := r.childEnv(); env != nil {
		cmd.Env = env
	}
	if extra := r.taskEnv; len(extra) > 0 {
		cmd.Env = append(cmd.Env, extra...)
	}
	return cmd
}

// setTaskPackager configures the environment entries of the currently
// executed task: the configured PACKAGER identity when non-empty, none
// otherwise. The agent runs one task at a time, so the single taskEnv
// slot is safe without further synchronization.
func (r *Runner) setTaskPackager(packager string) {
	if packager == "" {
		r.taskEnv = nil
		return
	}
	r.taskEnv = []string{"PACKAGER=" + packager}
}

// childEnv returns the environment handed to build commands. Build tools
// derive their caches from $HOME (cargo uses ~/.cargo), so a writable
// home is required; container runners can point HOME at a root-owned
// directory. GitHub Actions container jobs set HOME=/github/home and run
// steps as the image user, which makes cargo fail with
// "Permission denied (os error 13)" on ~/.cargo.
//
// The result is resolved once per runner and cached. nil means the
// children inherit the agent's environment unchanged (the common case:
// one-shot containers run with the image default /home/builder).
func (r *Runner) childEnv() []string {
	r.envOnce.Do(func() { r.env = r.resolveEnv() })
	return r.env
}

// resolveEnv replaces an unusable HOME with a private writable directory
// inside the work dir. It returns nil when the inherited HOME is already
// usable (no behavior change) and falls back to the inherited environment
// if even the work dir cannot be prepared: the command then fails with
// its original error instead of a masked one.
func (r *Runner) resolveEnv() []string {
	home := os.Getenv("HOME")
	if home != "" && writableHome(home) {
		return nil
	}
	dir := filepath.Join(r.workDir, "home")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("agent: create home dir %s: %v", dir, err)
		return nil
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		log.Printf("agent: chmod home dir %s: %v", dir, err)
	}
	return withEnv(os.Environ(), "HOME", dir)
}

// writableHome reports whether dir is an existing directory the current
// user can write to. It probes with a real file instead of trusting mode
// bits, which would ignore ACL grants and deny writes to root.
func writableHome(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return false
	}
	probe, err := os.CreateTemp(dir, ".varve-home-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name)
	return true
}

// withEnv returns a copy of env with key set to value, replacing an
// existing entry or appending one. A nil env means "inherit the process
// environment".
func withEnv(env []string, key, value string) []string {
	if env == nil {
		env = os.Environ()
	}
	out := append([]string(nil), env...)
	prefix := key + "="
	for i, e := range out {
		if strings.HasPrefix(e, prefix) {
			out[i] = prefix + value
			return out
		}
	}
	return append(out, prefix+value)
}
