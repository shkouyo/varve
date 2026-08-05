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

package db

import (
	"errors"
	"testing"
	"time"
)

// TestGetBuild asserts a full build round trip including optional fields
// and JSON columns.
func TestGetBuild(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "bld")
	// Create the task already assigned so MarkRunning can advance it.
	_, b := createTask(t, s, "bld-task", "assigned", pkg, at(0))
	if b.ID == 0 {
		t.Fatal("CreateTask did not fill build ID")
	}

	started := at(time.Second)
	finished := at(2 * time.Second)
	artifacts := []Artifact{
		{File: "foo-1.0-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "foo", Version: "1.0-1", Arch: "x86_64", Size: 100, SHA256: "aa"},
	}
	samples := []Sample{{At: at(time.Second), CPUTimeNS: 5, MemoryBytes: 1024}}

	// Mirror the build's started_at via MarkRunning flow.
	if err := s.MarkRunning(testCtx, "bld-task", started); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeTask(testCtx, "bld-task", "succeeded", "", finished, artifacts, samples)
	}); err != nil {
		t.Fatalf("FinalizeTask: %v", err)
	}

	got, err := s.GetBuild(testCtx, b.ID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if got.ID != b.ID || got.PackageID != pkg.ID || got.Branch != "main" || got.Commit != "deadbeef" {
		t.Errorf("scalar mismatch: %+v", got)
	}
	if got.Status != "succeeded" {
		t.Errorf("Status = %q, want succeeded", got.Status)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, started)
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(finished) {
		t.Errorf("FinishedAt = %v, want %v", got.FinishedAt, finished)
	}
	if got.LogPath != "logs/1.log" {
		t.Errorf("LogPath = %q, want logs/1.log", got.LogPath)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0].File != "foo-1.0-1-x86_64.pkg.tar.zst" || got.Artifacts[0].Kind != "package" {
		t.Errorf("Artifacts = %v", got.Artifacts)
	}
	if len(got.ResourceUsage) != 1 || got.ResourceUsage[0].CPUTimeNS != 5 || got.ResourceUsage[0].MemoryBytes != 1024 {
		t.Errorf("ResourceUsage = %v", got.ResourceUsage)
	}

	if _, err := s.GetBuild(testCtx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetBuild(9999) = %v, want ErrNotFound", err)
	}
}

// TestListBuilds covers failedOnly filtering and pagination.
func TestListBuilds(t *testing.T) {
	s := newTestStore(t)
	// Three builds: succeeded, failed, running.
	pkg1 := mustSeedPackage(t, s, "lb1")
	_, _ = createTask(t, s, "lb-suc", "queued", pkg1, at(0))
	if err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeTask(testCtx, "lb-suc", "succeeded", "", at(time.Second), nil, nil)
	}); err != nil {
		t.Fatalf("finalize succeeded: %v", err)
	}

	pkg2 := mustSeedPackage(t, s, "lb2")
	_, _ = createTask(t, s, "lb-fail", "queued", pkg2, at(0))
	if err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeTask(testCtx, "lb-fail", "failed", "makepkg: boom", at(2*time.Second), nil, nil)
	}); err != nil {
		t.Fatalf("finalize failed: %v", err)
	}

	pkg3 := mustSeedPackage(t, s, "lb3")
	_, _ = createTask(t, s, "lb-run", "assigned", pkg3, at(0))
	if err := s.MarkRunning(testCtx, "lb-run", at(time.Second)); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	t.Run("all", func(t *testing.T) {
		rows, total, err := s.ListBuilds(testCtx, 1, 10, false)
		if err != nil {
			t.Fatalf("ListBuilds: %v", err)
		}
		if total != 3 || len(rows) != 3 {
			t.Fatalf("rows=%d total=%d, want 3/3", len(rows), total)
		}
		// Newest (highest id) first.
		if rows[0].Status != "running" {
			t.Errorf("rows[0].Status = %q, want running (id desc)", rows[0].Status)
		}
		if rows[2].Status != "succeeded" {
			t.Errorf("rows[2].Status = %q, want succeeded", rows[2].Status)
		}
	})

	t.Run("failed only", func(t *testing.T) {
		rows, total, err := s.ListBuilds(testCtx, 1, 10, true)
		if err != nil {
			t.Fatalf("ListBuilds(failedOnly): %v", err)
		}
		if total != 1 || len(rows) != 1 || rows[0].Error != "makepkg: boom" {
			t.Errorf("rows=%v total=%d, want 1 failed build", rows, total)
		}
	})

	t.Run("pagination", func(t *testing.T) {
		page1, total, err := s.ListBuilds(testCtx, 1, 2, false)
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		if total != 3 || len(page1) != 2 {
			t.Errorf("page 1: rows=%d total=%d", len(page1), total)
		}
		page2, _, err := s.ListBuilds(testCtx, 2, 2, false)
		if err != nil {
			t.Fatalf("page 2: %v", err)
		}
		if len(page2) != 1 {
			t.Errorf("page 2: rows=%d, want 1", len(page2))
		}
	})
}

// TestGetBuildCorruptJSON guards the no-panic contract on both JSON
// columns.
func TestGetBuildCorruptJSON(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "cj")
	_, b := createTask(t, s, "cj-task", "queued", pkg, at(0))
	if _, err := s.write.Exec(
		`UPDATE builds SET artifacts = 'not json', resource_usage = '{broken' WHERE id = ?`, b.ID); err != nil {
		t.Fatalf("corrupt columns: %v", err)
	}
	if _, err := s.GetBuild(testCtx, b.ID); err == nil {
		t.Fatal("GetBuild on corrupt JSON: want error, got nil")
	}
}
