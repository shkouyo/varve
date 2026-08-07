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
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrateUpgradeFromV1 builds a database that only ever ran migration
// 001 (integer build ids, old columns), seeds legacy rows and then opens
// it through the store so migration 002 runs. It asserts the legacy→hash
// id mapping, the rewritten log paths and the new columns and defaults.
func TestMigrateUpgradeFromV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")

	legacy, err := sql.Open("sqlite", path+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	script, err := migrationsFS.ReadFile("migrations/001_init.sql")
	if err != nil {
		t.Fatalf("read migration 001: %v", err)
	}
	if _, err := legacy.Exec(string(script)); err != nil {
		t.Fatalf("apply migration 001: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
		INSERT INTO schema_migrations (version, applied_at) VALUES (1, 'x')`); err != nil {
		t.Fatalf("seed migration ledger: %v", err)
	}
	seed := `
	INSERT INTO packages (pkgbase, branch, vcs_kind, arch, enabled, current_version, pkgdesc, last_srcinfo_hash, last_upstream_ref, last_build_id, maintainers)
		VALUES ('legacy-pkg', 'main', 'git', 'x86_64', 1, '1.0-1', 'd', 'h', 'r', 7, '[]');
	INSERT INTO builds (package_id, branch, "commit", upstream_ref, srcinfo_hash, status, worker_id, log_path, started_at, finished_at, error, artifacts, resource_usage)
		VALUES (1, 'main', 'c1', 'r', 'h', 'succeeded', NULL, 'logs/1.log', NULL, '2026-08-05T00:00:00.000000000Z', '', '[]', '[]'),
		       (1, 'main', 'c2', 'r', 'h', 'queued', NULL, 'logs/2.log', NULL, NULL, '', '[]', '[]');
	INSERT INTO tasks (id, package_id, build_id, state, worker_id, assigned_at, created_at, last_progress_at, attempts, claim_token, cancel_requested)
		VALUES ('legacy-task', 1, 2, 'queued', NULL, NULL, '2026-08-05T00:00:00.000000000Z', '2026-08-05T00:00:00.000000000Z', 0, '', 0);
	INSERT INTO workers (name, role, mode, arch, capacity, status, last_heartbeat, version)
		VALUES ('legacy-node', 'agent', 'pool', 'x86_64', 1, 'online', NULL, '');
	INSERT INTO builds (package_id, branch, "commit", upstream_ref, srcinfo_hash, status, worker_id, log_path, started_at, finished_at, error, artifacts, resource_usage)
		VALUES (1, 'main', 'c3', 'r', 'h', 'running', 1, 'logs/3.log', NULL, NULL, '', '[]', '[]');
	`
	if _, err := legacy.Exec(seed); err != nil {
		t.Fatalf("seed legacy rows: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open after migration: %v", err)
	}
	defer s.Close()

	// Ledger: both migrations recorded, exactly once.
	var versions []int
	rows, err := s.read.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query ledger: %v", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	rows.Close()
	if len(versions) != 2 || versions[0] != 1 || versions[1] != 2 {
		t.Fatalf("schema_migrations = %v, want [1 2]", versions)
	}

	// Build 1: deterministic hash id, rewritten log path, no worker name.
	b, err := s.GetBuild(testCtx, "0000000000000001")
	if err != nil {
		t.Fatalf("GetBuild(migrated id): %v", err)
	}
	if b.LogPath != "logs/0000000000000001.log" {
		t.Errorf("migrated log_path = %q, want logs/0000000000000001.log", b.LogPath)
	}
	if b.WorkerName != "" {
		t.Errorf("migrated worker_name = %q, want empty default", b.WorkerName)
	}
	if b.Status != "succeeded" {
		t.Errorf("migrated status = %q, want succeeded", b.Status)
	}

	// Newest first via seq, which preserved the old integer order.
	all, total, err := s.ListBuilds(testCtx, 1, 10, false)
	if err != nil {
		t.Fatalf("ListBuilds: %v", err)
	}
	if total != 3 || all[0].ID != "0000000000000003" || all[2].ID != "0000000000000001" {
		t.Errorf("list order ids = %q/%q/%q total = %d, want 0000000000000003/.../0000000000000001, 3", all[0].ID, all[1].ID, all[2].ID, total)
	}

	// Package row: last_build_id converted, cooldown column present.
	pkg, err := s.GetPackageByBase(testCtx, "legacy-pkg")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if pkg.LastBuildID != "0000000000000007" {
		t.Errorf("last_build_id = %q, want 0000000000000007", pkg.LastBuildID)
	}
	if pkg.LastFailedAt != nil {
		t.Errorf("last_failed_at = %v, want nil default", pkg.LastFailedAt)
	}

	// Task row: build_id converted, fail_count default zero.
	task, err := s.GetTask(testCtx, "legacy-task")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.BuildID != "0000000000000002" {
		t.Errorf("task build_id = %q, want 0000000000000002", task.BuildID)
	}
	if task.FailCount != 0 {
		t.Errorf("task fail_count = %d, want 0", task.FailCount)
	}

	// worker_id on builds lost its foreign key: deleting the worker row
	// that the running build references must succeed.
	if err := s.DeleteWorker(testCtx, "legacy-node"); err != nil {
		t.Errorf("DeleteWorker with build reference = %v, want nil (no FK)", err)
	}
}

// TestBuildWorkerName asserts the plain-text machine attribution lifecycle:
// ClaimTask writes the worker name onto the build row, RequeueTask clears
// it, and the stored value round-trips through GetBuild.
func TestBuildWorkerName(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "wn")
	w := registerWorker(t, s, "node-42", 1)
	createTask(t, s, "wn-task", "queued", pkg, at(0))

	claimed, err := s.ClaimTask(testCtx, w.ID, 1, "tok")
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	b, err := s.GetBuild(testCtx, claimed.BuildID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if b.WorkerName != "node-42" {
		t.Errorf("WorkerName after claim = %q, want node-42", b.WorkerName)
	}
	if b.WorkerID != w.ID {
		t.Errorf("WorkerID after claim = %d, want %d", b.WorkerID, w.ID)
	}

	if err := s.RequeueTask(testCtx, "wn-task"); err != nil {
		t.Fatalf("RequeueTask: %v", err)
	}
	b, err = s.GetBuild(testCtx, claimed.BuildID)
	if err != nil {
		t.Fatalf("GetBuild after requeue: %v", err)
	}
	if b.WorkerName != "" || b.WorkerID != 0 {
		t.Errorf("after requeue WorkerName = %q WorkerID = %d, want both unset", b.WorkerName, b.WorkerID)
	}
}

// TestNewColumnDefaults asserts the retry bookkeeping defaults on fresh
// rows: fail_count zero, worker_name empty, last_failed_at unset.
func TestNewColumnDefaults(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "defaults")
	task, _ := createTask(t, s, "defaults-task", "queued", pkg, at(0))
	if task.FailCount != 0 {
		t.Errorf("FailCount = %d, want 0", task.FailCount)
	}
	got, err := s.GetTask(testCtx, "defaults-task")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.FailCount != 0 {
		t.Errorf("stored fail_count = %d, want 0", got.FailCount)
	}
	p, err := s.GetPackageByBase(testCtx, "defaults")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if p.LastFailedAt != nil || p.LastBuildID != "" {
		t.Errorf("fresh package last_failed_at = %v last_build_id = %q, want unset", p.LastFailedAt, p.LastBuildID)
	}
}
