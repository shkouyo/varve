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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pkgbuildDotfile renders a .varve.toml pointing at an external repo.
func pkgbuildDotfile(url string) string {
	return "[pkgbuild_source]\nurl = \"" + url + "\"\n"
}

// TestPollOncePkgbuildSourceExternal walks the lifecycle of a branch whose
// dotfile sets pkgbuild_source: the branch carries no .SRCINFO at all, the
// metadata comes from the external repository and the change carries the
// external head alongside the branch commit.
func TestPollOncePkgbuildSourceExternal(t *testing.T) {
	ext := newSourceRepo(t, "master", map[string]string{
		".SRCINFO": srcinfoBody("extpkg", "1.0", "1"),
		"PKGBUILD": "# pkgbuild v1\n",
	})
	src := newSourceRepo(t, "extpkg", map[string]string{
		".varve.toml": pkgbuildDotfile(serveViaGitDaemon(t, ext)),
	})
	store, _ := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "file://"+src, store, sink)

	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	changes := assertChangeCount(t, sink, 1)
	c := changes[0]
	if c.Package.Pkgbase != "extpkg" || c.Package.Branch != "extpkg" {
		t.Errorf("change package = %+v, want extpkg", c.Package)
	}
	if c.Pkgver != "1.0" || c.Pkgrel != "1" {
		t.Errorf("metadata from external .SRCINFO: pkgver/pkgrel = %q/%q, want 1.0/1", c.Pkgver, c.Pkgrel)
	}
	head := runGit(t, ext, "rev-parse", "HEAD")
	if c.PkgbuildRef != head {
		t.Errorf("PkgbuildRef = %q, want the external head %q", c.PkgbuildRef, head)
	}
	if c.Reason != "commit+pkgbuild" {
		t.Errorf("reason = %q, want commit+pkgbuild on first enqueue", c.Reason)
	}
}

// TestPollOncePkgbuildSourceTriggers covers the trigger semantics: after a
// successful build is recorded, an unchanged branch and external repo stay
// quiet, an external head change enqueues with reason pkgbuild and a
// branch dotfile change enqueues with reason commit.
func TestPollOncePkgbuildSourceTriggers(t *testing.T) {
	ext := newSourceRepo(t, "master", map[string]string{
		".SRCINFO": srcinfoBody("extpkg", "1.0", "1"),
		"PKGBUILD": "# v1\n",
	})
	src := newSourceRepo(t, "extpkg", map[string]string{
		".varve.toml": pkgbuildDotfile(serveViaGitDaemon(t, ext)),
	})
	store, dbPath := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "file://"+src, store, sink)

	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #1: %v", err)
	}
	assertChangeCount(t, sink, 1)

	// Successful build recorded for both the branch tip and the external
	// head: the next round must stay quiet.
	branchTip := runGit(t, src, "rev-parse", "HEAD")
	extHead := runGit(t, ext, "rev-parse", "HEAD")
	seedPackageRowPkgbuild(t, dbPath, "extpkg", branchTip, "", extHead)
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #2: %v", err)
	}
	assertChangeCount(t, sink, 1)

	// The external repo advances: the external head change triggers.
	commitFiles(t, ext, map[string]string{"PKGBUILD": "# v2\n"}, "bump external")
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #3: %v", err)
	}
	changes := assertChangeCount(t, sink, 2)
	if changes[1].Reason != ReasonPkgbuild {
		t.Errorf("change #3 = %+v, want a pkgbuild reason", changes[1])
	}
	extHead2 := runGit(t, ext, "rev-parse", "HEAD")
	seedPackageRowPkgbuild(t, dbPath, "extpkg", branchTip, "", extHead2)

	// A dotfile edit moves the branch tip: the branch commit change
	// triggers even though the external head is unchanged.
	commitFiles(t, src, map[string]string{".varve.toml": pkgbuildDotfile(serveViaGitDaemon(t, ext)) + "\n# comment\n"}, "bump dotfile")
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce #4: %v", err)
	}
	changes = assertChangeCount(t, sink, 3)
	if changes[2].Reason != ReasonCommit {
		t.Errorf("change #4 = %+v, want a commit reason", changes[2])
	}
}

// TestPollOncePkgbuildSourceDirectory covers the optional directory
// subpath: the external .SRCINFO is read below the directory prefix.
func TestPollOncePkgbuildSourceDirectory(t *testing.T) {
	ext := newSourceRepo(t, "master", map[string]string{
		"pkgs/extpkg/.SRCINFO": srcinfoBody("extpkg", "2.0", "1"),
		"pkgs/extpkg/PKGBUILD": "# nested\n",
	})
	src := newSourceRepo(t, "extpkg", map[string]string{
		".varve.toml": "[pkgbuild_source]\nurl = \"" + serveViaGitDaemon(t, ext) + "\"\ndirectory = \"pkgs/extpkg\"\n",
	})
	store, _ := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "file://"+src, store, sink)

	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	changes := assertChangeCount(t, sink, 1)
	if changes[0].Pkgver != "2.0" || changes[0].Package.Pkgbase != "extpkg" {
		t.Errorf("change = %+v, want extpkg 2.0 from the directory .SRCINFO", changes[0])
	}
}

// TestPollOncePkgbuildSourceSkips covers the external fault isolation: a
// branch whose external repository has no .SRCINFO, or whose branch does
// not exist, is skipped without blocking a plain branch next to it.
func TestPollOncePkgbuildSourceSkips(t *testing.T) {
	t.Run("missing external .SRCINFO", func(t *testing.T) {
		ext := newSourceRepo(t, "master", map[string]string{"PKGBUILD": "# no srcinfo\n"})
		src := newMultiBranchRepo(t, []branchSpec{
			{name: "extpkg", files: map[string]string{
				".varve.toml": pkgbuildDotfile(serveViaGitDaemon(t, ext)),
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

	t.Run("external branch missing", func(t *testing.T) {
		ext := newSourceRepo(t, "master", map[string]string{
			".SRCINFO": srcinfoBody("extpkg", "1.0", "1"),
		})
		src := newSourceRepo(t, "extpkg", map[string]string{
			".varve.toml": "[pkgbuild_source]\nurl = \"" + serveViaGitDaemon(t, ext) + "\"\nbranch = \"dev\"\n",
		})
		store, _ := openStore(t)
		sink := &fakeSink{}
		d := newTestDetector(t, "file://"+src, store, sink)

		if err := d.PollOnce(context.Background()); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
		assertChangeCount(t, sink, 0)
	})
}

// TestPollOncePkgbuildSourceVCSUpstream asserts the upstream detection
// reads the external .SRCINFO's source= entries: a -git pkgbase whose
// external .SRCINFO declares a git source resolves its upstream ref even
// though the branch tree carries no .SRCINFO at all. The external head and
// the upstream ref both come from the git PATH shim's canned hash.
func TestPollOncePkgbuildSourceVCSUpstream(t *testing.T) {
	withShimPath(t)
	ext := newSourceRepo(t, "master", map[string]string{
		".SRCINFO": srcinfoWithSource("extpkg-git", "1.0", "1", "git+https://example.org/upstream.git"),
	})
	src := newSourceRepo(t, "extpkg-git", map[string]string{
		".varve.toml": pkgbuildDotfile(serveViaGitDaemon(t, ext)),
	})
	t.Setenv("VARVE_TEST_GIT_HEAD", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	store, _ := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "file://"+src, store, sink)

	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	changes := assertChangeCount(t, sink, 1)
	c := changes[0]
	if c.Package.VCSKind != "git" {
		t.Errorf("VCSKind = %q, want git from the external .SRCINFO", c.Package.VCSKind)
	}
	if c.UpstreamRef != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("UpstreamRef = %q, want the shim head", c.UpstreamRef)
	}
	if c.PkgbuildRef != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("PkgbuildRef = %q, want the shim head", c.PkgbuildRef)
	}
}

// TestPkgbuildRel guards the directory traversal rejection.
func TestPkgbuildRel(t *testing.T) {
	good := []struct{ dir, name string }{
		{"", ".SRCINFO"},
		{"pkgs/foo", "PKGBUILD"},
		{"./pkgs/foo", ".SRCINFO"},
	}
	for _, g := range good {
		rel, err := pkgbuildRel(g.dir, g.name)
		if err != nil {
			t.Errorf("pkgbuildRel(%q, %q) error = %v, want a clean path", g.dir, g.name, err)
			continue
		}
		if rel == "" || rel[0] == '/' {
			t.Errorf("pkgbuildRel(%q, %q) = %q, want a checkout-relative path", g.dir, g.name, rel)
		}
	}
	for _, bad := range []string{"..", "../x", "/etc", "a/../../b"} {
		if _, err := pkgbuildRel(bad, ".SRCINFO"); err == nil {
			t.Errorf("pkgbuildRel(%q) succeeded, want a traversal error", bad)
		}
	}
}

// TestPkgbuildURLValidation asserts the pkgbuild_source url whitelist is
// enforced before any external command runs: option-like, scheme-less
// and file URLs are rejected by both PkgbuildHead and PkgbuildFile.
func TestPkgbuildURLValidation(t *testing.T) {
	var execs int
	execFn := func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		execs++
		return exec.CommandContext(ctx, "true")
	}
	for _, url := range []string{
		"-evil",
		"-u@h:p",
		"file:///srv/repo.git",
		"/srv/git/repo.git",
		"a b",
		"javascript:alert(1)",
	} {
		if _, err := PkgbuildHead(context.Background(), execFn, "", PkgbuildSource{URL: url, Branch: "master"}); err == nil {
			t.Errorf("PkgbuildHead(%q): want error", url)
		}
		if _, err := PkgbuildFile(context.Background(), execFn, "", PkgbuildSource{URL: url, Branch: "master"}, ".SRCINFO"); err == nil {
			t.Errorf("PkgbuildFile(%q): want error", url)
		}
	}
	if execs != 0 {
		t.Errorf("ran %d external commands despite invalid urls", execs)
	}
}

// TestFetchKeyEnvQuoting asserts the fetch_key path is shell-escaped
// with single quotes in both GIT_SSH_COMMAND and SVN_SSH, so a path
// containing quotes, $ or backticks cannot inject into the sh-invoked
// command line, and that empty or missing keys yield no environment.
func TestFetchKeyEnvQuoting(t *testing.T) {
	key := filepath.Join(t.TempDir(), "key'`$(x)`.pem")
	writeFile(t, key, "k")
	env := fetchKeyEnv(key)
	joined := strings.Join(env, "\n")
	want := "ssh -i '" + strings.ReplaceAll(key, "'", `'\''`) + "' -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"
	if !strings.Contains(joined, "GIT_SSH_COMMAND="+want) {
		t.Errorf("GIT_SSH_COMMAND not single-quote escaped:\n%v\nwant fragment %q", env, "GIT_SSH_COMMAND="+want)
	}
	if !strings.Contains(joined, "SVN_SSH="+want) {
		t.Errorf("SVN_SSH missing or not escaped: %v", env)
	}
	if env2 := fetchKeyEnv(""); env2 != nil {
		t.Errorf("fetchKeyEnv(\"\") = %v, want nil", env2)
	}
	if env3 := fetchKeyEnv(filepath.Join(t.TempDir(), "missing")); env3 != nil {
		t.Errorf("fetchKeyEnv(missing) = %v, want nil", env3)
	}
}
