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

package db

import (
	"context"
	"testing"
	"time"
)

var testCtx = context.Background()

// newTestStore opens an in-memory store and registers its cleanup.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// newFileTestStore opens a file-backed store (real WAL) under a temp dir.
func newFileTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("Open(file): %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// at returns a fixed UTC instant plus an offset, for deterministic
// timestamps across tests.
func at(offset time.Duration) time.Time {
	return time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC).Add(offset)
}

// seedPackage inserts a packages row directly for tests. There is no
// public package-insert method by design; tests seed rows through the
// write connection.
func seedPackage(t *testing.T, s *Store, p Package) Package {
	t.Helper()
	if p.Pkgbase == "" {
		t.Fatal("seedPackage requires a pkgbase")
	}
	maintainers, err := encodeJSON(p.Maintainers)
	if err != nil {
		t.Fatalf("encode maintainers: %v", err)
	}
	enabled := 0
	if p.Enabled {
		enabled = 1
	}
	res, err := s.write.Exec(`INSERT INTO packages
		(pkgbase, branch, vcs_kind, arch, enabled, current_version, pkgdesc, last_srcinfo_hash, last_upstream_ref, last_build_id, maintainers)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`,
		p.Pkgbase, p.Branch, p.VCSKind, p.Arch, enabled, p.CurrentVersion, p.Pkgdesc,
		p.LastSrcinfoHash, p.LastUpstreamRef, maintainers)
	if err != nil {
		t.Fatalf("seed package %q: %v", p.Pkgbase, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("seed package %q: %v", p.Pkgbase, err)
	}
	p.ID = id
	return p
}

// mustSeedPackage seeds a minimal x86_64 package.
func mustSeedPackage(t *testing.T, s *Store, pkgbase string) Package {
	t.Helper()
	return seedPackage(t, s, Package{
		Pkgbase:     pkgbase,
		Branch:      "main",
		VCSKind:     "",
		Arch:        "x86_64",
		Enabled:     true,
		Maintainers: []string{"alice@example.com"},
	})
}

// newTaskBuild pairs a task with its mirrored build row for CreateTask.
func newTaskBuild(id, state string, pkg Package, at time.Time) (*Task, *Build) {
	return &Task{
			ID:             id,
			PackageID:      pkg.ID,
			State:          state,
			CreatedAt:      at,
			LastProgressAt: at,
		}, &Build{
			PackageID:   pkg.ID,
			Branch:      pkg.Branch,
			Commit:      "deadbeef",
			SrcinfoHash: "srcinfo-hash",
		}
}

// createTask seeds the package and creates the task in one helper.
func createTask(t *testing.T, s *Store, id, state string, pkg Package, at time.Time) (*Task, *Build) {
	t.Helper()
	task, build := newTaskBuild(id, state, pkg, at)
	if err := s.CreateTask(testCtx, task, build); err != nil {
		t.Fatalf("CreateTask(%s): %v", id, err)
	}
	return task, build
}

// registerWorker registers a worker and returns it with its ID filled.
func registerWorker(t *testing.T, s *Store, name string, capacity int) *Worker {
	t.Helper()
	w := &Worker{
		Name:     name,
		Role:     "agent",
		Mode:     "pool",
		Arch:     "x86_64",
		Capacity: capacity,
		Status:   "online",
		Version:  "test",
	}
	if err := s.RegisterWorker(testCtx, w); err != nil {
		t.Fatalf("RegisterWorker(%s): %v", name, err)
	}
	return w
}
