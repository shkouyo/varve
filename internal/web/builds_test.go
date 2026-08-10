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
	"strings"
	"testing"
	"time"
)

// TestBuildsListRenders asserts GET /builds lists the newest build rows
// with their package, status, machine and log links, and shows the empty
// state when there are no builds.
func TestBuildsListRenders(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil)
	seedWorker(t, store, "node-1")

	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))
	rec := get(t, s, http.MethodGet, "/builds", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /builds = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body,
		"/builds/"+itoa(build.ID),
		"demo-pkg",
		"/builds/"+itoa(build.ID)+"#log",
	)
	if strings.Contains(body, "No builds recorded yet") {
		t.Error("builds page must not show the empty state with rows present")
	}

	// Empty store renders the empty message.
	empty := newTestServer(t, testConfig(), &fakeOrchestrator{}, newTestDB(t), newFakeLogReader(""))
	rec = get(t, empty, http.MethodGet, "/builds", nil)
	mustContain(t, rec.Body.String(), "No builds recorded yet")
}

// TestBuildsListDuration asserts the builds list renders the wall-clock
// duration of finished builds in the Duration column.
func TestBuildsListDuration(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	now := time.Now().UTC()
	build := seedTimedBuild(t, store, pkg, now.Add(-2*time.Minute), now)
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))
	rec := get(t, s, http.MethodGet, "/builds", nil)
	body := rec.Body.String()
	mustContain(t, body, "Duration", "/builds/"+itoa(build.ID), "2m")
}

// TestBuildsListPagination asserts the builds page paginates and clamps
// out-of-range pages.
func TestBuildsListPagination(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	ids := make([]string, 0, 25)
	for i := 0; i < 25; i++ {
		b := seedBuild(t, store, pkg, "succeeded", nil, nil)
		ids = append(ids, b.ID)
	}
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))

	// ids[24] is the newest build (seq DESC) and sits on page 1; ids[0]
	// is the oldest and belongs on page 2.
	rec := get(t, s, http.MethodGet, "/builds", nil)
	body := rec.Body.String()
	mustContain(t, body, "Page 1 of 2", "25 builds", "/builds/"+ids[24])
	if strings.Contains(body, "/builds/"+ids[0]) {
		t.Error("page 1 must not contain the oldest build")
	}

	rec = get(t, s, http.MethodGet, "/builds?page=2", nil)
	body = rec.Body.String()
	mustContain(t, body, "Page 2 of 2", "/builds/"+ids[0])
	if strings.Contains(body, "/builds/"+ids[24]) {
		t.Error("page 2 must not contain the newest build")
	}

	// Out-of-range page clamps to the last page.
	rec = get(t, s, http.MethodGet, "/builds?page=99", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /builds?page=99 = %d, want 200", rec.Code)
	}
	mustContain(t, rec.Body.String(), "Page 2 of 2")
}

// TestBuildsListPageClamp asserts page numbers never 400: malformed and
// negative values clamp to page 1 and oversized values to the last
// page.
func TestBuildsListPageClamp(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	seedBuild(t, store, pkg, "succeeded", nil, nil)
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))
	for _, p := range []string{"0", "-1", "abc"} {
		rec := get(t, s, http.MethodGet, "/builds?page="+p, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "demo-pkg") {
			t.Errorf("page=%q = %d, want 200 with the build list rendered", p, rec.Code)
		}
	}
}

// TestDisplayArch pins the architecture display rule: "any" wins, then
// x86_64; exotic-only sets render as-is.
func TestDisplayArch(t *testing.T) {
	cases := map[string]string{
		"x86_64":         "x86_64",
		"aarch64|x86_64": "x86_64",
		"any":            "any",
		"any|x86_64":     "any",
		"aarch64":        "aarch64",
		"":               "",
	}
	for in, want := range cases {
		if got := displayArch(in); got != want {
			t.Errorf("displayArch(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestShortID pins the identifier truncation rule.
func TestShortID(t *testing.T) {
	if got := shortID("abc"); got != "abc" {
		t.Errorf("shortID(short) = %q, want abc", got)
	}
	if got := shortID("1234567890abcdef"); got != "12345678…" {
		t.Errorf("shortID(long) = %q, want 12345678…", got)
	}
}
