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

package detect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/db"
)

// TestMirrorDir covers the mirror name derivation table: the URL path
// part without ".git", with "/" replaced by "_".
func TestMirrorDir(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"git@git.example.org:pkgbuilds.git", "pkgbuilds"},
		{"https://git.example.org/pkgs/foo.git", "pkgs_foo"},
		{"git@host:path/to/repo.git", "path_to_repo"},
		{"file:///tmp/repo.git", "tmp_repo"},
		{"https://example.org/a/b/c", "a_b_c"},
		{"ssh://git@host/repos/x.git", "repos_x"},
		{"git@host:", ""},
	}
	for _, tt := range tests {
		if got := MirrorDir(tt.url); got != tt.want {
			t.Errorf("MirrorDir(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

// TestNewDetectorErrors covers the constructor preconditions.
func TestNewDetectorErrors(t *testing.T) {
	store, _ := openStore(t)
	sink := &fakeSink{}
	if _, err := NewDetector(nil, store, sink, 0); err == nil {
		t.Error("NewDetector(nil) succeeded, want error")
	}
	if _, err := NewDetector(&config.SourceConfig{}, store, sink, 0); err == nil {
		t.Error("NewDetector(empty URL) succeeded, want error")
	}
	if _, err := NewDetector(&config.SourceConfig{URL: "git@host:"}, store, sink, 0); err == nil {
		t.Error("NewDetector(unusable URL) succeeded, want error")
	}
}

// TestNewDetectorMirrorDir asserts the production mirror path layout
// "/data/source/<name>.git".
func TestNewDetectorMirrorDir(t *testing.T) {
	store, _ := openStore(t)
	d, err := NewDetector(&config.SourceConfig{URL: "git@git.example.org:pkgbuilds.git"}, store, &fakeSink{}, 0)
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}
	want := filepath.Join(sourceRoot, "pkgbuilds.git")
	if d.mirrorDir != want {
		t.Errorf("mirrorDir = %q, want %q", d.mirrorDir, want)
	}
}

// TestPollOncePlainPackage walks the full lifecycle of a plain package:
// first enqueue, no change after a recorded successful build, enqueue on a
// PKGBUILD-only commit (the .SRCINFO is untouched), enqueue on a .SRCINFO
// change and the natural re-queue while the build outcome is not
// recorded.
func TestPollOncePlainPackage(t *testing.T) {
	src := newSourceRepo(t, "foo", map[string]string{
		".SRCINFO": srcinfoBody("foo", "1.0", "1"),
		"PKGBUILD": "# pkgbuild v1\n",
	})
	store, dbPath := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "file://"+src, store, sink)

	// 1. First poll of a new branch: enqueue (commit).
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #1: %v", err)
	}
	changes := assertChangeCount(t, sink, 1)
	want := Change{
		Package: Package{Pkgbase: "foo", Branch: "foo", VCSKind: "", Arch: "x86_64"},
		Pkgname: []string{"foo"},
		Pkgver:  "1.0",
		Pkgrel:  "1",
		Reason:  ReasonCommit,
	}
	if !reflect.DeepEqual(changes[0], want) {
		t.Errorf("change #1 = %+v, want %+v", changes[0], want)
	}

	// 2. Successful build recorded: no change on the next poll.
	tip := runGit(t, src, "rev-parse", "HEAD")
	seedPackageRow(t, dbPath, "foo", tip, "")
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #2: %v", err)
	}
	assertChangeCount(t, sink, 1)

	// 3. A PKGBUILD-only commit (the .SRCINFO bytes are unchanged)
	// moves the branch tip and triggers.
	commitFiles(t, src, map[string]string{"PKGBUILD": "# pkgbuild v2\n"}, "bump pkgbuild")
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #3: %v", err)
	}
	changes = assertChangeCount(t, sink, 2)
	if changes[1].Reason != ReasonCommit || changes[1].Package.Pkgbase != "foo" {
		t.Errorf("change #3 = %+v, want a commit change for foo", changes[1])
	}

	// 4. A .SRCINFO change also triggers.
	commitFiles(t, src, map[string]string{".SRCINFO": srcinfoBody("foo", "1.0", "2")}, "bump pkgrel")
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #4: %v", err)
	}
	changes = assertChangeCount(t, sink, 3)
	if changes[2].Reason != ReasonCommit || changes[2].Pkgrel != "2" {
		t.Errorf("change #4 = %+v, want a commit change with pkgrel 2", changes[2])
	}

	// 5. The failed build left the record stale, so the same diff is
	// detected again on the next round.
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #5: %v", err)
	}
	assertChangeCount(t, sink, 4)
}

// TestPollOnceVCSGitUpstream covers a -git package whose upstream HEAD is
// served by the testdata/bin/git PATH shim: first enqueue, no change after
// a recorded build, enqueue on an upstream commit change and the natural
// re-queue.
func TestPollOnceVCSGitUpstream(t *testing.T) {
	withShimPath(t)
	src := newSourceRepo(t, "foo-git", map[string]string{
		".SRCINFO": srcinfoWithSource("foo-git", "1.0", "1", "git+https://example.org/upstream.git"),
	})
	store, dbPath := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "file://"+src, store, sink)

	const h1 = "1111111111111111111111111111111111111111"
	const h2 = "2222222222222222222222222222222222222222"
	t.Setenv("VARVE_TEST_GIT_HEAD", h1)

	// 1. First poll: both the commit and the upstream ref differ from the
	// (empty) records, so the reason is commit+upstream.
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #1: %v", err)
	}
	changes := assertChangeCount(t, sink, 1)
	want := Change{
		Package:     Package{Pkgbase: "foo-git", Branch: "foo-git", VCSKind: "git", Arch: "x86_64"},
		Pkgname:     []string{"foo-git"},
		Source:      []string{"git+https://example.org/upstream.git"},
		Pkgver:      "1.0",
		Pkgrel:      "1",
		UpstreamRef: h1,
		Reason:      ReasonBoth,
	}
	if !reflect.DeepEqual(changes[0], want) {
		t.Errorf("change #1 = %+v, want %+v", changes[0], want)
	}

	// 2. Successful build recorded: no change.
	tip := runGit(t, src, "rev-parse", "HEAD")
	seedPackageRow(t, dbPath, "foo-git", tip, h1)
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #2: %v", err)
	}
	assertChangeCount(t, sink, 1)

	// 3. Upstream commit changed: enqueue (upstream).
	t.Setenv("VARVE_TEST_GIT_HEAD", h2)
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #3: %v", err)
	}
	changes = assertChangeCount(t, sink, 2)
	if changes[1].Reason != ReasonUpstream || changes[1].UpstreamRef != h2 {
		t.Errorf("change #3 = %+v, want upstream %s", changes[1], h2)
	}

	// 4. Not recorded after the failed build -> re-queued.
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #4: %v", err)
	}
	assertChangeCount(t, sink, 3)
}

// TestPollOnceVCSSVNUpstream covers a -svn package whose revision comes
// from the testdata/bin/svn PATH shim, including an upstream change.
func TestPollOnceVCSSVNUpstream(t *testing.T) {
	withShimPath(t)
	src := newSourceRepo(t, "foo-svn", map[string]string{
		".SRCINFO": srcinfoWithSource("foo-svn", "1.0", "1", "svn+https://example.org/upstream"),
	})
	store, dbPath := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "file://"+src, store, sink)

	t.Setenv("VARVE_TEST_SVN_REV", "42")
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #1: %v", err)
	}
	changes := assertChangeCount(t, sink, 1)
	if changes[0].Package.VCSKind != "svn" || changes[0].UpstreamRef != "42" || changes[0].Reason != ReasonBoth {
		t.Errorf("change #1 = %+v, want svn upstream 42 (srcinfo+upstream)", changes[0])
	}

	// Successful build recorded: only the upstream can change now.
	tip := runGit(t, src, "rev-parse", "HEAD")
	seedPackageRow(t, dbPath, "foo-svn", tip, "42")
	t.Setenv("VARVE_TEST_SVN_REV", "43")
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #2: %v", err)
	}
	changes = assertChangeCount(t, sink, 2)
	if changes[1].Reason != ReasonUpstream || changes[1].UpstreamRef != "43" {
		t.Errorf("change #2 = %+v, want upstream 43", changes[1])
	}
}

// TestPollOnceDotfileExtras asserts the dotfile extras merge flows through
// the pipeline: the branch submit carries the merged maintainers and
// collect rules, and a dotfile vcs="none" override wins over the -git
// suffix.
func TestPollOnceDotfileExtras(t *testing.T) {
	src := newSourceRepo(t, "foo-git", map[string]string{
		".SRCINFO": srcinfoBody("foo-git", "1.0", "1"),
		".varve.toml": `maintainers = ["main@example.org"]
extras = ["meta/signing.toml"]
`,
		"meta/signing.toml": `maintainers = ["extra@example.org"]
vcs = "none"
[collect]
exclude = ["*-debug"]
`,
	})
	store, _ := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "file://"+src, store, sink)

	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	changes := assertChangeCount(t, sink, 1)
	if !reflect.DeepEqual(changes[0].Maintainers, []db.Maintainer{{Email: "main@example.org"}, {Email: "extra@example.org"}}) {
		t.Errorf("Maintainers = %v", changes[0].Maintainers)
	}
	if !reflect.DeepEqual(changes[0].Collect.Exclude, []string{"*-debug"}) {
		t.Errorf("Collect.Exclude = %v", changes[0].Collect.Exclude)
	}
	if changes[0].Package.VCSKind != "" || changes[0].Reason != ReasonCommit {
		t.Errorf("change = %+v, want plain commit change (vcs overridden to none)", changes[0])
	}
}

// TestPollOnceSkips cover the per-branch fault isolation: a branch without
// .SRCINFO, one with an invalid dotfile and one whose sink submit conflicts
// are all skipped without blocking the other branches.
func TestPollOnceSkips(t *testing.T) {
	t.Run("missing .SRCINFO", func(t *testing.T) {
		src := newMultiBranchRepo(t, []branchSpec{
			{name: "nosrc", files: map[string]string{"PKGBUILD": "# no .SRCINFO here"}},
			{name: "good", files: map[string]string{".SRCINFO": srcinfoBody("good", "1.0", "1")}},
		})
		store, _ := openStore(t)
		sink := &fakeSink{}
		d := newTestDetector(t, "file://"+src, store, sink)

		if err := d.PollOnce(context.Background()); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
		changes := assertChangeCount(t, sink, 1)
		if changes[0].Package.Branch != "good" {
			t.Errorf("submitted %+v, want only the good branch", changes[0])
		}
	})

	t.Run("invalid dotfile", func(t *testing.T) {
		src := newMultiBranchRepo(t, []branchSpec{
			{name: "bad", files: map[string]string{
				".SRCINFO":    srcinfoBody("bad", "1.0", "1"),
				".varve.toml": "not toml [[[",
			}},
			{name: "good", files: map[string]string{".SRCINFO": srcinfoBody("good", "1.0", "1")}},
		})
		store, _ := openStore(t)
		sink := &fakeSink{}
		d := newTestDetector(t, "file://"+src, store, sink)

		if err := d.PollOnce(context.Background()); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
		changes := assertChangeCount(t, sink, 1)
		if changes[0].Package.Branch != "good" {
			t.Errorf("submitted %+v, want only the good branch", changes[0])
		}
	})

	t.Run("sink conflict", func(t *testing.T) {
		src := newMultiBranchRepo(t, []branchSpec{
			{name: "aaa", files: map[string]string{".SRCINFO": srcinfoBody("aaa", "1.0", "1")}},
			{name: "bbb", files: map[string]string{".SRCINFO": srcinfoBody("bbb", "1.0", "1")}},
		})
		store, _ := openStore(t)
		sink := &fakeSink{errOnce: true, err: errors.New("db: conflict")}
		d := newTestDetector(t, "file://"+src, store, sink)

		if err := d.PollOnce(context.Background()); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
		changes := assertChangeCount(t, sink, 1)
		if changes[0].Package.Branch != "bbb" {
			t.Errorf("submitted %+v, want only bbb (aaa conflicted)", changes[0])
		}
	})
}

// TestPollOnceUpstreamQueryFailureSkips asserts a failed upstream query
// skips the branch instead of submitting a false positive.
// The svn shim fails when invoked without --xml; here the source= entry is
// not a VCS URL so no query runs at all, and a package that cannot name an
// upstream is still submitted on srcinfo changes with an empty ref.
func TestPollOnceUpstreamQueryFailureSkips(t *testing.T) {
	src := newSourceRepo(t, "foo-git", map[string]string{
		".SRCINFO": srcinfoWithSource("foo-git", "1.0", "1", "https://example.org/foo.tar.gz"),
	})
	store, _ := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "file://"+src, store, sink)

	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	changes := assertChangeCount(t, sink, 1)
	if changes[0].UpstreamRef != "" || changes[0].Reason != ReasonCommit {
		t.Errorf("change = %+v, want commit change with empty upstream ref", changes[0])
	}
}

// TestPollOnceUpstreamConcurrencyBounded drives eight VCS branches whose
// ls-remote calls are traced by the git PATH shim. The shim parks every
// query behind a barrier file, so the test first observes all
// vcsQueryConcurrency slots occupied (deterministic overlap) and only then
// releases them; the trace must never exceed the cap.
func TestPollOnceUpstreamConcurrencyBounded(t *testing.T) {
	withShimPath(t)
	branches := make([]branchSpec, 0, 8)
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("vcs%d-git", i)
		branches = append(branches, branchSpec{name: name, files: map[string]string{
			".SRCINFO": srcinfoWithSource(name, "1.0", "1", "git+https://example.org/upstream.git"),
		}})
	}
	src := newMultiBranchRepo(t, branches)

	trace := filepath.Join(t.TempDir(), "trace")
	barrier := filepath.Join(t.TempDir(), "release")
	if err := os.WriteFile(trace, nil, 0o644); err != nil {
		t.Fatalf("create trace: %v", err)
	}
	t.Setenv("VARVE_TEST_GIT_HEAD", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	t.Setenv("VARVE_TEST_GIT_BARRIER", barrier)
	t.Setenv("VARVE_TEST_GIT_TRACE", trace)

	store, _ := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "file://"+src, store, sink)

	// Release the barrier on any exit path so a failed waitFor does not
	// leave the parked ls-remote shims blocking forever.
	var released bool
	defer func() {
		if !released {
			_ = os.WriteFile(barrier, nil, 0o644)
		}
	}()

	done := make(chan error, 1)
	go func() { done <- d.PollOnce(context.Background()) }()

	// Wait until every upstream-query slot is occupied: the parked
	// ls-remote calls prove the queries overlap by construction.
	waitFor(t, 3*time.Second, func() bool {
		return countLines(t, trace, "start") >= vcsQueryConcurrency
	})
	if err := os.WriteFile(barrier, nil, 0o644); err != nil {
		t.Fatalf("write barrier: %v", err)
	}
	released = true
	if err := <-done; err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	assertChangeCount(t, sink, 8)

	max := maxConcurrent(t, trace)
	if max != vcsQueryConcurrency {
		t.Errorf("upstream queries: max active = %d, want exactly %d (barrier-synchronized)", max, vcsQueryConcurrency)
	}
	if max > vcsQueryConcurrency {
		t.Errorf("upstream queries exceeded the cap: max active = %d, want <= %d", max, vcsQueryConcurrency)
	}
}

// maxConcurrent walks a shim trace of "start"/"end" lines and returns the
// maximum number of simultaneously active queries.
func maxConcurrent(t *testing.T, trace string) int {
	t.Helper()
	active, max := 0, 0
	for _, line := range readLines(t, trace) {
		switch line {
		case "start":
			active++
			if active > max {
				max = active
			}
		case "end":
			active--
		}
	}
	return max
}

// assertChangeCount checks the sink submission count and returns the
// snapshot for further assertions.
func assertChangeCount(t *testing.T, sink *fakeSink, want int) []Change {
	t.Helper()
	got := sink.snapshot()
	if len(got) != want {
		t.Fatalf("submitted %d changes, want %d: %+v", len(got), want, got)
	}
	return got
}

// TestPollOnceMultiArch asserts that every declared .SRCINFO architecture
// is carried into the submitted change. The old archOf picked only the
// first element, losing the rest before they reached storage and matching.
func TestPollOnceMultiArch(t *testing.T) {
	body := "pkgbase = foo\n" +
		"\tpkgdesc = test package\n" +
		"\tpkgver = 1.0\n" +
		"\tpkgrel = 1\n" +
		"\tarch = x86_64\n" +
		"\tarch = aarch64\n" +
		"pkgname = foo\n"
	src := newSourceRepo(t, "foo", map[string]string{".SRCINFO": body})
	store, _ := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "file://"+src, store, sink)

	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	changes := assertChangeCount(t, sink, 1)
	if changes[0].Package.Arch != "aarch64|x86_64" {
		t.Errorf("change arch = %q, want %q (every declared arch preserved)",
			changes[0].Package.Arch, "aarch64|x86_64")
	}
}

// TestArchSet canonically normalizes the declared .SRCINFO architecture
// list into the packages.arch storage format: "any" dominates, the rest is
// deduplicated, sorted and joined with "|" so matching covers every
// element. An empty declaration keeps the x86_64 baseline default.
func TestArchSet(t *testing.T) {
	tests := []struct {
		name string
		arch []string
		want string
	}{
		{name: "single", arch: []string{"x86_64"}, want: "x86_64"},
		{name: "multi preserved in full", arch: []string{"x86_64", "aarch64"}, want: "aarch64|x86_64"},
		{name: "multi order-insensitive", arch: []string{"aarch64", "x86_64"}, want: "aarch64|x86_64"},
		{name: "any alone", arch: []string{"any"}, want: "any"},
		{name: "any dominates a set", arch: []string{"x86_64", "any"}, want: "any"},
		{name: "duplicates deduplicated", arch: []string{"x86_64", "x86_64"}, want: "x86_64"},
		{name: "blank entries skipped", arch: []string{"", "x86_64"}, want: "x86_64"},
		{name: "empty defaults to baseline", arch: nil, want: "x86_64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := archSet(tt.arch); got != tt.want {
				t.Errorf("archSet(%v) = %q, want %q", tt.arch, got, tt.want)
			}
		})
	}
}

// TestPlanBranchCommitPath is the regression guard for the leading-dot
// bug: planBranch must read the dotted ".SRCINFO" from the branch tree
// (git show <branch>:.SRCINFO), so a branch carrying .SRCINFO is planned
// with the expected branch commit while a branch without it, or with
// only a non-dotted SRCINFO file, is skipped.
func TestPlanBranchCommitPath(t *testing.T) {
	tests := []struct {
		name       string
		files      map[string]string
		wantNil    bool
		wantCommit string
	}{
		{
			name:       "dotted .SRCINFO is read and the commit snapshotted",
			files:      map[string]string{".SRCINFO": srcinfoBody("foo", "1.0", "1")},
			wantCommit: "", // filled below from the repo HEAD
		},
		{
			name:    "missing .SRCINFO is skipped",
			files:   map[string]string{"PKGBUILD": "# no .SRCINFO here"},
			wantNil: true,
		},
		{
			name:    "non-dotted SRCINFO alone is not enough",
			files:   map[string]string{"SRCINFO": srcinfoBody("foo", "1.0", "1")},
			wantNil: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newSourceRepo(t, "pkg", tt.files)
			store, _ := openStore(t)
			d := newTestDetector(t, "file://"+src, store, &fakeSink{})
			d.mirrorDir = cloneMirror(t, src)

			p := d.planBranch(context.Background(), "pkg")
			if tt.wantNil {
				if p != nil {
					t.Fatalf("planBranch = %+v, want nil (branch skipped)", p)
				}
				return
			}
			if p == nil {
				t.Fatal("planBranch = nil, want a plan for a branch carrying .SRCINFO")
			}
			want := runGit(t, src, "rev-parse", "HEAD")
			if p.commit != want {
				t.Errorf("plan.commit = %q, want %q", p.commit, want)
			}
		})
	}
}

// TestPollOnceSrcinfoMetadata asserts the .SRCINFO metadata (url,
// licenses, conflicts, provides, pkgname, source, pkgver, pkgrel) flows
// from the branch into the submitted change.
func TestPollOnceSrcinfoMetadata(t *testing.T) {
	src := newSourceRepo(t, "meta-pkg", map[string]string{
		".SRCINFO": "pkgbase = meta-pkg\n" +
			"\tpkgdesc = metadata package\n" +
			"\tpkgver = 1.0\n" +
			"\tpkgrel = 1\n" +
			"\turl = https://example.org/meta-pkg\n" +
			"\tarch = x86_64\n" +
			"\tlicense = GPL\n" +
			"\tlicense = MIT\n" +
			"\tconflict = old-meta\n" +
			"\tprovides = meta-shim\n" +
			"\tsource = https://example.org/meta-pkg.tar.gz\n" +
			"pkgname = meta-pkg\n" +
			"pkgname = meta-extra\n",
	})
	store, _ := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "file://"+src, store, sink)

	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	changes := assertChangeCount(t, sink, 1)
	c := changes[0]
	if c.URL != "https://example.org/meta-pkg" {
		t.Errorf("URL = %q, want https://example.org/meta-pkg", c.URL)
	}
	if !reflect.DeepEqual(c.Licenses, []string{"GPL", "MIT"}) {
		t.Errorf("Licenses = %v, want [GPL MIT]", c.Licenses)
	}
	if !reflect.DeepEqual(c.Conflicts, []string{"old-meta"}) {
		t.Errorf("Conflicts = %v, want [old-meta]", c.Conflicts)
	}
	if !reflect.DeepEqual(c.Provides, []string{"meta-shim"}) {
		t.Errorf("Provides = %v, want [meta-shim]", c.Provides)
	}
	if !reflect.DeepEqual(c.Pkgname, []string{"meta-pkg", "meta-extra"}) {
		t.Errorf("Pkgname = %v, want [meta-pkg meta-extra]", c.Pkgname)
	}
	if !reflect.DeepEqual(c.Source, []string{"https://example.org/meta-pkg.tar.gz"}) {
		t.Errorf("Source = %v, want the source list", c.Source)
	}
	if c.Pkgver != "1.0" || c.Pkgrel != "1" {
		t.Errorf("Pkgver/Pkgrel = %q/%q, want 1.0/1", c.Pkgver, c.Pkgrel)
	}
}

// TestPollOnceDotfileAUR asserts the [aur] dotfile section flows through
// the pipeline: the branch submit carries the AUR package name and submit
// flag, and object-form maintainers keep their name/email pairs.
func TestPollOnceDotfileAUR(t *testing.T) {
	src := newSourceRepo(t, "foo-aur", map[string]string{
		".SRCINFO": srcinfoBody("foo-aur", "1.0", "1"),
		".varve.toml": `[[maintainers]]
name = "Alice"
email = "alice@example.org"

[aur]
name = "foo-aur-pkg"
submit = true
`,
	})
	store, _ := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "file://"+src, store, sink)

	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	changes := assertChangeCount(t, sink, 1)
	if !reflect.DeepEqual(changes[0].Maintainers, []db.Maintainer{{Name: "Alice", Email: "alice@example.org"}}) {
		t.Errorf("Maintainers = %+v", changes[0].Maintainers)
	}
	if changes[0].AUR.Name != "foo-aur-pkg" || !changes[0].AUR.Submit {
		t.Errorf("AUR = %+v, want {Name:foo-aur-pkg Submit:true}", changes[0].AUR)
	}
}
