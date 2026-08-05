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
	"testing"
	"time"
)

// TestTimeConventions asserts the RFC3339 (UTC) storage round trip at
// nanosecond precision.
func TestTimeConventions(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "time")
	when := at(123456789 * time.Nanosecond)
	createTask(t, s, "time-1", "queued", pkg, when)

	// Store, read back, compare instants.
	task, err := s.GetTask(testCtx, "time-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !task.CreatedAt.Equal(when) {
		t.Errorf("CreatedAt = %v, want %v", task.CreatedAt, when)
	}
	if !task.LastProgressAt.Equal(when) {
		t.Errorf("LastProgressAt = %v, want %v", task.LastProgressAt, when)
	}
	// Raw storage is fixed-width UTC RFC3339 text.
	var raw string
	if err := s.read.QueryRow(
		`SELECT created_at FROM tasks WHERE id = 'time-1'`).Scan(&raw); err != nil {
		t.Fatalf("raw created_at: %v", err)
	}
	want := "2026-08-05T00:00:00.123456789Z"
	if raw != want {
		t.Errorf("raw created_at = %q, want %q", raw, want)
	}

	// formatTime normalizes to UTC.
	nonUTC := time.Date(2026, 8, 5, 8, 0, 0, 0, time.FixedZone("+08", 8*3600))
	if got := formatTime(nonUTC); got != "2026-08-05T00:00:00.000000000Z" {
		t.Errorf("formatTime(+08:00) = %q, want UTC midnight", got)
	}
}

// TestBoolConventions asserts booleans are stored as INTEGER 0/1.
func TestBoolConventions(t *testing.T) {
	s := newTestStore(t)
	seedPackage(t, s, Package{Pkgbase: "enabled-true", Branch: "main", Arch: "x86_64", Enabled: true})
	seedPackage(t, s, Package{Pkgbase: "enabled-false", Branch: "main", Arch: "x86_64", Enabled: false})

	var raw int
	if err := s.read.QueryRow(
		`SELECT enabled FROM packages WHERE pkgbase = 'enabled-true'`).Scan(&raw); err != nil {
		t.Fatalf("enabled-true: %v", err)
	}
	if raw != 1 {
		t.Errorf("enabled=true stored as %d, want 1", raw)
	}
	if err := s.read.QueryRow(
		`SELECT enabled FROM packages WHERE pkgbase = 'enabled-false'`).Scan(&raw); err != nil {
		t.Fatalf("enabled-false: %v", err)
	}
	if raw != 0 {
		t.Errorf("enabled=false stored as %d, want 0", raw)
	}

	truePkg, err := s.GetPackageByBase(testCtx, "enabled-true")
	if err != nil {
		t.Fatalf("get true: %v", err)
	}
	falsePkg, err := s.GetPackageByBase(testCtx, "enabled-false")
	if err != nil {
		t.Fatalf("get false: %v", err)
	}
	if !truePkg.Enabled || falsePkg.Enabled {
		t.Errorf("decoded Enabled = %v/%v, want true/false", truePkg.Enabled, falsePkg.Enabled)
	}
}

// TestJSONConventions asserts the maintainers/artifacts/resource_usage
// columns store JSON text and decode back to slices.
func TestJSONConventions(t *testing.T) {
	s := newTestStore(t)
	pkg := seedPackage(t, s, Package{
		Pkgbase:     "json",
		Branch:      "main",
		Arch:        "x86_64",
		Enabled:     true,
		Maintainers: []string{"a@example.com", "b@example.com"},
	})
	_, b := createTask(t, s, "json-1", "assigned", pkg, at(0))

	artifacts := []Artifact{
		{File: "x.pkg.tar.zst", Kind: "package", Pkgname: "x", Version: "1-1", Arch: "x86_64", Size: 1, SHA256: "s1"},
	}
	samples := []Sample{{At: at(time.Second), CPUTimeNS: 11, MemoryBytes: 22}}
	if err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeTask(testCtx, "json-1", "succeeded", "", at(2*time.Second), artifacts, samples)
	}); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	build, err := s.GetBuild(testCtx, b.ID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if len(build.Artifacts) != 1 || build.Artifacts[0].SHA256 != "s1" || build.Artifacts[0].Size != 1 {
		t.Errorf("artifacts = %v", build.Artifacts)
	}
	if len(build.ResourceUsage) != 1 || build.ResourceUsage[0].CPUTimeNS != 11 || build.ResourceUsage[0].MemoryBytes != 22 {
		t.Errorf("resource usage = %v", build.ResourceUsage)
	}

	// Raw JSON text with snake_case keys.
	var raw string
	if err := s.read.QueryRow(
		`SELECT artifacts FROM builds WHERE id = ?`, b.ID).Scan(&raw); err != nil {
		t.Fatalf("raw artifacts: %v", err)
	}
	if raw != `[{"file":"x.pkg.tar.zst","kind":"package","pkgname":"x","version":"1-1","arch":"x86_64","size":1,"sha256":"s1"}]` {
		t.Errorf("raw artifacts JSON = %s", raw)
	}
}

// TestCorruptJSONNoPanic guards the no-panic contract on the JSON decode
// paths exercised through the public API.
func TestCorruptJSONNoPanic(t *testing.T) {
	s := newTestStore(t)
	pkg := seedPackage(t, s, Package{
		Pkgbase: "corrupt-json",
		Branch:  "main",
		Arch:    "x86_64",
		Enabled: true,
	})
	_, b := createTask(t, s, "corrupt-json-1", "queued", pkg, at(0))

	if _, err := s.write.Exec(`UPDATE packages SET maintainers = '[' WHERE pkgbase = 'corrupt-json'`); err != nil {
		t.Fatalf("corrupt maintainers: %v", err)
	}
	if _, err := s.write.Exec(`UPDATE builds SET artifacts = '{', resource_usage = '[' WHERE id = ?`, b.ID); err != nil {
		t.Fatalf("corrupt build json: %v", err)
	}

	if _, err := s.GetPackageByBase(testCtx, "corrupt-json"); err == nil {
		t.Error("GetPackageByBase with corrupt maintainers: want error")
	}
	if _, err := s.GetBuild(testCtx, b.ID); err == nil {
		t.Error("GetBuild with corrupt JSON columns: want error")
	}
	if _, _, err := s.ListBuilds(testCtx, 1, 10, false); err == nil {
		t.Error("ListBuilds with corrupt JSON columns: want error")
	}
	if _, _, err := s.ListPackages(testCtx, "", 1, 10); err == nil {
		t.Error("ListPackages with corrupt maintainers: want error")
	}
	// A corrupt resource_usage must not block sample merging silently.
	if err := s.AppendResourceSamples(testCtx, b.ID, []Sample{{At: at(time.Second)}}); err == nil {
		t.Error("AppendResourceSamples with corrupt JSON: want error")
	}
}
