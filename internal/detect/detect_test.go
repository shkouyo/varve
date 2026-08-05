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

package detect

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"git.0x0f.dev/varve/internal/config"
)

// TestMirrorDir covers the mirror name derivation table: the URL path
// part without ".git", with "/" replaced by "_" (DETAIL §3.3).
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
	if _, err := NewDetector(nil, store, sink); err == nil {
		t.Error("NewDetector(nil) succeeded, want error")
	}
	if _, err := NewDetector(&config.SourceConfig{}, store, sink); err == nil {
		t.Error("NewDetector(empty URL) succeeded, want error")
	}
	if _, err := NewDetector(&config.SourceConfig{URL: "git@host:"}, store, sink); err == nil {
		t.Error("NewDetector(unusable URL) succeeded, want error")
	}
}

// TestNewDetectorMirrorDir asserts the production mirror path layout
// "/data/source/<name>.git" (decision A7).
func TestNewDetectorMirrorDir(t *testing.T) {
	store, _ := openStore(t)
	d, err := NewDetector(&config.SourceConfig{URL: "git@git.example.org:pkgbuilds.git"}, store, &fakeSink{})
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}
	want := filepath.Join(sourceRoot, "pkgbuilds.git")
	if d.mirrorDir != want {
		t.Errorf("mirrorDir = %q, want %q", d.mirrorDir, want)
	}
}

// TestPollOncePlainPackage walks the full lifecycle of a plain package
// (DETAIL §3.7 #8): first enqueue, no change after a recorded successful
// build, enqueue on a .SRCINFO change, and the A16 natural re-queue while
// the build outcome is not recorded.
func TestPollOncePlainPackage(t *testing.T) {
	src := newSourceRepo(t, "foo", map[string]string{
		"SRCINFO": srcinfoBody("foo", "1.0", "1"),
	})
	store, dbPath := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "file://"+src, store, sink)

	// 1. First poll of a new branch: enqueue (srcinfo).
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #1: %v", err)
	}
	changes := assertChangeCount(t, sink, 1)
	want := Change{
		Package: Package{Pkgbase: "foo", Branch: "foo", VCSKind: "", Arch: "x86_64"},
		Reason:  ReasonSrcinfo,
	}
	if !reflect.DeepEqual(changes[0], want) {
		t.Errorf("change #1 = %+v, want %+v", changes[0], want)
	}

	// 2. Successful build recorded: no change on the next poll.
	seedPackageRow(t, dbPath, "foo", hashOf(srcinfoBody("foo", "1.0", "1")), "")
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #2: %v", err)
	}
	assertChangeCount(t, sink, 1)

	// 3. .SRCINFO change: enqueue (srcinfo).
	commitFiles(t, src, map[string]string{"SRCINFO": srcinfoBody("foo", "1.0", "2")}, "bump pkgrel")
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #3: %v", err)
	}
	changes = assertChangeCount(t, sink, 2)
	if changes[1].Reason != ReasonSrcinfo || changes[1].Package.Pkgbase != "foo" {
		t.Errorf("change #3 = %+v, want srcinfo for foo", changes[1])
	}

	// 4. A16: the failed build left the record stale, so the same diff is
	// detected again on the next round.
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #4: %v", err)
	}
	assertChangeCount(t, sink, 3)
}

// TestPollOnceVCSGitUpstream covers a -git package whose upstream HEAD is
// served by the testdata/bin/git PATH shim: first enqueue, no change after
// a recorded build, enqueue on an upstream commit change and the A16
// natural re-queue (DETAIL §3.7 #8).
func TestPollOnceVCSGitUpstream(t *testing.T) {
	withShimPath(t)
	src := newSourceRepo(t, "foo-git", map[string]string{
		"SRCINFO": srcinfoWithSource("foo-git", "1.0", "1", "git+https://example.org/upstream.git"),
	})
	store, dbPath := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "file://"+src, store, sink)

	const h1 = "1111111111111111111111111111111111111111"
	const h2 = "2222222222222222222222222222222222222222"
	t.Setenv("VARVE_TEST_GIT_HEAD", h1)

	// 1. First poll: both the hash and the upstream ref differ from the
	// (empty) records, so the reason is srcinfo+upstream.
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #1: %v", err)
	}
	changes := assertChangeCount(t, sink, 1)
	want := Change{
		Package:     Package{Pkgbase: "foo-git", Branch: "foo-git", VCSKind: "git", Arch: "x86_64"},
		UpstreamRef: h1,
		Reason:      ReasonBoth,
	}
	if !reflect.DeepEqual(changes[0], want) {
		t.Errorf("change #1 = %+v, want %+v", changes[0], want)
	}

	// 2. Successful build recorded: no change.
	seedPackageRow(t, dbPath, "foo-git", hashOf(srcinfoWithSource("foo-git", "1.0", "1", "git+https://example.org/upstream.git")), h1)
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

	// 4. A16: not recorded after the failed build -> re-queued.
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
		"SRCINFO": srcinfoWithSource("foo-svn", "1.0", "1", "svn+https://example.org/upstream"),
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
	seedPackageRow(t, dbPath, "foo-svn", hashOf(srcinfoWithSource("foo-svn", "1.0", "1", "svn+https://example.org/upstream")), "42")
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
		"SRCINFO": srcinfoBody("foo-git", "1.0", "1"),
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
	if !reflect.DeepEqual(changes[0].Maintainers, []string{"main@example.org", "extra@example.org"}) {
		t.Errorf("Maintainers = %v", changes[0].Maintainers)
	}
	if !reflect.DeepEqual(changes[0].Collect.Exclude, []string{"*-debug"}) {
		t.Errorf("Collect.Exclude = %v", changes[0].Collect.Exclude)
	}
	if changes[0].Package.VCSKind != "" || changes[0].Reason != ReasonSrcinfo {
		t.Errorf("change = %+v, want plain srcinfo change (vcs overridden to none)", changes[0])
	}
}

// TestPollOnceSkips cover the per-branch fault isolation: a branch without
// SRCINFO, one with an invalid dotfile and one whose sink submit conflicts
// are all skipped without blocking the other branches (DETAIL §3.5, §3.7
// #8).
func TestPollOnceSkips(t *testing.T) {
	t.Run("missing SRCINFO", func(t *testing.T) {
		src := newMultiBranchRepo(t, []branchSpec{
			{name: "nosrc", files: map[string]string{"PKGBUILD": "# no SRCINFO here"}},
			{name: "good", files: map[string]string{"SRCINFO": srcinfoBody("good", "1.0", "1")}},
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
				"SRCINFO":     srcinfoBody("bad", "1.0", "1"),
				".varve.toml": "not toml [[[",
			}},
			{name: "good", files: map[string]string{"SRCINFO": srcinfoBody("good", "1.0", "1")}},
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
			{name: "aaa", files: map[string]string{"SRCINFO": srcinfoBody("aaa", "1.0", "1")}},
			{name: "bbb", files: map[string]string{"SRCINFO": srcinfoBody("bbb", "1.0", "1")}},
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
// skips the branch instead of submitting a false positive (DETAIL §3.5).
// The svn shim fails when invoked without --xml; here the source= entry is
// not a VCS URL so no query runs at all, and a package that cannot name an
// upstream is still submitted on srcinfo changes with an empty ref.
func TestPollOnceUpstreamQueryFailureSkips(t *testing.T) {
	src := newSourceRepo(t, "foo-git", map[string]string{
		"SRCINFO": srcinfoWithSource("foo-git", "1.0", "1", "https://example.org/foo.tar.gz"),
	})
	store, _ := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "file://"+src, store, sink)

	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	changes := assertChangeCount(t, sink, 1)
	if changes[0].UpstreamRef != "" || changes[0].Reason != ReasonSrcinfo {
		t.Errorf("change = %+v, want srcinfo change with empty upstream ref", changes[0])
	}
}

// TestPollOnceUpstreamConcurrencyBounded drives eight VCS branches whose
// ls-remote calls are traced by the git PATH shim: the queries must
// overlap but never exceed vcsQueryConcurrency (DETAIL §3.7 #8).
func TestPollOnceUpstreamConcurrencyBounded(t *testing.T) {
	withShimPath(t)
	branches := make([]branchSpec, 0, 8)
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("vcs%d-git", i)
		branches = append(branches, branchSpec{name: name, files: map[string]string{
			"SRCINFO": srcinfoWithSource(name, "1.0", "1", "git+https://example.org/upstream.git"),
		}})
	}
	src := newMultiBranchRepo(t, branches)

	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("VARVE_TEST_GIT_HEAD", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	t.Setenv("VARVE_TEST_GIT_SLEEP", "0.2")
	t.Setenv("VARVE_TEST_GIT_TRACE", trace)

	store, _ := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "file://"+src, store, sink)

	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	assertChangeCount(t, sink, 8)

	max := maxConcurrent(t, trace)
	if max < 2 {
		t.Errorf("upstream queries did not overlap (max active = %d)", max)
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
