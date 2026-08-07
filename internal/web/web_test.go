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
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/dispatch"
)

// TestHandlerRouteTable exercises every route: public pages render, admin
// routes demand Basic Auth (401 + challenge), malformed input is a 400,
// missing resources are 404 and the merged admin entry redirects.
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
		{"builds list", http.MethodGet, "/builds", http.StatusOK},
		{"packages list", http.MethodGet, "/packages", http.StatusOK},
		{"package detail", http.MethodGet, "/packages/demo-pkg", http.StatusOK},
		{"package missing", http.MethodGet, "/packages/nope", http.StatusNotFound},
		{"package invalid", http.MethodGet, "/packages/bad%20name", http.StatusBadRequest},
		{"build detail", http.MethodGet, "/builds/" + itoa(build.ID), http.StatusOK},
		{"build missing", http.MethodGet, "/builds/ffffffffffffffff", http.StatusNotFound},
		{"build invalid", http.MethodGet, "/builds/99999", http.StatusBadRequest},
		{"build log", http.MethodGet, "/builds/" + itoa(build.ID) + "/log", http.StatusFound},
		{"copying", http.MethodGet, "/copying.txt", http.StatusMovedPermanently},
		{"copying canonical", http.MethodGet, "/COPYING.txt", http.StatusOK},
		{"favicon svg", http.MethodGet, "/favicon.svg", http.StatusOK},
		{"favicon ico", http.MethodGet, "/favicon.ico", http.StatusFound},
		{"trailing slash", http.MethodGet, "/packages/", http.StatusMovedPermanently},
		{"logout", http.MethodGet, "/admin/logout", http.StatusUnauthorized},
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

	// The admin entry redirects to the dashboard once authenticated.
	rec := getAuth(t, s, http.MethodGet, "/admin", "admin", "s3cret")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("GET /admin with auth = %d %q, want 303 /", rec.Code, rec.Header().Get("Location"))
	}
	rec = getAuth(t, s, http.MethodGet, "/admin/builds?failed=1", "admin", "s3cret")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/builds with auth = %d, want 200", rec.Code)
	}
}

// TestCopyingServesLicense asserts /COPYING.txt returns the verbatim
// repository license text (no added comments) as plain text, and that
// the legacy lowercase path redirects to it permanently.
func TestCopyingServesLicense(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, newTestDB(t), newFakeLogReader(""))
	rec := get(t, s, http.MethodGet, "/COPYING.txt", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /COPYING.txt = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	body := rec.Body.String()
	mustContain(t, body, "GNU AFFERO GENERAL PUBLIC LICENSE", "Version 3, 19 November 2007")
	// The asset must be the raw license text: no mirror note header, no
	// trailing annotation.
	if strings.Contains(body, "mirror") || strings.HasPrefix(body, "#") {
		t.Error("COPYING.txt must be the verbatim repository license text")
	}

	rec = get(t, s, http.MethodGet, "/copying.txt", nil)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /copying.txt = %d, want 301", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/COPYING.txt" {
		t.Errorf("Location = %q, want /COPYING.txt", loc)
	}
}

// TestFaviconRoutes asserts the SVG icon is served inline and the .ico
// path redirects to it.
func TestFaviconRoutes(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, newTestDB(t), newFakeLogReader(""))
	rec := get(t, s, http.MethodGet, "/favicon.svg", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /favicon.svg = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("Content-Type = %q, want image/svg+xml", ct)
	}
	mustContain(t, rec.Body.String(), "<svg", "</svg>")

	rec = get(t, s, http.MethodGet, "/favicon.ico", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /favicon.ico = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/favicon.svg" {
		t.Errorf("Location = %q, want /favicon.svg", loc)
	}
}

// TestTrailingSlashRedirect asserts non-root paths ending in "/" are
// normalized 301 to the slashless form, the root path stays put, and the
// query string survives.
func TestTrailingSlashRedirect(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{}}, newTestDB(t), newFakeLogReader(""))
	cases := []struct {
		path string
		want string
	}{
		{"/packages/", "/packages"},
		{"/builds/", "/builds"},
		{"/packages/?q=foo&page=2", "/packages?q=foo&page=2"},
	}
	for _, tc := range cases {
		rec := get(t, s, http.MethodGet, tc.path, nil)
		if rec.Code != http.StatusMovedPermanently {
			t.Errorf("GET %s = %d, want 301", tc.path, rec.Code)
			continue
		}
		if loc := rec.Header().Get("Location"); loc != tc.want {
			t.Errorf("GET %s: Location = %q, want %q", tc.path, loc, tc.want)
		}
	}
	rec := get(t, s, http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("GET / = %d, want 200 (root never redirects)", rec.Code)
	}
}

// TestSecurityHeaders asserts every response carries the minimal security
// header set: frame denial and content-type sniffing prevention.
func TestSecurityHeaders(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{}}, newTestDB(t), newFakeLogReader(""))
	for _, path := range []string{"/", "/packages", "/packages/nope", "/COPYING.txt", "/favicon.svg"} {
		rec := get(t, s, http.MethodGet, path, nil)
		if rec.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: missing X-Frame-Options: DENY", path)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing X-Content-Type-Options: nosniff", path)
		}
	}
}

// TestTemplateSetCompiles asserts that the full template set is
// registered and each servable page renders with a 200. admin.html was
// removed when the admin area merged into the dashboard.
func TestTemplateSetCompiles(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil)
	orch := &fakeOrchestrator{stats: &dispatch.Stats{}}
	s := newTestServer(t, testConfig(), orch, store, newFakeLogReader(""))

	want := []string{
		"dashboard.html", "packages.html", "package.html", "build.html",
		"admin_builds.html", "builds.html", "error.html",
	}
	for _, name := range want {
		if s.tmpl.Lookup(name) == nil {
			t.Errorf("template %q is not registered", name)
		}
	}

	// The legacy log page is served by a redirect; the merged log with
	// its resumable SSE client renders inside build.html (log.html was
	// removed when its EventSource moved there).
	paths := map[string]string{
		"dashboard.html":    "/",
		"packages.html":     "/packages",
		"package.html":      "/packages/demo-pkg",
		"build.html":        "/builds/" + itoa(build.ID),
		"builds.html":       "/builds",
		"admin_builds.html": "/admin/builds?failed=1",
	}
	for name, path := range paths {
		var rec *httptest.ResponseRecorder
		if name == "admin_builds.html" {
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
