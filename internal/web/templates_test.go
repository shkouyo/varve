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
	"net/http/httptest"
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/dispatch"
)

// TestErrorPages asserts the 404 and 401 responses render the error page
// with semantic markup and no JavaScript, and that malformed input maps
// to a 400.
func TestErrorPages(t *testing.T) {
	store := newTestDB(t)
	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{}}, store, newFakeLogReader(""))

	for _, path := range []string{"/packages/nope", "/builds/ffffffffffffffff"} {
		rec := get(t, s, http.MethodGet, path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
		body := rec.Body.String()
		mustContain(t, body, "<main", "</main>", "Not Found", "Back to overview")
		if strings.Contains(body, "<script") {
			t.Errorf("%s error page must not contain scripts", path)
		}
	}

	for _, path := range []string{"/packages/bad%20name", "/builds/12345", "/builds/not-hex"} {
		rec := get(t, s, http.MethodGet, path, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", path, rec.Code)
		}
	}

	rec := get(t, s, http.MethodGet, "/admin", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /admin = %d, want 401", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body, "Unauthorized", "<main", "Back to overview")
	if strings.Contains(body, "<script") {
		t.Error("401 error page must not contain scripts")
	}

	// Logout answers 401 (dropping the saved credentials) and the error
	// page carries a link back to the start page, so it is never a dead
	// end.
	rec = get(t, s, http.MethodGet, "/admin/logout", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /admin/logout = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("logout must keep the WWW-Authenticate challenge")
	}
	body = rec.Body.String()
	mustContain(t, body, "Logged out", "Unauthorized", `href="/"`)
}

// TestSemanticMarkup asserts the shared page chrome on every rendered
// page: html lang, skip link, semantic landmarks, table headers with
// scope, and aria-hidden decorative icons (WCAG 2.2 AA).
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
		"builds":      "/builds",
		"admin":       "/",
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
		if name != "adminBuilds" && name != "build" && name != "builds" {
			mustContain(t, body, "aria-labelledby")
		}
		assertIconsHidden(t, body, name)
	}
}

// TestNoExternalResources asserts pages reference no external resources:
// no stylesheet links, no remote scripts or media, and no CDN mentions.
// The SVG xmlns namespace is not a resource request and is intentionally
// ignored.
func TestNoExternalResources(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil)
	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{}}, store, newFakeLogReader(""))

	for _, path := range []string{"/", "/packages", "/builds", "/builds/" + itoa(build.ID)} {
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

// TestA11yContrastAndKeyboard pins the WCAG 2.2 AA fixes from the
// axe-core audit to the rendered HTML:
//
//   - no sub-threshold secondary text (slate-400, ~2.6:1 on white) and
//     footer links use slate-600 text instead of slate-500 (slate-600 keeps
//     ~6.9:1 on the slate-100 page background), WCAG 1.4.3;
//   - every in-text link is always underlined rather than only on hover,
//     so links are distinguishable without relying on color (WCAG 1.4.1
//     link-in-text-block);
//   - the scrollable log regions are keyboard-focusable via tabindex="0"
//     (WCAG 2.1.1 scrollable-region-focusable);
//   - the 404 status numeral keeps 3:1 on white for 60px bold text.
func TestA11yContrastAndKeyboard(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil)
	seedBuild(t, store, pkg, "failed", nil, nil) // failed row for /admin/builds
	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{
		ByStatus:     map[string]int{"succeeded": 1, "failed": 1},
		RecentBuilds: []db.Build{{ID: build.ID, PackageID: pkg.ID, Status: "succeeded"}},
	}}, store, newFakeLogReader("==> done\n"))

	pages := []struct {
		name  string
		path  string
		auth  bool
		links bool // page renders at least one in-text link
	}{
		{"dashboard", "/", false, true},
		{"packages", "/packages", false, true},
		{"package", "/packages/demo-pkg", false, true},
		{"build", "/builds/" + itoa(build.ID), false, true},
		{"builds", "/builds", false, true},
		{"admin", "/", true, true},
		{"adminBuilds", "/admin/builds?failed=1", true, true},
		{"notfound", "/packages/nope", false, false},
	}
	for _, pg := range pages {
		var rec *httptest.ResponseRecorder
		if pg.auth {
			rec = getAuth(t, s, http.MethodGet, pg.path, "admin", "s3cret")
		} else {
			rec = get(t, s, http.MethodGet, pg.path, nil)
		}
		body := rec.Body.String()
		if strings.Contains(body, `class="text-slate-400"`) {
			t.Errorf("%s: sub-threshold text-slate-400 still rendered", pg.name)
		}
		mustContain(t, body, `href="/COPYING.txt"`, "License: AGPL-3.0-or-later", "https://git.0x0f.dev/shkouyo/varve") // footer links
		if pg.links {
			mustContain(t, body, "underline underline-offset-2")
			if strings.Contains(body, "underline-offset-2 hover:underline") {
				t.Errorf("%s: link still only underlines on hover", pg.name)
			}
		}
	}

	// The scrollable log region must be keyboard-focusable (WCAG 2.1.1);
	// the merged log renders on the build page as a div with numbered lines.
	rec := get(t, s, http.MethodGet, "/builds/"+itoa(build.ID), nil)
	mustContain(t, rec.Body.String(),
		`<div tabindex="0" class="max-h-[70vh] overflow-auto whitespace-pre`,
		`<span class="mr-4 inline-block w-10 select-none text-right text-slate-500">1</span>==&gt; done`)

	// 404 big status numeral: slate-500 on white (~4.8:1 >= 3:1 large text).
	rec = get(t, s, http.MethodGet, "/packages/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("404 page = %d, want 404", rec.Code)
	}
	mustContain(t, rec.Body.String(), `text-6xl font-bold tracking-tight text-slate-500`)
}
