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
// every package into a temporary GNUPGHOME (DESIGN §7.7): the armored
// private key is imported once, then each package gets
// "gpg --batch --pinentry-mode loopback --passphrase <pass> --detach-sign".
// It returns the created .sig paths. The caller owns the GNUPGHOME
// lifecycle (created here, removed by the caller at task end; the
// container teardown is the backstop).
//
// A key-claim failure or any gpg failure fails the task (stage=sign,
// DETAIL §12.5).
func (r *Runner) signPackages(ctx context.Context, task *api.TaskDetail, token string,
	pkgs []string, gnupgHome string, w io.Writer) ([]string, error) {
	km, err := r.client.GetSigningKey(ctx, task.ID, token)
	if err != nil {
		return nil, fmt.Errorf("claim signing key: %w", err)
	}
	if err := os.MkdirAll(gnupgHome, 0o700); err != nil {
		return nil, fmt.Errorf("create gnupg home: %w", err)
	}
	keyFile := filepath.Join(gnupgHome, "private.asc")
	if err := os.WriteFile(keyFile, []byte(km.ArmoredPrivateKey), 0o600); err != nil {
		return nil, fmt.Errorf("write signing key: %w", err)
	}
	env := append(os.Environ(), "GNUPGHOME="+gnupgHome)

	exit, err := runCmd(ctx, r.execCommand, "", w, env, "gpg", "--batch", "--import", keyFile)
	if err != nil || exit != 0 {
		return nil, fmt.Errorf("gpg import: exit %d: %w", exit, err)
	}

	sigs := make([]string, 0, len(pkgs))
	for _, pkg := range pkgs {
		exit, err := runCmd(ctx, r.execCommand, "", w, env, "gpg",
			"--batch", "--pinentry-mode", "loopback",
			"--passphrase", km.Passphrase, "--detach-sign", pkg)
		if err != nil || exit != 0 {
			return nil, fmt.Errorf("gpg detach-sign %s: exit %d: %w", filepath.Base(pkg), exit, err)
		}
		sigs = append(sigs, pkg+".sig")
	}
	return sigs, nil
}
