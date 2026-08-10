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
		"1.2 KiB",                             // humanized artifact size (1234 bytes)
		"package",                             // artifact type label
		"https://dl.example.org/pool/demo-pkg-1.2.3-1-x86_64.pkg.tar.zst", // download link
		"Build history",
	)
}

// TestDownloadButtonMatrix drives download_enabled × artifact set: the
// hero button appears only when downloads are enabled and the latest
// build carries exactly one package artifact, while the artifact card
// links every package artifact individually and side files (signature,
// srcinfo) never get a link.
func TestDownloadButtonMatrix(t *testing.T) {
	cases := []struct {
		name      string
		enabled   bool
		baseURI   string
		artifacts []db.Artifact
		wantHero  bool     // hero download button rendered
		heroText  string   // hero button label when present
		wantLinks []string // artifact-card download URLs that must render
		noLinks   []string // URLs that must not render anywhere
	}{
		{"enabled with one package", true, "https://dl.example.org/pool",
			[]db.Artifact{{File: "p.pkg.tar.zst", Kind: "package", Pkgname: "p"}}, true,
			"Download p.pkg.tar.zst", []string{"https://dl.example.org/pool/p.pkg.tar.zst"}, nil},
		{"disabled with artifact", false, "https://dl.example.org/pool",
			[]db.Artifact{{File: "p.pkg.tar.zst", Kind: "package", Pkgname: "p"}}, false,
			"", nil, []string{"https://dl.example.org/pool/p.pkg.tar.zst"}},
		{"enabled without artifact", true, "https://dl.example.org/pool", nil, false,
			"", nil, nil},
		{"enabled with only srcinfo", true, "https://dl.example.org/pool",
			[]db.Artifact{{File: ".SRCINFO", Kind: "srcinfo"}}, false,
			"", nil, []string{"https://dl.example.org/pool/.SRCINFO"}},
		{"enabled with split packages", true, "https://dl.example.org/pool",
			[]db.Artifact{
				{File: "a-1-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "a"},
				{File: "b-1-1-any.pkg.tar.zst", Kind: "package", Pkgname: "b"},
			}, false,
			"", []string{
				"https://dl.example.org/pool/a-1-1-x86_64.pkg.tar.zst",
				"https://dl.example.org/pool/b-1-1-any.pkg.tar.zst",
			}, nil},
		{"enabled with package plus side files", true, "https://dl.example.org/pool",
			[]db.Artifact{
				{File: "p-1-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "p"},
				{File: "p-1-1-x86_64.pkg.tar.zst.sig", Kind: "signature"},
				{File: ".SRCINFO", Kind: "srcinfo"},
			}, true,
			"Download p-1-1-x86_64.pkg.tar.zst",
			[]string{"https://dl.example.org/pool/p-1-1-x86_64.pkg.tar.zst"},
			[]string{
				"https://dl.example.org/pool/p-1-1-x86_64.pkg.tar.zst.sig",
				"https://dl.example.org/pool/.SRCINFO",
			}},
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
			// The hero button is the only element with this class combo
			// (gap-2 + rounded-lg + bg-blue-700); card links are plain
			// underlined text links.
			hero := `class="inline-flex items-center gap-2 rounded-lg bg-blue-700`
			if tc.wantHero {
				mustContain(t, body, hero, tc.heroText)
			} else if strings.Contains(body, hero) {
				t.Error("hero download button rendered although it should be hidden")
			}
			for _, link := range tc.wantLinks {
				mustContain(t, body, link)
			}
			for _, link := range tc.noLinks {
				if strings.Contains(body, link) {
					t.Errorf("download link %q rendered although it should be hidden", link)
				}
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

// TestPackageAURRow drives the AUR metadata row of the package page: a
// branch published to AUR ([aur] name set) renders the AUR package link
// plus the recorded push state (attempt time, short commit, sanitized
// failure summary when the last push failed), while a branch without an
// AUR name renders no AUR row at all. Most packages are not published,
// so a quiet page is the default and no "not enabled" placeholder is
// printed.
func TestPackageAURRow(t *testing.T) {
	store := newTestDB(t)
	publish := func(pkgbase string) db.Package {
		p := db.Package{
			Pkgbase:     pkgbase,
			Branch:      "main",
			VCSKind:     "git",
			Arch:        "x86_64",
			Pkgdesc:     "A demo package",
			Maintainers: []db.Maintainer{{Name: "Alice Example", Email: "alice@example.org"}},
			AURName:     "aur-" + pkgbase,
			AURSubmit:   true,
		}
		if err := store.UpsertPackage(testCtx, &p); err != nil {
			t.Fatalf("upsert package %q: %v", pkgbase, err)
		}
		return p
	}
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))

	// Not published to AUR: no AUR row, no AUR URL anywhere on the page.
	seedPackage(t, store, "plain-pkg", "A demo package")
	body := get(t, s, http.MethodGet, "/packages/plain-pkg", nil).Body.String()
	if strings.Contains(body, "aur.archlinux.org") {
		t.Error("unpublished package must not render an AUR row")
	}

	// Published, no push recorded yet: the link row renders alone.
	publish("demo-pkg")
	body = get(t, s, http.MethodGet, "/packages/demo-pkg", nil).Body.String()
	mustContain(t, body,
		"AUR",
		`href="https://aur.archlinux.org/packages/aur-demo-pkg"`,
		`target="_blank"`, `rel="noopener noreferrer"`,
	)
	if strings.Contains(body, "last push") {
		t.Error("AUR row must not show a push before any push was attempted")
	}

	// Successful push: the row gains the attempt time and short commit.
	when := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	commit := "cafebabecafebabecafebabecafebabecafebabe"
	if err := store.RecordAURPush(testCtx, "demo-pkg", commit, when, ""); err != nil {
		t.Fatalf("record AUR push: %v", err)
	}
	body = get(t, s, http.MethodGet, "/packages/demo-pkg", nil).Body.String()
	mustContain(t, body, "last push "+absTime(&when), "("+shortID(commit)+")")
	if strings.Contains(body, `class="mt-1 break-all text-red-800 dark:text-red-300"`) {
		t.Error("successful push must not render the failure summary")
	}

	// Failed push: the sanitized one-line summary joins the row.
	errMsg := "git push aur-demo-pkg: exit status 1:\r\n\tfatal: unable to access:\r\n\tPermission denied (publickey)"
	if err := store.RecordAURPush(testCtx, "demo-pkg", commit, when, errMsg); err != nil {
		t.Fatalf("record failed AUR push: %v", err)
	}
	body = get(t, s, http.MethodGet, "/packages/demo-pkg", nil).Body.String()
	mustContain(t, body,
		"last push "+absTime(&when),
		aurErrorSummary(errMsg), // control characters stripped
		"text-red-800",
	)
}

// TestPackageMaintainerNames asserts the maintainers row renders display
// names only. The email of a named entry never appears and no mailto
// link is emitted, while legacy email-only entries fall back to the
// address so the row still shows something.
func TestPackageMaintainerNames(t *testing.T) {
	store := newTestDB(t)
	p := db.Package{
		Pkgbase: "named-pkg",
		Branch:  "main",
		VCSKind: "git",
		Arch:    "x86_64",
		Pkgdesc: "A demo package",
		Maintainers: []db.Maintainer{
			{Name: "Alice Example", Email: "alice@example.org"},
			{Email: "legacy@example.org"},
		},
	}
	if err := store.UpsertPackage(testCtx, &p); err != nil {
		t.Fatalf("upsert package: %v", err)
	}
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))
	body := get(t, s, http.MethodGet, "/packages/named-pkg", nil).Body.String()
	mustContain(t, body, "Alice Example", "legacy@example.org")
	if strings.Contains(body, "alice@example.org") {
		t.Error("email of a named maintainer must not render")
	}
	if strings.Contains(body, "mailto:") {
		t.Error("maintainers row must not render mailto links")
	}
}

// TestDownloadFor pins the hero-link rules: one package artifact links,
// a split build has no single hero button (the card covers it), and
// disabled or buildless states are nil.
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
	if downloadFor(true, "https://dl.example.org", &db.Build{Artifacts: []db.Artifact{
		{File: ".SRCINFO", Kind: "srcinfo"},
	}}) != nil {
		t.Error("downloadFor must be nil without a package artifact")
	}
	split := &db.Build{Artifacts: []db.Artifact{
		{File: "a-1-1-x86_64.pkg.tar.zst", Kind: "package"},
		{File: "b-1-1-any.pkg.tar.zst", Kind: "package"},
	}}
	if downloadFor(true, "https://dl.example.org", split) != nil {
		t.Error("downloadFor must be nil when the build split into several packages")
	}
}

// TestPackageDetailMetadata asserts the .SRCINFO metadata (url, licenses,
// conflicts, provides) stored on the package row flows into the package
// page data for the template.
func TestPackageDetailMetadata(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil)
	err := store.WithTx(testCtx, func(tx *db.Tx) error {
		return tx.UpdatePackageAfterBuild(testCtx, pkg.Pkgbase, db.PackageUpdate{
			CurrentVersion: "1.2.3-1", Pkgdesc: "A demo package",
			SrcinfoHash: "srcinfo-hash", UpstreamRef: "ref", BuildID: build.ID,
			URL:       "https://example.org/demo-pkg",
			Licenses:  []string{"GPL", "MIT"},
			Conflicts: []string{"old-demo"},
			Provides:  []string{"demo-lib"},
			Pkgname:   []string{"demo-pkg"},
			Source:    []string{"https://example.org/demo-pkg.tar.gz"},
			Pkgver:    "1.2.3",
			Pkgrel:    "1",
		})
	})
	if err != nil {
		t.Fatalf("update package metadata: %v", err)
	}
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))

	data, err := s.packageData(newRequest(t, http.MethodGet, "/packages/demo-pkg"), "demo-pkg", 1)
	if err != nil {
		t.Fatalf("packageData: %v", err)
	}
	if data.Pkg.URL != "https://example.org/demo-pkg" {
		t.Errorf("Pkg.URL = %q, want https://example.org/demo-pkg", data.Pkg.URL)
	}
	if len(data.Pkg.Licenses) != 2 || data.Pkg.Licenses[0] != "GPL" || data.Pkg.Licenses[1] != "MIT" {
		t.Errorf("Pkg.Licenses = %v, want [GPL MIT]", data.Pkg.Licenses)
	}
	if len(data.Pkg.Conflicts) != 1 || data.Pkg.Conflicts[0] != "old-demo" {
		t.Errorf("Pkg.Conflicts = %v, want [old-demo]", data.Pkg.Conflicts)
	}
	if len(data.Pkg.Provides) != 1 || data.Pkg.Provides[0] != "demo-lib" {
		t.Errorf("Pkg.Provides = %v, want [demo-lib]", data.Pkg.Provides)
	}
	if len(data.Pkg.Pkgname) != 1 || data.Pkg.Pkgname[0] != "demo-pkg" {
		t.Errorf("Pkg.Pkgname = %v, want [demo-pkg]", data.Pkg.Pkgname)
	}
	if len(data.Pkg.Source) != 1 || data.Pkg.Source[0] != "https://example.org/demo-pkg.tar.gz" {
		t.Errorf("Pkg.Source = %v, want the source list", data.Pkg.Source)
	}
	if data.Pkg.Pkgver != "1.2.3" || data.Pkg.Pkgrel != "1" {
		t.Errorf("Pkg.Pkgver/Pkgrel = %q/%q, want 1.2.3/1", data.Pkg.Pkgver, data.Pkg.Pkgrel)
	}
}

// TestPackageHistoryDuration asserts the package build history renders
// the wall-clock duration of finished builds in the Duration column.
func TestPackageHistoryDuration(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	now := time.Now().UTC()
	build := seedTimedBuild(t, store, pkg, now.Add(-2*time.Minute), now)
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))
	rec := get(t, s, http.MethodGet, "/packages/demo-pkg", nil)
	body := rec.Body.String()
	mustContain(t, body, "Duration", "/builds/"+itoa(build.ID), "2m")
}

// TestPackageHistoryPagination asserts the build history paginates per
// package: page 1 holds the newest 20, page 2 the rest, and an
// out-of-range page is clamped to the last one.
func TestPackageHistoryPagination(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	for i := 0; i < 25; i++ {
		seedBuild(t, store, pkg, "succeeded", nil, nil)
	}
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))

	page1, err := s.packageData(newRequest(t, http.MethodGet, "/packages/demo-pkg"), "demo-pkg", 1)
	if err != nil {
		t.Fatalf("packageData page 1: %v", err)
	}
	if page1.Total != 25 || page1.Pages != 2 || len(page1.Builds) != 20 {
		t.Errorf("page 1 = total %d pages %d builds %d, want 25/2/20", page1.Total, page1.Pages, len(page1.Builds))
	}
	page2, err := s.packageData(newRequest(t, http.MethodGet, "/packages/demo-pkg"), "demo-pkg", 2)
	if err != nil {
		t.Fatalf("packageData page 2: %v", err)
	}
	if len(page2.Builds) != 5 {
		t.Errorf("page 2 builds = %d, want 5", len(page2.Builds))
	}
	clamped, err := s.packageData(newRequest(t, http.MethodGet, "/packages/demo-pkg"), "demo-pkg", 99)
	if err != nil {
		t.Fatalf("packageData out-of-range page: %v", err)
	}
	if clamped.Page != 2 || len(clamped.Builds) != 5 {
		t.Errorf("clamped page = %d builds %d, want page 2 with 5 builds", clamped.Page, len(clamped.Builds))
	}
}

// TestPackagesScopeArchFilter drives the ?scope= and ?arch= matrix:
// scope narrows the match to the package name or description, arch to
// the architecture set, and both combine; invalid values are a 400.
func TestPackagesScopeArchFilter(t *testing.T) {
	store := newTestDB(t)
	upsert := func(pkgbase, desc, arch string) {
		p := db.Package{
			Pkgbase:     pkgbase,
			Branch:      "main",
			VCSKind:     "git",
			Arch:        arch,
			Pkgdesc:     desc,
			Maintainers: []db.Maintainer{{Email: "alice@example.com"}},
		}
		if err := store.UpsertPackage(testCtx, &p); err != nil {
			t.Fatalf("upsert package %q: %v", pkgbase, err)
		}
	}
	upsert("alpha-tools", "first set of tools", "x86_64")
	upsert("beta-libs", "second set of libraries", "any")
	upsert("gamma-utils", "gamma utilities", "x86_64|any")

	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))

	// Package descriptions are build-derived: populate them through the
	// post-build update path like a real build would.
	for _, p := range []struct {
		pkgbase, desc, arch string
	}{
		{"alpha-tools", "first set of tools", "x86_64"},
		{"beta-libs", "second set of libraries", "any"},
		{"gamma-utils", "gamma utilities", "x86_64|any"},
	} {
		err := store.WithTx(testCtx, func(tx *db.Tx) error {
			return tx.UpdatePackageAfterBuild(testCtx, p.pkgbase, db.PackageUpdate{
				CurrentVersion: "1.0-1", Pkgdesc: p.desc, SrcinfoHash: "srcinfo-hash",
			})
		})
		if err != nil {
			t.Fatalf("update package %q: %v", p.pkgbase, err)
		}
	}

	cases := []struct {
		name string
		q    string
		want string
		nope string
	}{
		{"name scope", "?q=tools&scope=name", "alpha-tools", "beta-libs"},
		{"desc scope", "?q=tools&scope=desc", "alpha-tools", "beta-libs"},
		{"name only excludes desc", "?q=utilities&scope=name", "", "gamma-utils"},
		{"desc only finds desc", "?q=utilities&scope=desc", "gamma-utils", "alpha-tools"},
		{"both scopes", "?q=tools", "alpha-tools", "beta-libs"},
		{"case insensitive", "?q=Tools&scope=name", "alpha-tools", "beta-libs"},
		{"arch x86_64", "?arch=x86_64", "alpha-tools", "beta-libs"},
		{"arch any", "?arch=any", "beta-libs", "alpha-tools"},
		{"mixed set matches both arches", "?q=gamma&arch=x86_64", "gamma-utils", "beta-libs"},
		{"combined filter excludes", "?q=tools&arch=any", "", "alpha-tools"},
		{"combined filter keeps", "?q=lib&arch=any", "beta-libs", "alpha-tools"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, s, http.MethodGet, "/packages"+tc.q, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /packages%s = %d, want 200", tc.q, rec.Code)
			}
			body := rec.Body.String()
			if tc.want != "" {
				mustContain(t, body, tc.want)
			}
			if tc.nope != "" && strings.Contains(body, tc.nope) {
				t.Errorf("%s: result must not contain %q", tc.q, tc.nope)
			}
		})
	}
}

// TestPackagesValidation asserts malformed input handling: over-long
// search terms and invalid filter values are a 400, while page numbers
// clamp to page 1 instead of failing.
func TestPackagesValidation(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, newTestDB(t), newFakeLogReader(""))
	long := strings.Repeat("a", 201)
	rec := get(t, s, http.MethodGet, "/packages?q="+long, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("long q = %d, want 400", rec.Code)
	}
	for _, q := range []string{"scope=bogus", "arch=bogus", "scope=name&arch=weird"} {
		rec = get(t, s, http.MethodGet, "/packages?"+q, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("?%s = %d, want 400", q, rec.Code)
		}
	}
	// Page numbers never 400: malformed and negative values clamp to 1.
	for _, p := range []string{"0", "-1", "abc"} {
		rec = get(t, s, http.MethodGet, "/packages?page="+p, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("page=%q = %d, want 200 (clamped)", p, rec.Code)
		}
	}
}
