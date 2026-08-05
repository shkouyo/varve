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
	"os/exec"
	"syscall"
	"time"
)

// terminate stops a running build command (DETAIL §12.4 #3, decision A3):
// SIGTERM to the process group, escalated to SIGKILL after killGrace if it
// has not exited. The command must have been started with Setpgid so the
// whole process group is addressable. It blocks until the process is gone.
func (r *Runner) terminate(cmd *exec.Cmd, done <-chan struct{}) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	// A process that already exited makes the signal fail with ESRCH;
	// that is fine — done fires immediately in that case.
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-done:
		return
	case <-time.After(r.killGrace):
		// SIGKILL cannot be ignored; the wait is bounded.
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-done
	}
}
