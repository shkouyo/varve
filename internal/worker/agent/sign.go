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
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"git.0x0f.dev/varve/internal/api"
)

// signPackages claims the task's one-shot signing key and detach-signs
// every package into a temporary GNUPGHOME outside the build tree (the
// caller picks the location). The armored private key is imported through
// stdin, so no key file ever lands on disk, and the passphrase is handed
// to gpg on fd 0 (--passphrase-fd 0) instead of argv, so no other process
// in the container can read it from /proc/<pid>/cmdline. Moving the
// GNUPGHOME out of the build tree is defense in depth only: the keyring
// is still readable by the same user, so the real protection is the
// stdin-only key and passphrase plus the caller's removal of the home
// right after signing, before the repository-supplied on_success hooks
// can run. It returns the created .sig paths. The caller owns the
// GNUPGHOME lifecycle (created here, removed by the caller once signing
// is done and at task end as the failure backstop; the container teardown
// is the last resort).
//
// A key-claim failure or any gpg failure fails the task (stage=sign).
func (r *Runner) signPackages(ctx context.Context, task *api.TaskDetail, token string,
	pkgs []string, gnupgHome string, w io.Writer) ([]string, error) {
	km, err := r.client.GetSigningKey(ctx, task.ID, token)
	if err != nil {
		return nil, fmt.Errorf("claim signing key: %w", err)
	}
	if err := os.MkdirAll(gnupgHome, 0o700); err != nil {
		return nil, fmt.Errorf("create gnupg home: %w", err)
	}
	env := withEnv(r.childEnv(), "GNUPGHOME", gnupgHome)

	// Import the armored key from stdin: no private.asc on disk.
	exit, err := runCmdIn(ctx, r.command, "", w, env, km.ArmoredPrivateKey, "gpg", "--batch", "--import")
	if err != nil || exit != 0 {
		return nil, fmt.Errorf("gpg import: exit %d: %w", exit, err)
	}

	sigs := make([]string, 0, len(pkgs))
	for _, pkg := range pkgs {
		// The passphrase rides on stdin (fd 0), never in argv.
		exit, err := runCmdIn(ctx, r.command, "", w, env, km.Passphrase+"\n", "gpg",
			"--batch", "--pinentry-mode", "loopback",
			"--passphrase-fd", "0", "--detach-sign", pkg)
		if err != nil || exit != 0 {
			return nil, fmt.Errorf("gpg detach-sign %s: exit %d: %w", filepath.Base(pkg), exit, err)
		}
		sigs = append(sigs, pkg+".sig")
	}
	return sigs, nil
}
