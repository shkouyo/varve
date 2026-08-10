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
	"log"
	"time"
)

// stopTimeout bounds the wait for the scheduler goroutine to halt after
// its context is cancelled. The happy path exits promptly; a pass stuck
// in a hung external call (SMTP relay, GitHub API) could otherwise hold
// Stop forever and hang the whole graceful shutdown. Tests may shorten
// it.
var stopTimeout = 30 * time.Second

// Stop halts the periodic scheduler goroutine, then waits for the ingest
// mutex to drain so no ingest orchestration is in flight when the caller
// shuts the servers down. It is idempotent; the concrete
// *OrchestratorImpl exposes it (cmd/varve calls it on SIGTERM; the
// interface intentionally does not carry it).
func (o *OrchestratorImpl) Stop() {
	o.stopOnce.Do(func() {
		o.schedCancel()
		select {
		case <-o.schedDone:
		case <-time.After(stopTimeout):
			log.Printf("dispatch: stop: scheduler did not halt within %s, proceeding", stopTimeout)
		}
		o.ingestMu.Lock()
		o.ingestMu.Unlock()
	})
}
