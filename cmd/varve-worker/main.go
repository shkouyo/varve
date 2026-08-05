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

// Command varve-worker is the worker-side binary of the varve build
// system (proposal §5.2–5.3, DESIGN §2.12, DETAIL §14). It loads the
// worker configuration from the environment — including the optional
// CWD .env file — and dispatches to the host runner (default) or the
// in-container agent runner by VARVE_ROLE. It takes no subcommands.
package main

import (
	"fmt"
	"os"
)

// main runs the worker and exits non-zero on any startup or fatal
// runtime error, so that containers and process supervisors observe
// the failure (DETAIL §14.2).
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "varve-worker: %v\n", err)
		os.Exit(1)
	}
}
