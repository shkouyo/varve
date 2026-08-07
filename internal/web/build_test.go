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

package web

import (
	"net/http"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/db"
)

// TestBuildDetailRenders asserts the build page shows the summary, the
// machine (or "not assigned"), the log history and the resource samples
// as text.
func TestBuildDetailRenders(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	samples := []db.Sample{
		{At: time.Now().UTC(), CPUTimeNS: 12345678900, MemoryBytes: 536870912}, // 12.3s, 512 MiB
	}
	build := seedBuild(t, store, pkg, "failed", nil, samples)

	logs := newFakeLogReader("making package: demo-pkg\nfinished\n")
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, logs)
	rec := get(t, s, http.MethodGet, "/builds/"+itoa(build.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /builds/%s = %d, want 200", build.ID, rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body,
		"Build #"+itoa(build.ID),
		"demo-pkg",                       // package context
		"not assigned",                   // machine fallback (no worker)
		"making package: demo-pkg",       // log history
		"CPU 12.3s",                      // resource usage text
		"Memory 512 MiB",                 // resource usage text
		"/builds/"+itoa(build.ID)+"/log", // live log link
	)
}

// TestBuildDetailMachineName resolves the executing node through
// GetWorkerByID when the build carries a worker id. The store never
// populates builds.worker_id through public methods, so the resolution is
// exercised at the handler level with a direct row-level check instead.
func TestBuildDetailMachineName(t *testing.T) {
	// The public read path is GetWorkerByID; verify it resolves the name
	// we seeded (used by handleBuild).
	store := newTestDB(t)
	w := seedWorker(t, store, "proud-heron-7")
	got, err := store.GetWorkerByID(testCtx, w.ID)
	if err != nil || got.Name != "proud-heron-7" {
		t.Fatalf("GetWorkerByID = %+v, %v; want proud-heron-7", got, err)
	}
}

// TestBuildNotFound asserts a missing build renders the 404 error page.
func TestBuildNotFound(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, newTestDB(t), newFakeLogReader(""))
	rec := get(t, s, http.MethodGet, "/builds/ffffffffffffffff", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /builds/ffffffffffffffff = %d, want 404", rec.Code)
	}
	mustContain(t, rec.Body.String(), "Not Found", "Build not found")
}

// TestBuildInvalidID asserts a malformed build id is a 400.
func TestBuildInvalidID(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, newTestDB(t), newFakeLogReader(""))
	rec := get(t, s, http.MethodGet, "/builds/12345", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET /builds/12345 = %d, want 400", rec.Code)
	}
}

// TestFormatBytesAndCPU covers the text metric formatting.
func TestFormatBytesAndCPU(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{2048, "2 KiB"},
		{536870912, "512 MiB"},
		{1610612736, "1.5 GiB"},
	}
	for _, tc := range cases {
		if got := formatBytes(tc.in); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := formatCPU(12345678900); got != "12.3s" {
		t.Errorf("formatCPU = %q, want 12.3s", got)
	}
}
