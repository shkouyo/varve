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
	"regexp"
	"strings"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/dispatch"
)

// TestRefreshGating pins the removal of auto-refresh: no page renders a
// meta refresh or a reload timer, the build page carries the resumable
// SSE log client only while the build is active (PageActive), and
// terminal build and error pages render no script at all.
func TestRefreshGating(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	term := seedBuild(t, store, pkg, "failed", nil, nil)
	active := seedActiveBuild(t, store, pkg, "running")
	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{}}, store, newFakeLogReader("line1\n"))

	// No page auto-refreshes: no meta tag, no reload timer.
	for _, path := range []string{"/", "/packages", "/builds", "/builds/" + itoa(active.ID)} {
		rec := get(t, s, http.MethodGet, path, nil)
		body := rec.Body.String()
		if strings.Contains(body, "http-equiv=\"refresh\"") || strings.Contains(body, "setInterval") || strings.Contains(body, "location.reload()") {
			t.Errorf("%s: auto-refresh tag or timer still rendered", path)
		}
	}

	// The active build page is the only page with a script: the SSE
	// log client.
	rec := get(t, s, http.MethodGet, "/builds/"+itoa(active.ID), nil)
	if !strings.Contains(rec.Body.String(), "new EventSource(") {
		t.Error("active build page must carry the SSE log client")
	}

	// Terminal build and error pages are frozen: no script at all.
	for _, path := range []string{"/builds/" + itoa(term.ID), "/packages/nope"} {
		rec := get(t, s, http.MethodGet, path, nil)
		if strings.Contains(rec.Body.String(), "<script") {
			t.Errorf("%s: frozen page must carry no script", path)
		}
	}
}

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

// TestFooterRenderTime asserts every page footer carries the source link
// ("Powered by Varve"), the license link, and a live render time scaled
// to its unit: whole milliseconds at 1ms or more, microseconds below,
// measured by the buffered render wrapper.
func TestFooterRenderTime(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	seedBuild(t, store, pkg, "succeeded", nil, nil)
	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{}}, store, newFakeLogReader(""))
	ms := regexp.MustCompile(`Render: \d+(ms|µs)`)
	for _, path := range []string{"/", "/packages", "/packages/demo-pkg", "/builds", "/packages/nope"} {
		rec := get(t, s, http.MethodGet, path, nil)
		body := rec.Body.String()
		mustContain(t, body,
			`href="https://git.0x0f.dev/shkouyo/varve"`,
			"Powered by Varve",
			`href="/COPYING.txt"`,
			"License: AGPL-3.0-or-later",
		)
		if !ms.MatchString(body) {
			t.Errorf("%s: footer render time missing (want %q in body)", path, ms)
		}
	}
}

// TestRenderTimeUnits pins the footer render-time scaling at the unit
// boundary: 1ms and up render as whole milliseconds, everything below
// as whole microseconds, and a sub-microsecond elapsed render reads as
// 1µs rather than a bare zero.
func TestRenderTimeUnits(t *testing.T) {
	cases := []struct {
		elapsed time.Duration
		want    string
	}{
		{0, "1µs"},                     // sub-microsecond renders clamp to 1µs
		{500 * time.Nanosecond, "1µs"}, // 0.5µs truncates to 0, clamped
		{999 * time.Microsecond, "999µs"},
		{1000 * time.Microsecond, "1ms"}, // exactly 1ms crosses to ms
		{1500 * time.Microsecond, "1ms"}, // whole milliseconds truncate
		{5 * time.Millisecond, "5ms"},
	}
	for _, tc := range cases {
		b := base{renderStart: time.Now().Add(-tc.elapsed)}
		if got := b.RenderTime(); got != tc.want {
			t.Errorf("RenderTime after %v: got %q, want %q", tc.elapsed, got, tc.want)
		}
	}
}

// TestFooterLayout pins the footer split into two opposite groups: the
// source link and render time sit in a left-hand list, the license link
// in a right-hand one, and the flex nav pushes them apart with
// justify-between so they never crowd the center (narrow screens wrap
// the right-hand group to its own line instead of overlapping).
func TestFooterLayout(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	seedBuild(t, store, pkg, "succeeded", nil, nil)
	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{}}, store, newFakeLogReader(""))

	rec := get(t, s, http.MethodGet, "/", nil)
	body := rec.Body.String()
	start := strings.Index(body, "<footer")
	if start < 0 {
		t.Fatal("page renders no <footer> landmark")
	}
	end := strings.Index(body[start:], "</footer>")
	if end < 0 {
		t.Fatal("<footer> has no closing tag")
	}
	footer := body[start : start+end]

	mustContain(t, footer, `aria-label="Footer"`, "justify-between")

	// The two groups are sibling lists, each carrying exactly one item.
	uls := listBlocks(footer)
	if len(uls) != 2 {
		t.Fatalf("footer renders %d list groups, want 2", len(uls))
	}
	mustContain(t, uls[0], `href="https://git.0x0f.dev/shkouyo/varve"`, "Powered by Varve", "Render:")
	mustContain(t, uls[1], `href="/COPYING.txt"`, "License: AGPL-3.0-or-later")
}

// listBlocks returns the text of every <ul>...</ul> element in order.
func listBlocks(s string) []string {
	var out []string
	for i := 0; ; {
		start := strings.Index(s[i:], "<ul")
		if start < 0 {
			break
		}
		start += i
		end := strings.Index(s[start:], "</ul>")
		if end < 0 {
			break
		}
		end += start
		out = append(out, s[start:end])
		i = end + len("</ul>")
	}
	return out
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

	// The scrollable log region must be keyboard-focusable (WCAG 2.1.1)
	// and exposed as a log region; the merged log renders on the build
	// page with the #log anchor on its heading.
	rec := get(t, s, http.MethodGet, "/builds/"+itoa(build.ID), nil)
	mustContain(t, rec.Body.String(),
		`id="log"`,
		`max-h-[70vh] overflow-auto whitespace-pre`,
		`role="log"`,
		`aria-label="Build log"`,
		`==&gt; done`)

	// 404 big status numeral: slate-500 on white (~4.8:1 >= 3:1 large text).
	rec = get(t, s, http.MethodGet, "/packages/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("404 page = %d, want 404", rec.Code)
	}
	mustContain(t, rec.Body.String(), `text-6xl font-bold tracking-tight text-slate-500`)
}
