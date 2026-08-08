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

// Command varve is the controller binary of the varve build system: the
// single assembly point of every controller module. It serves the worker
// API and the web UI, or rebuilds the SQLite index from the storage side
// files.
//
// Usage:
//
//	varve                  # default: serve (config /data/varve.toml)
//	varve --config <path>  # serve with an explicit config file
//	varve rebuild-index    # rebuild the SQLite index from side files
//
// Tests in this package replace package-level constructors (newSigner,
// newOrchestrator, newDetector, startServer, waitSignal) and must not run
// t.Parallel.
package main

import (
	"fmt"
	"os"
)

// main runs the controller and exits non-zero on any startup or fatal
// runtime error, so that containers and process supervisors observe the
// failure.
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "varve: %v\n", err)
		os.Exit(1)
	}
}

// defaultConfigPath is the controller configuration location.
const defaultConfigPath = "/data/varve.toml"

// run dispatches the command line: "serve" is the default, and both
// subcommands accept the optional "--config <path>" pair. The testable
// entry points are runServe and runRebuildIndex.
func run(args []string) error {
	if len(args) == 0 {
		return runServe(nil)
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "rebuild-index":
		return runRebuildIndex(args[1:])
	case "--config":
		return runServe(args)
	default:
		return fmt.Errorf("varve: unknown subcommand %q (usage: varve [serve] [--config <path>] | varve rebuild-index [--config <path>])", args[0])
	}
}

// configPath extracts the configuration path from args: the optional
// "--config <path>" pair, or the default when args is empty. Anything
// else is a usage error.
func configPath(args []string) (string, error) {
	switch len(args) {
	case 0:
		return defaultConfigPath, nil
	case 2:
		if args[0] == "--config" {
			return args[1], nil
		}
	}
	return "", fmt.Errorf("varve: unexpected arguments %v (usage: varve [serve] [--config <path>] | varve rebuild-index [--config <path>])", args)
}
