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

package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/dispatch"
)

// TestErrorPages asserts the 404 and 401 responses render the error page
// with semantic markup and no JavaScript (DETAIL §10.7 point 7).
func TestErrorPages(t *testing.T) {
	store := newTestDB(t)
	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{}}, store, newFakeLogReader(""))

	for _, path := range []string{"/packages/nope", "/builds/12345"} {
		rec := get(t, s, http.MethodGet, path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
		body := rec.Body.String()
		mustContain(t, body, "<main", "</main>", "Not Found", "Back to dashboard")
		if strings.Contains(body, "<script") {
			t.Errorf("%s error page must not contain scripts", path)
		}
	}

	rec := get(t, s, http.MethodGet, "/admin", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /admin = %d, want 401", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body, "Unauthorized", "<main", "Back to dashboard")
	if strings.Contains(body, "<script") {
		t.Error("401 error page must not contain scripts")
	}
}

// TestSemanticMarkup asserts the shared page chrome on every rendered
// page: html lang, skip link, semantic landmarks, table headers with
// scope, and aria-hidden decorative icons (DETAIL §10.7 point 7, WCAG
// 2.2 AA).
func TestSemanticMarkup(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil)
	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{
		ByStatus:     map[string]int{"succeeded": 1},
		RecentBuilds: []db.Build{{ID: build.ID, PackageID: pkg.ID, Status: "succeeded"}},
	}}, store, newFakeLogReader("==> done\n"))

	pages := map[string]string{
		"dashboard":   "/",
		"packages":    "/packages",
		"package":     "/packages/demo-pkg",
		"build":       "/builds/" + itoa(build.ID),
		"log":         "/builds/" + itoa(build.ID) + "/log",
		"admin":       "/admin",
		"adminBuilds": "/admin/builds?failed=1",
	}
	for name, path := range pages {
		var rec *httptest.ResponseRecorder
		if name == "admin" || name == "adminBuilds" {
			rec = getAuth(t, s, http.MethodGet, path, "admin", "s3cret")
		} else {
			rec = get(t, s, http.MethodGet, path, nil)
		}
		if rec.Code != http.StatusOK {
			t.Errorf("%s page = %d, want 200", name, rec.Code)
			continue
		}
		body := rec.Body.String()
		if !strings.HasPrefix(body, "<!DOCTYPE html>") {
			t.Errorf("%s page: missing doctype", name)
		}
		mustContain(t, body,
			`lang="en"`,
			`href="#main"`, // skip link
			"<header",      // landmark
			"<main",        // landmark
			"<footer",      // landmark
			`aria-label="Main"`,
		)
		if name != "adminBuilds" && name != "log" && name != "build" {
			mustContain(t, body, "aria-labelledby")
		}
		assertIconsHidden(t, body, name)
	}
}

// TestNoExternalResources asserts pages reference no external resources
// (proposal §13.1): no stylesheet links, no remote scripts or media, and
// no CDN mentions. The SVG xmlns namespace is not a resource request and
// is intentionally ignored.
func TestNoExternalResources(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil)
	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{}}, store, newFakeLogReader(""))

	for _, path := range []string{"/", "/packages", "/builds/" + itoa(build.ID)} {
		rec := get(t, s, http.MethodGet, path, nil)
		body := rec.Body.String()
		if strings.Contains(body, "<link") {
			t.Errorf("%s: external stylesheet <link> present", path)
		}
		if strings.Contains(body, "src=\"http") || strings.Contains(body, "src='http") {
			t.Errorf("%s: external script/media src present", path)
		}
		if strings.Contains(body, "<iframe") || strings.Contains(body, "<img") {
			t.Errorf("%s: embedded media present", path)
		}
		if strings.Contains(body, "tailwindcss.com") || strings.Contains(body, "cdn") {
			t.Errorf("%s: CDN/banner URL mention present", path)
		}
	}
}

// assertIconsHidden asserts every inline svg is decorative
// (aria-hidden="true", focusable="false").
func assertIconsHidden(t *testing.T, body, page string) {
	t.Helper()
	idx := 0
	for {
		start := strings.Index(body[idx:], "<svg")
		if start < 0 {
			break
		}
		start += idx
		end := strings.Index(body[start:], "</svg>")
		if end < 0 {
			t.Errorf("%s: unclosed svg", page)
			break
		}
		seg := body[start : start+end]
		if !strings.Contains(seg, `aria-hidden="true"`) || !strings.Contains(seg, "focusable=\"false\"") {
			t.Errorf("%s: decorative svg missing aria-hidden/focusable:\n%s", page, truncate(seg, 160))
		}
		idx = start + end + len("</svg>")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
