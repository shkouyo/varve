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
	"context"
	"errors"
	"net/http"
	"testing"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/dispatch"
)

// TestDashboardRender asserts the dashboard shows status counts, the
// queue length, recent builds (package name + status) and the worker
// overview.
func TestDashboardRender(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil)
	seedWorker(t, store, "node-1")

	orch := &fakeOrchestrator{stats: &dispatch.Stats{
		QueueLen: 3,
		ByStatus: map[string]int{"succeeded": 1, "running": 2},
		RecentBuilds: []db.Build{
			{ID: build.ID, PackageID: pkg.ID, Status: "succeeded", Branch: "main"},
		},
		Workers: nil,
	}}
	s := newTestServer(t, testConfig(), orch, store, newFakeLogReader(""))

	rec := get(t, s, http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body,
		"succeeded", "1", // status counts
		"running", "2", // status counts
		"queued", "assigned", "failed", "cancelled", // zero counts stay visible
		">0<",                     // the empty statuses render a literal 0
		"3",                       // queue length
		"demo-pkg",                // recent build package name
		"node-1",                  // worker overview name
		"online",                  // worker status
		"/builds/"+itoa(build.ID), // recent build link
	)
}

// TestStatusCountsZeroFill asserts every known status card renders,
// zero counts included, with unknown statuses appended after the six.
func TestStatusCountsZeroFill(t *testing.T) {
	got := statusCounts(map[string]int{"running": 2, "odd": 1})
	want := []string{"queued", "assigned", "running", "succeeded", "failed", "cancelled", "odd"}
	if len(got) != len(want) {
		t.Fatalf("statusCounts = %d entries, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Status != w {
			t.Errorf("entry %d status = %q, want %q", i, got[i].Status, w)
		}
	}
	if got[0].Count != 0 || got[2].Count != 2 || got[6].Count != 1 {
		t.Errorf("counts = %+v, want queued 0, running 2, odd 1", got)
	}
}

// TestDashboardStatsError maps an orchestrator failure to a 500 page.
func TestDashboardStatsError(t *testing.T) {
	orch := &fakeOrchestrator{statsErr: errors.New("boom")}
	s := newTestServer(t, testConfig(), orch, newTestDB(t), newFakeLogReader(""))
	rec := get(t, s, http.MethodGet, "/", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET / = %d, want 500", rec.Code)
	}
}

// TestRecentBuildViews resolves the executing machine name from the
// workers list (builds.worker_id joins workers.name).
func TestRecentBuildViews(t *testing.T) {
	store := newTestDB(t)
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))
	workers := []db.Worker{{ID: 7, Name: "proud-heron-7"}}
	builds := []db.Build{{ID: "0000000000000001", PackageID: 0, WorkerID: 7, Status: "failed"}}

	views := s.recentBuildViews(context.Background(), builds, workers)
	if len(views) != 1 {
		t.Fatalf("got %d views, want 1", len(views))
	}
	if views[0].WorkerName != "proud-heron-7" {
		t.Errorf("WorkerName = %q, want %q", views[0].WorkerName, "proud-heron-7")
	}
	if views[0].Status != "failed" {
		t.Errorf("Status = %q, want failed", views[0].Status)
	}
}
