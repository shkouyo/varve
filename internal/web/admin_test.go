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
	"errors"
	"net/http"
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/dispatch"
)

// TestAdminRender asserts the admin page shows the dashboard data plus
// the task queue with cancel forms and worker manage buttons (A23).
func TestAdminRender(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil)
	seedWorker(t, store, "node-1")

	// One active queued task for the same package (the terminal build
	// above does not block a new active task).
	seedActiveTask(t, store, pkg, "queued", "t-abc")

	orch := &fakeOrchestrator{stats: &dispatch.Stats{
		QueueLen: 1,
		ByStatus: map[string]int{"succeeded": 1},
		RecentBuilds: []db.Build{
			{ID: build.ID, PackageID: pkg.ID, Status: "succeeded"},
		},
	}}
	s := newTestServer(t, testConfig(), orch, store, newFakeLogReader(""))

	rec := getAuth(t, s, http.MethodGet, "/admin", "admin", "s3cret")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body,
		"Task queue",
		"t-abc",                            // task id
		"/admin/tasks/t-abc/cancel",        // cancel action
		"/admin/packages/demo-pkg/rebuild", // rebuild action
		"/admin/workers/node-1/disable",    // disable action
		"/admin/workers/node-1/remove",     // remove action
		"/admin/builds?failed=1",           // failed list link
		"demo-pkg",
	)
}

// TestAdminActions asserts each POST action reaches the orchestrator and
// redirects with a flash (DETAIL §10.7 point 5).
func TestAdminActions(t *testing.T) {
	orch := &fakeOrchestrator{stats: &dispatch.Stats{}}
	s := newTestServer(t, testConfig(), orch, newTestDB(t), newFakeLogReader(""))

	actions := []struct {
		name   string
		method string
		path   string
		assert func(t *testing.T)
	}{
		{"rebuild", http.MethodPost, "/admin/packages/demo-pkg/rebuild", func(t *testing.T) {
			if len(orch.rebuilds) != 1 || orch.rebuilds[0] != "demo-pkg" {
				t.Fatalf("rebuilds = %v, want [demo-pkg]", orch.rebuilds)
			}
		}},
		{"cancel", http.MethodPost, "/admin/tasks/t-1/cancel", func(t *testing.T) {
			if len(orch.cancels) != 1 || orch.cancels[0] != "t-1" {
				t.Fatalf("cancels = %v, want [t-1]", orch.cancels)
			}
		}},
		{"disable", http.MethodPost, "/admin/workers/node-1/disable", func(t *testing.T) {
			if len(orch.disables) != 1 || orch.disables[0] != "node-1" {
				t.Fatalf("disables = %v, want [node-1]", orch.disables)
			}
		}},
		{"remove", http.MethodPost, "/admin/workers/node-1/remove", func(t *testing.T) {
			if len(orch.removes) != 1 || orch.removes[0] != "node-1" {
				t.Fatalf("removes = %v, want [node-1]", orch.removes)
			}
		}},
	}
	for _, tc := range actions {
		t.Run(tc.name, func(t *testing.T) {
			rec := getAuth(t, s, http.MethodPost, tc.path, "admin", "s3cret")
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("POST %s = %d, want 303", tc.path, rec.Code)
			}
			if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/admin?ok=") {
				t.Errorf("Location = %q, want /admin?ok=...", loc)
			}
			tc.assert(t)
		})
	}
}

// TestAdminActionErrorFlash asserts a failing action redirects with the
// error carried in the query string and the admin page displays it
// (DETAIL §10.4 point 3).
func TestAdminActionErrorFlash(t *testing.T) {
	store := newTestDB(t)
	seedPackage(t, store, "demo-pkg", "A demo package")
	orch := &fakeOrchestrator{stats: &dispatch.Stats{}, rebuildErr: errors.New("conflict")}
	s := newTestServer(t, testConfig(), orch, store, newFakeLogReader(""))

	rec := getAuth(t, s, http.MethodPost, "/admin/packages/demo-pkg/rebuild", "admin", "s3cret")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST rebuild = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/admin?error=") || !strings.Contains(loc, "conflict") {
		t.Errorf("Location = %q, want /admin?error=...conflict...", loc)
	}

	rec = getAuth(t, s, http.MethodGet, loc, "admin", "s3cret")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", loc, rec.Code)
	}
	mustContain(t, rec.Body.String(), "Rebuild failed: conflict")
}

// TestAdminFailedBuilds asserts GET /admin/builds?failed=1 lists failed
// builds only, with package and error summary (DETAIL §10.7 point 5).
func TestAdminFailedBuilds(t *testing.T) {
	store := newTestDB(t)
	good := seedPackage(t, store, "good-pkg", "works")
	bad := seedPackage(t, store, "bad-pkg", "broken")
	seedBuild(t, store, good, "succeeded", nil, nil)
	seedBuild(t, store, bad, "failed", nil, nil)

	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{}},
		store, newFakeLogReader(""))
	rec := getAuth(t, s, http.MethodGet, "/admin/builds?failed=1", "admin", "s3cret")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/builds = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body, "Failed builds", "bad-pkg")
	if strings.Contains(body, "good-pkg") {
		t.Error("failed list must not contain succeeded builds")
	}
}

// TestAdminFlashRenders asserts an ?ok= flash renders as a green banner.
func TestAdminFlashRenders(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{}},
		newTestDB(t), newFakeLogReader(""))
	rec := getAuth(t, s, http.MethodGet, "/admin?ok=Rebuild+queued", "admin", "s3cret")
	mustContain(t, rec.Body.String(), "Rebuild queued", "role=\"alert\"")
}
