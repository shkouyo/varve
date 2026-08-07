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
	"net/http/httptest"
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/dispatch"
)

// TestAdminRedirectsToDashboard asserts GET /admin demands auth and then
// redirects to the merged dashboard page.
func TestAdminRedirectsToDashboard(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{}},
		newTestDB(t), newFakeLogReader(""))

	rec := get(t, s, http.MethodGet, "/admin", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /admin (anonymous) = %d, want 401", rec.Code)
	}
	rec = getAuth(t, s, http.MethodGet, "/admin", "admin", "s3cret")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("GET /admin (authed) = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
}

// TestDashboardAdminData asserts the merged dashboard carries the admin
// data (task queue, flash) only for authenticated requests.
func TestDashboardAdminData(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	seedActiveTask(t, store, pkg, "queued", "t-abc")
	seedWorker(t, store, "node-1")

	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{}},
		store, newFakeLogReader(""))

	// Anonymous: public view only.
	anon := newRequest(t, http.MethodGet, "/")
	data, err := s.dashboardData(anon)
	if err != nil {
		t.Fatalf("dashboardData(anonymous): %v", err)
	}
	if data.Admin {
		t.Error("anonymous dashboard must not be marked admin")
	}
	if len(data.Tasks) != 0 {
		t.Errorf("anonymous dashboard Tasks = %v, want none", data.Tasks)
	}

	// Authenticated: admin flag, task queue and flash resolved.
	auth := newRequest(t, http.MethodGet, "/?ok=Rebuild+queued")
	auth.SetBasicAuth("admin", "s3cret")
	data, err = s.dashboardData(auth)
	if err != nil {
		t.Fatalf("dashboardData(authed): %v", err)
	}
	if !data.Admin {
		t.Error("authed dashboard must be marked admin")
	}
	if len(data.Tasks) != 1 || data.Tasks[0].ID != "t-abc" ||
		data.Tasks[0].Pkgbase != "demo-pkg" || data.Tasks[0].CancelURL != "/admin/tasks/t-abc/cancel" {
		t.Errorf("Tasks = %+v, want one queued task t-abc for demo-pkg", data.Tasks)
	}
	if data.Flash == nil || data.Flash.Kind != "ok" || data.Flash.Message != "Rebuild queued" {
		t.Errorf("Flash = %+v, want ok flash Rebuild queued", data.Flash)
	}
}

// TestAdminActions asserts each POST action reaches the orchestrator and
// redirects with a flash.
func TestAdminActions(t *testing.T) {
	orch := &fakeOrchestrator{stats: &dispatch.Stats{}}
	s := newTestServer(t, testConfig(), orch, newTestDB(t), newFakeLogReader(""))

	actions := []struct {
		name   string
		method string
		path   string
		form   map[string]string
		assert func(t *testing.T)
	}{
		{"rebuild", http.MethodPost, "/admin/packages/demo-pkg/rebuild", nil, func(t *testing.T) {
			if len(orch.rebuilds) != 1 || orch.rebuilds[0] != "demo-pkg" {
				t.Fatalf("rebuilds = %v, want [demo-pkg]", orch.rebuilds)
			}
		}},
		{"cancel", http.MethodPost, "/admin/tasks/t-1/cancel", nil, func(t *testing.T) {
			if len(orch.cancels) != 1 || orch.cancels[0] != "t-1" {
				t.Fatalf("cancels = %v, want [t-1]", orch.cancels)
			}
		}},
		{"disable", http.MethodPost, "/admin/workers/node-1/disable", nil, func(t *testing.T) {
			if len(orch.disables) != 1 || orch.disables[0] != "node-1" {
				t.Fatalf("disables = %v, want [node-1]", orch.disables)
			}
		}},
		{"enable", http.MethodPost, "/admin/workers/node-1/enable", nil, func(t *testing.T) {
			if len(orch.enables) != 1 || orch.enables[0] != "node-1" {
				t.Fatalf("enables = %v, want [node-1]", orch.enables)
			}
		}},
		{"remove", http.MethodPost, "/admin/workers/node-1/remove", map[string]string{"confirm": "1"}, func(t *testing.T) {
			if len(orch.removes) != 1 || orch.removes[0] != "node-1" {
				t.Fatalf("removes = %v, want [node-1]", orch.removes)
			}
		}},
	}
	for _, tc := range actions {
		t.Run(tc.name, func(t *testing.T) {
			var rec *httptest.ResponseRecorder
			if tc.form != nil {
				rec = postForm(t, s, tc.path, "admin", "s3cret", tc.form)
			} else {
				rec = getAuth(t, s, http.MethodPost, tc.path, "admin", "s3cret")
			}
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

// TestAdminRemoveRequiresConfirmation asserts the removal form must carry
// the confirm checkbox; a missing confirmation redirects with an error
// and never reaches the orchestrator.
func TestAdminRemoveRequiresConfirmation(t *testing.T) {
	orch := &fakeOrchestrator{stats: &dispatch.Stats{}}
	s := newTestServer(t, testConfig(), orch, newTestDB(t), newFakeLogReader(""))

	rec := postForm(t, s, "/admin/workers/node-1/remove", "admin", "s3cret", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST remove without confirm = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/admin?error=") || !strings.Contains(loc, "confirmation") {
		t.Errorf("Location = %q, want /admin?error=...confirmation...", loc)
	}
	if len(orch.removes) != 0 {
		t.Errorf("removes = %v, want none without confirmation", orch.removes)
	}
}

// TestAdminActionErrorFlash asserts a failing action redirects with the
// error carried in the query string and the merged dashboard surfaces it
// as a flash for authenticated requests.
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

	// The flash survives the /admin redirect into the dashboard query.
	rec = getAuth(t, s, http.MethodGet, loc, "admin", "s3cret")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") == "" {
		t.Fatalf("GET %s = %d, want a redirect to the dashboard", loc, rec.Code)
	}
	dash := strings.Replace(loc, "/admin", "/", 1)
	req := newRequest(t, http.MethodGet, dash)
	req.SetBasicAuth("admin", "s3cret")
	data, err := s.dashboardData(req)
	if err != nil {
		t.Fatalf("dashboardData: %v", err)
	}
	if data.Flash == nil || data.Flash.Kind != "error" || !strings.Contains(data.Flash.Message, "conflict") {
		t.Errorf("Flash = %+v, want an error flash mentioning conflict", data.Flash)
	}
}

// TestAdminFailedBuilds asserts GET /admin/builds?failed=1 lists failed
// builds only, with package and error summary.
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

	// The failed filter must be "1" when present.
	rec = getAuth(t, s, http.MethodGet, "/admin/builds?failed=2", "admin", "s3cret")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("GET /admin/builds?failed=2 = %d, want 400", rec.Code)
	}
}

// TestAdminFlashSurvivesRedirect asserts the ok flash carried by an admin
// action lands on the merged dashboard data.
func TestAdminFlashSurvivesRedirect(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{}},
		newTestDB(t), newFakeLogReader(""))
	req := newRequest(t, http.MethodGet, "/?ok=Rebuild+queued")
	req.SetBasicAuth("admin", "s3cret")
	data, err := s.dashboardData(req)
	if err != nil {
		t.Fatalf("dashboardData: %v", err)
	}
	if data.Flash == nil || data.Flash.Message != "Rebuild queued" {
		t.Errorf("Flash = %+v, want ok flash Rebuild queued", data.Flash)
	}
}
