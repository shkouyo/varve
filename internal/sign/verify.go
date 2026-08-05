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
	"fmt"
	"strings"
)

// VerifyDetached checks a detached signature over a package artifact using
// the managed keyring (gpg --verify <sig> <pkg>, DETAIL §7.4 step 3). Any
// non-zero gpg exit — a bad signature, a tampered artifact or a missing
// key — is returned as an error so that dispatch can fail the task
// (DESIGN §7.5). Concurrently safe: every call runs an isolated subprocess
// (DETAIL §7.6).
func (s *Signer) VerifyDetached(sigPath, pkgPath string) error {
	cmd := s.execCommand(context.Background(), "gpg", "--homedir", s.gnupgHome,
		"--batch", "--verify", sigPath, pkgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sign: verify %s against %s: %w: %s",
			sigPath, pkgPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// GnuPGEnv returns the environment fragment that points a child process at
// the managed keyring, consumed by repo-add --sign (DETAIL §7.4 step 4).
// Concurrently safe (read-only).
func (s *Signer) GnuPGEnv() []string {
	return []string{"GNUPGHOME=" + s.gnupgHome}
}
