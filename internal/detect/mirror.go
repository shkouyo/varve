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

package detect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// mirrorTimeout bounds every mirror maintenance command so a hung network
// never wedges the poll loop. Tests may shorten it.
var mirrorTimeout = 60 * time.Second

// ensureMirror clones the source repository as a bare mirror on first use
// and fetches it afterwards, pruning vanished branches (DESIGN §2.3,
// DETAIL §3.4 #1).
func (d *Detector) ensureMirror(ctx context.Context) error {
	if _, err := os.Stat(d.mirrorDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return d.cloneMirror(ctx)
		}
		return fmt.Errorf("detect: stat mirror %s: %w", d.mirrorDir, err)
	}
	return d.fetchMirror(ctx)
}

// cloneMirror runs the initial "git clone --mirror" into d.mirrorDir.
func (d *Detector) cloneMirror(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, mirrorTimeout)
	defer cancel()
	cmd := d.execCommand(cctx, "git", "clone", "--mirror", d.cfg.URL, d.mirrorDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("detect: clone mirror %s: %w: %s", d.cfg.URL, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// fetchMirror updates an existing mirror, fast-forwarding every head and
// pruning branches deleted upstream.
func (d *Detector) fetchMirror(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, mirrorTimeout)
	defer cancel()
	cmd := d.execCommand(cctx, "git", "-C", d.mirrorDir,
		"fetch", "origin", "+refs/heads/*:refs/heads/*", "--prune")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("detect: fetch mirror: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
