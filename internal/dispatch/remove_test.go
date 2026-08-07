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
	"context"
	"errors"
	"testing"

	"git.0x0f.dev/varve/internal/db"
)

// TestRemoveCascades covers the branch-vanished cascade: the updater
// removes the repository files, the package's rows disappear, the build
// logs are deleted and a second call is a no-op (idempotent).
func TestRemoveCascades(t *testing.T) {
	env := newTestEnv(t)
	env.buildSucceeded(t, "gone", testArtifacts("gone", "1.0-1"))
	pkg, err := env.store.GetPackageByBase(context.Background(), "gone")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	builds, _, err := env.store.ListBuilds(context.Background(), 1, 100, false)
	if err != nil {
		t.Fatalf("ListBuilds: %v", err)
	}
	var logs []string
	for _, b := range builds {
		if b.PackageID == pkg.ID {
			if err := env.logs.Append(b.ID, []byte("log")); err != nil {
				t.Fatalf("append log: %v", err)
			}
			logs = append(logs, b.ID)
		}
	}
	if len(logs) == 0 {
		t.Fatal("no build rows for the package")
	}

	if err := env.o.Remove(context.Background(), "gone"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if len(env.up.removed) != 1 || env.up.removed[0] != "gone" {
		t.Errorf("updater removals = %v, want [gone]", env.up.removed)
	}
	if _, err := env.store.GetPackageByBase(context.Background(), "gone"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("package still present: %v", err)
	}
	tasks, err := env.store.ListActiveTasks(context.Background())
	if err != nil {
		t.Fatalf("ListActiveTasks: %v", err)
	}
	for _, tk := range tasks {
		if tk.PackageID == pkg.ID {
			t.Errorf("task %s still present after the cascade", tk.ID)
		}
	}
	for _, id := range logs {
		if _, err := env.logs.Read(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("log %s still present: %v", id, err)
		}
	}

	// Idempotent: the package is gone, so the second call is a no-op and
	// the updater is not invoked again.
	if err := env.o.Remove(context.Background(), "gone"); err != nil {
		t.Fatalf("Remove again: %v", err)
	}
	if len(env.up.removed) != 1 {
		t.Errorf("updater removals = %v, want the single cascade", env.up.removed)
	}
}

// TestRemoveRepoFailureKeepsRows covers the abort semantics: a repository
// failure surfaces the error and keeps the rows, so the next detection
// round retries the idempotent cascade.
func TestRemoveRepoFailureKeepsRows(t *testing.T) {
	env := newTestEnv(t)
	env.enqueue(t, "stuck", "stuck")
	env.up.removeErr = errors.New("repo-remove: boom")
	if err := env.o.Remove(context.Background(), "stuck"); err == nil {
		t.Fatal("Remove with a failing updater succeeded")
	}
	if _, err := env.store.GetPackageByBase(context.Background(), "stuck"); err != nil {
		t.Errorf("package dropped despite the repo failure: %v", err)
	}
}
