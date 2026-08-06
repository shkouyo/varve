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

	"git.0x0f.dev/varve/internal/db"
)

// TestPackagesListPagination asserts ?page= pagination passes through to
// the list (page 1 holds the first 20 of 25, page 2 the remaining 5).
func TestPackagesListPagination(t *testing.T) {
	store := newTestDB(t)
	// Zero-padded names keep the lexicographic ORDER BY identical to the
	// numeric order.
	for i := 0; i < 25; i++ {
		seedPackage(t, store, "pkg-"+pad3(i), "package number")
	}
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))

	rec := get(t, s, http.MethodGet, "/packages", nil)
	body := rec.Body.String()
	mustContain(t, body, "pkg-000", "pkg-019")
	if strings.Contains(body, "pkg-020") {
		t.Error("page 1 should not contain pkg-020")
	}
	mustContain(t, body, "Page 1 of 2", "25 packages")

	rec = get(t, s, http.MethodGet, "/packages?page=2", nil)
	body = rec.Body.String()
	mustContain(t, body, "pkg-020", "pkg-024", "Page 2 of 2")
	if strings.Contains(body, "pkg-019") {
		t.Error("page 2 should not contain pkg-019")
	}
}

// pad3 formats n zero-padded to three digits.
func pad3(n int) string {
	return string([]byte{
		byte('0' + n/100),
		byte('0' + (n/10)%10),
		byte('0' + n%10),
	})
}

// TestPackagesListSearch asserts ?q= filters by pkgbase.
func TestPackagesListSearch(t *testing.T) {
	store := newTestDB(t)
	seedPackage(t, store, "alpha-tools", "first set of tools")
	seedPackage(t, store, "beta-libs", "second set of libraries")
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))

	rec := get(t, s, http.MethodGet, "/packages?q=alpha", nil)
	body := rec.Body.String()
	mustContain(t, body, "alpha-tools")
	if strings.Contains(body, "beta-libs") {
		t.Error("search for alpha should not include beta-libs")
	}
}

// TestPackageDetailRenders asserts the package page shows version,
// metadata, artifacts and the download button when enabled.
func TestPackageDetailRenders(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	artifacts := []db.Artifact{{File: "demo-pkg-1.2.3-1-x86_64.pkg.tar.zst", Kind: "package", Size: 1234}}
	build := seedBuild(t, store, pkg, "succeeded", artifacts, nil)
	setPackageBuild(t, store, pkg.Pkgbase, build.ID)

	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))
	rec := get(t, s, http.MethodGet, "/packages/demo-pkg", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /packages/demo-pkg = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body,
		"demo-pkg",
		"1.2.3-1",                             // version
		"demo-pkg-1.2.3-1-x86_64.pkg.tar.zst", // artifact file
		"https://dl.example.org/pool/demo-pkg-1.2.3-1-x86_64.pkg.tar.zst", // download link
		"Build history",
	)
}

// TestDownloadButtonMatrix drives download_enabled × artifact presence:
// the button appears only when downloads are enabled and the latest build
// carries an artifact.
func TestDownloadButtonMatrix(t *testing.T) {
	cases := []struct {
		name      string
		enabled   bool
		baseURI   string
		artifacts []db.Artifact
		wantLink  bool
	}{
		{"enabled with artifact", true, "https://dl.example.org/pool",
			[]db.Artifact{{File: "p.pkg.tar.zst", Kind: "package"}}, true},
		{"disabled with artifact", false, "https://dl.example.org/pool",
			[]db.Artifact{{File: "p.pkg.tar.zst", Kind: "package"}}, false},
		{"enabled without artifact", true, "https://dl.example.org/pool", nil, false},
		{"enabled with only srcinfo", true, "https://dl.example.org/pool",
			[]db.Artifact{{File: ".SRCINFO", Kind: "srcinfo"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestDB(t)
			pkg := seedPackage(t, store, "demo-pkg", "A demo package")
			build := seedBuild(t, store, pkg, "succeeded", tc.artifacts, nil)
			setPackageBuild(t, store, pkg.Pkgbase, build.ID)

			cfg := testConfig()
			cfg.Web.DownloadEnabled = tc.enabled
			cfg.Web.DownloadBaseURI = tc.baseURI
			s := newTestServer(t, cfg, &fakeOrchestrator{}, store, newFakeLogReader(""))
			rec := get(t, s, http.MethodGet, "/packages/demo-pkg", nil)
			body := rec.Body.String()
			if tc.wantLink {
				mustContain(t, body, tc.baseURI+"/p.pkg.tar.zst")
			} else if strings.Contains(body, "Download") {
				t.Error("download button rendered although downloads should be hidden")
			}
		})
	}
}

// TestPackageNotFound asserts a missing package renders the 404 error
// page.
func TestPackageNotFound(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, newTestDB(t), newFakeLogReader(""))
	rec := get(t, s, http.MethodGet, "/packages/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /packages/nope = %d, want 404", rec.Code)
	}
	mustContain(t, rec.Body.String(), "Not Found", "Package not found")
}

// TestDownloadFor prefers the package artifact over other kinds.
func TestDownloadFor(t *testing.T) {
	latest := &db.Build{Artifacts: []db.Artifact{
		{File: "pkg-1-1-x86_64.pkg.tar.zst", Kind: "package"},
		{File: "sig", Kind: "signature"},
	}}
	link := downloadFor(true, "https://dl.example.org", latest)
	if link == nil || link.URL != "https://dl.example.org/pkg-1-1-x86_64.pkg.tar.zst" {
		t.Fatalf("downloadFor = %+v, want package artifact link", link)
	}
	if downloadFor(false, "https://dl.example.org", latest) != nil {
		t.Error("downloadFor must be nil when disabled")
	}
	if downloadFor(true, "https://dl.example.org", nil) != nil {
		t.Error("downloadFor must be nil without a build")
	}
}
