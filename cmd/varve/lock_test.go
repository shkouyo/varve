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

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAcquireLock covers the flock contract: a second acquisition of the
// same path fails while the first is held (flock locks belong to the
// open file description, so two opens in one process conflict the same
// way two processes do), succeeds again after release, and the lock file
// is created with 0600.
func TestAcquireLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "varve.db.lock")

	release1, err := acquireLock(path)
	if err != nil {
		t.Fatalf("acquireLock: %v", err)
	}
	if _, err := acquireLock(path); err == nil {
		t.Fatal("second acquireLock succeeded while the first is held")
	}
	release1()
	if _, err := acquireLock(path); err != nil {
		t.Fatalf("acquireLock after release: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("lock file missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("lock file mode = %o, want 600", perm)
	}
}
