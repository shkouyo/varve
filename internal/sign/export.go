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

package sign

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrAlreadyExported is returned by ExportForTask when the same task
// already claimed its one-shot key material; the API maps it to HTTP 409.
var ErrAlreadyExported = errors.New("sign: key already exported for task")

// ExportForTask returns the signing key material for a task and caches it
// in memory; each task may claim the material only once. The armored
// private key is exported from the managed keyring with
// gpg --export-secret-keys --armor, and Passphrase is the configured key
// passphrase. Repeated calls for the same task return ErrAlreadyExported.
// The subprocess is bounded by gpgCmdTimeout on top of ctx, so a hung gpg
// can never block the caller forever. Concurrently safe.
func (s *Signer) ExportForTask(ctx context.Context, taskID string) (*KeyMaterial, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cache[taskID]; ok {
		return nil, ErrAlreadyExported
	}
	cmdCtx, cancel := context.WithTimeout(ctx, gpgCmdTimeout)
	defer cancel()
	cmd := s.execCommand(cmdCtx, "gpg", "--homedir", s.gnupgHome, "--batch",
		"--pinentry-mode", "loopback", "--passphrase", s.cfg.Passphrase,
		"--export-secret-keys", "--armor", s.keyID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("sign: export secret key for task %s: %w: %s",
			taskID, err, strings.TrimSpace(string(out)))
	}
	km := &KeyMaterial{
		KeyID:             s.keyID,
		ArmoredPrivateKey: string(out),
		Passphrase:        s.cfg.Passphrase,
	}
	s.cache[taskID] = km
	return km, nil
}

// ClearTask removes the cached key material of a finished task (called by
// dispatch when the task reaches a terminal state). It is idempotent:
// clearing an unknown task succeeds. After a ClearTask, the same task may
// be exported again as a fresh one-shot claim (the callers never trigger
// this). Concurrently safe.
func (s *Signer) ClearTask(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, taskID)
}
