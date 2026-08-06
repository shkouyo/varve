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
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"git.0x0f.dev/varve/internal/config"
)

// namePattern is the auto-generated node-name format
// ("proud-heron-7" style).
var namePattern = regexp.MustCompile(`^[a-z]+-[a-z]+-[0-9]+$`)

// TestResolveNameManual verifies VARVE_WORKER_NAME wins and nothing is
// persisted.
func TestResolveNameManual(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.WorkerConfig{WorkerName: "my-node", DataDir: dir}
	name, err := resolveName(cfg)
	if err != nil {
		t.Fatalf("resolveName: %v", err)
	}
	if name != "my-node" {
		t.Errorf("name = %q, want my-node", name)
	}
	if _, err := os.Stat(filepath.Join(dir, workerNameFile)); !os.IsNotExist(err) {
		t.Errorf("manual name must not create the persistence file (stat err = %v)", err)
	}
}

// TestPersistedName verifies the name is generated, stored and stable
// across restarts.
func TestPersistedName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, workerNameFile)

	n1, err := persistedName(path)
	if err != nil {
		t.Fatalf("persistedName: %v", err)
	}
	if !namePattern.MatchString(n1) {
		t.Errorf("generated name %q does not match %v", n1, namePattern)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("persistence file: %v", err)
	}
	if got := string(b); got != n1+"\n" {
		t.Errorf("file content = %q, want %q", got, n1+"\n")
	}

	n2, err := persistedName(path)
	if err != nil {
		t.Fatalf("persistedName (second read): %v", err)
	}
	if n2 != n1 {
		t.Errorf("name changed across reads: %q then %q", n1, n2)
	}
}

// TestGenerateNameFormat verifies the adjective-animal-number format over
// many draws and that the word lists are non-empty.
func TestGenerateNameFormat(t *testing.T) {
	for i := 0; i < 100; i++ {
		if !namePattern.MatchString(generateName()) {
			t.Fatalf("generateName() = %q does not match %v", generateName(), namePattern)
		}
	}
	if len(adjectives) == 0 || len(animals) == 0 {
		t.Fatal("word lists must not be empty")
	}
}
