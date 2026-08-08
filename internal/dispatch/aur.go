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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"git.0x0f.dev/varve/internal/config"
)

// execCommand is the command constructor used for every external git call;
// same-package tests may replace it with a recorder.
var execCommand = exec.CommandContext

// AURPublisher pushes the branch commits of the source mirror into an AUR
// package repository. The orchestrator owns the trigger decision (dotfile
// submit flag, commit-bearing change) and calls Push after a successful
// ingest; the interface is injectable so tests substitute a recorder.
type AURPublisher interface {
	// Push publishes the branch ref of the mirror at mirrorDir as the
	// master branch of the AUR package repository <user>@<server>:<name>.git.
	// The push is fast-forward only: git rejects a non-fast-forward update
	// without --force, which surfaces as ErrAURNonFastForward.
	Push(ctx context.Context, mirrorDir, branch, aurName string) error
}

// AUR push sentinel errors. The orchestrator records the message on the
// package row either way; the sentinel keeps the classification explicit
// for logging and tests.
var (
	// ErrAURDisabled reports a push attempt with no SSH key configured.
	ErrAURDisabled = errors.New("dispatch: AUR publishing disabled")
	// ErrAURNonFastForward reports a rejected update: the AUR master has
	// commits the source branch does not contain. The push is never
	// forced; the maintainers are notified instead.
	ErrAURNonFastForward = errors.New("dispatch: AUR push rejected: non-fast-forward")
)

// AURPusher is the git-based AURPublisher. It pushes from the controller's
// source mirror, so the branch commit history directly becomes the AUR
// master history with no extra commit. The SSH identity is injected through
// GIT_SSH_COMMAND; the identity file is mounted under /data by the
// deployment.
type AURPusher struct {
	cfg         *config.AURConfig
	execCommand func(ctx context.Context, name string, arg ...string) *exec.Cmd
}

// NewAURPusher builds an AURPusher from the controller [aur] section. A nil
// config (or an empty key_file) leaves publishing disabled.
func NewAURPusher(cfg *config.AURConfig) *AURPusher {
	if cfg == nil {
		cfg = &config.AURConfig{}
	}
	return &AURPusher{cfg: cfg, execCommand: execCommand}
}

// Push runs "git push <user>@<server>:<name>.git <branch>:master" inside
// the mirror directory. No --force is passed, so git itself refuses any
// update that is not a fast-forward; such rejections surface as
// ErrAURNonFastForward with the remote output attached.
func (p *AURPusher) Push(ctx context.Context, mirrorDir, branch, aurName string) error {
	if p.cfg == nil || p.cfg.KeyFile == "" {
		return ErrAURDisabled
	}
	remote := fmt.Sprintf("%s@%s:%s.git", p.cfg.User, p.cfg.Server, aurName)
	// IdentitiesOnly pins the identity file (no agent fallback) and
	// accept-new tolerates an unknown host without an interactive prompt.
	sshCmd := "ssh -i " + strconv.Quote(p.cfg.KeyFile) +
		" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"
	cmd := p.execCommand(ctx, "git", "-C", mirrorDir, "push", remote, branch+":master")
	cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+sshCmd)
	out, err := cmd.CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err != nil {
		if strings.Contains(msg, "non-fast-forward") || strings.Contains(msg, "[rejected]") {
			return fmt.Errorf("%w: %s", ErrAURNonFastForward, msg)
		}
		return fmt.Errorf("git push %s: %w: %s", aurName, err, msg)
	}
	return nil
}
