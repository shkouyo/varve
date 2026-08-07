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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"git.0x0f.dev/varve/internal/dispatch"
)

// TestHandlerRouteTable exercises every route: public pages render, admin
// routes demand Basic Auth (401 + challenge) and the missing-resource
// pages are 404.
func TestHandlerRouteTable(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil)
	seedWorker(t, store, "node-1")

	orch := &fakeOrchestrator{stats: &dispatch.Stats{ByStatus: map[string]int{"succeeded": 1}}}
	s := newTestServer(t, testConfig(), orch, store, newFakeLogReader(""))

	cases := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"dashboard", http.MethodGet, "/", http.StatusOK},
		{"packages list", http.MethodGet, "/packages", http.StatusOK},
		{"package detail", http.MethodGet, "/packages/demo-pkg", http.StatusOK},
		{"package missing", http.MethodGet, "/packages/nope", http.StatusNotFound},
		{"build detail", http.MethodGet, "/builds/" + itoa(build.ID), http.StatusOK},
		{"build missing", http.MethodGet, "/builds/99999", http.StatusNotFound},
		{"build log", http.MethodGet, "/builds/" + itoa(build.ID) + "/log", http.StatusOK},
		{"admin unauth", http.MethodGet, "/admin", http.StatusUnauthorized},
		{"admin builds unauth", http.MethodGet, "/admin/builds?failed=1", http.StatusUnauthorized},
		{"admin rebuild unauth", http.MethodPost, "/admin/packages/demo-pkg/rebuild", http.StatusUnauthorized},
		{"admin cancel unauth", http.MethodPost, "/admin/tasks/t-1/cancel", http.StatusUnauthorized},
		{"admin disable unauth", http.MethodPost, "/admin/workers/node-1/disable", http.StatusUnauthorized},
		{"admin remove unauth", http.MethodPost, "/admin/workers/node-1/remove", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, s, tc.method, tc.path, nil)
			if rec.Code != tc.want {
				t.Fatalf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
			}
			if tc.want == http.StatusUnauthorized {
				if got := rec.Header().Get("WWW-Authenticate"); got == "" {
					t.Error("missing WWW-Authenticate challenge header")
				}
			}
		})
	}

	// Admin routes respond once authenticated.
	rec := getAuth(t, s, http.MethodGet, "/admin", "admin", "s3cret")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin with auth = %d, want 200", rec.Code)
	}
	rec = getAuth(t, s, http.MethodGet, "/admin/builds?failed=1", "admin", "s3cret")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/builds with auth = %d, want 200", rec.Code)
	}
}

// TestTemplateSetCompiles asserts that the full eight-template set is
// registered and each renders with a 200.
func TestTemplateSetCompiles(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil)
	orch := &fakeOrchestrator{stats: &dispatch.Stats{}}
	s := newTestServer(t, testConfig(), orch, store, newFakeLogReader(""))

	want := []string{
		"dashboard.html", "packages.html", "package.html", "build.html",
		"log.html", "admin.html", "admin_builds.html", "error.html",
	}
	for _, name := range want {
		if s.tmpl.Lookup(name) == nil {
			t.Errorf("template %q is not registered", name)
		}
	}

	paths := map[string]string{
		"dashboard.html":    "/",
		"packages.html":     "/packages",
		"package.html":      "/packages/demo-pkg",
		"build.html":        "/builds/" + itoa(build.ID),
		"log.html":          "/builds/" + itoa(build.ID) + "/log",
		"admin.html":        "/admin",
		"admin_builds.html": "/admin/builds?failed=1",
	}
	for name, path := range paths {
		var rec *httptest.ResponseRecorder
		if name == "admin.html" || name == "admin_builds.html" {
			rec = getAuth(t, s, http.MethodGet, path, "admin", "s3cret")
		} else {
			rec = get(t, s, http.MethodGet, path, nil)
		}
		if rec.Code != http.StatusOK {
			t.Errorf("%s at %s = %d, want 200", name, path, rec.Code)
		}
	}
}

// itoa renders an id without strconv noise in tests: integers are
// formatted as decimals, strings are passed through unchanged (build ids
// became hash strings).
func itoa(n any) string {
	switch v := n.(type) {
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	case string:
		return v
	}
	return fmt.Sprint(n)
}
