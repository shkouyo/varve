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

package detect

import (
	"context"
	"time"
)

// defaultPollInterval guards against a zero poll_interval (DESIGN §8.1).
const defaultPollInterval = 5 * time.Minute

// Run polls the source mirror forever: it runs one PollOnce immediately
// and then repeats every cfg.PollInterval (DETAIL §3.4 #5). A failed
// round is logged and the loop continues; cancelling ctx exits cleanly.
// Not safe for concurrent use with PollOnce or BranchSnapshot (§3.6).
func (d *Detector) Run(ctx context.Context) error {
	interval := d.cfg.PollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if err := d.PollOnce(ctx); err != nil {
			d.logger.Warn("detect: poll round failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
