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

package host

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"

	"git.0x0f.dev/varve/internal/config"
)

// workerNameFile is the node-name persistence file relative to the data
// directory: a host keeps its auto-generated name across restarts.
const workerNameFile = "worker-name"

// adjectives and animals are the built-in word lists for auto-generated
// node names ("proud-heron-7" style).
var adjectives = []string{
	"proud", "swift", "brave", "calm", "clever", "eager", "gentle", "lively",
	"mellow", "nimble", "quiet", "rustic", "steady", "sunny", "witty", "zippy",
}

var animals = []string{
	"heron", "otter", "falcon", "lynx", "badger", "beaver", "crane", "dolphin",
	"elk", "fox", "gecko", "hawk", "ibis", "jaguar", "koala", "lark",
}

// resolveName returns the node name: VARVE_WORKER_NAME when set, otherwise
// the persisted auto-generated name.
func resolveName(cfg *config.WorkerConfig) (string, error) {
	if cfg.WorkerName != "" {
		return cfg.WorkerName, nil
	}
	return persistedName(filepath.Join(cfg.DataDir, workerNameFile))
}

// persistedName reads a previously stored name, or generates and persists
// a new one, so a host node keeps its identity across restarts.
func persistedName(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if name := strings.TrimSpace(string(b)); name != "" {
			return name, nil
		}
	}
	name := generateName()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(name+"\n"), 0o644); err != nil {
		return "", err
	}
	return name, nil
}

// generateName builds a readable node name: adjective-animal-number
// (e.g. "proud-heron-7").
func generateName() string {
	return fmt.Sprintf("%s-%s-%d", pick(adjectives), pick(animals), rand.IntN(1000))
}

// pick returns a uniformly random element of list.
func pick(list []string) string {
	return list[rand.IntN(len(list))]
}
