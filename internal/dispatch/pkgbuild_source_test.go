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

package dispatch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"git.0x0f.dev/varve/internal/detect"
	"git.0x0f.dev/varve/internal/detect/srcinfo"
)

// pkgbuildChange builds a detect.Change for a pkgbuild_source branch.
func pkgbuildChange(pkgbase, branch, extURL string) detect.Change {
	c := detectChange(pkgbase, branch)
	c.PkgbuildSource = &detect.PkgbuildSource{URL: extURL}
	c.PkgbuildRef = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	return c
}

// routeCloneExec wraps the fake git so "git clone" runs for real against a
// local file:// pkgbuild repository while every other git invocation stays
// on the fake helper.
func routeCloneExec(env *testEnv, base func(context.Context, string, ...string) *exec.Cmd) {
	env.o.execCommand = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		if name == "git" && len(arg) > 0 && arg[0] == "clone" {
			return exec.CommandContext(ctx, "git", arg...)
		}
		return base(ctx, name, arg...)
	}
}

// makeGitRepo initializes a local git repository (file:// protocol only)
// with one commit carrying the given files.
func makeGitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ext")
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-b", "master")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.org")
	git("add", ".")
	git("commit", "-m", "init")
	return dir
}

// TestEnqueuePkgbuildSource asserts the enqueue path of a pkgbuild_source
// change: the dispatch-time srcinfo hash comes from the external repository
// and the build row snapshots the dispatched external head while keeping
// the branch commit as build.Commit.
func TestEnqueuePkgbuildSource(t *testing.T) {
	ext := makeGitRepo(t, map[string]string{".SRCINFO": srcinfoBody})
	env := newTestEnv(t)
	state := &gitState{Commit: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"}
	routeCloneExec(env, fakeGitFor(t, env.log, state))

	c := pkgbuildChange("extpkg", "extpkg", "file://"+ext)
	if err := env.o.Enqueue(context.Background(), c, false); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	taskID := env.activeTaskFor(t, "extpkg")
	task, err := env.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	build, err := env.store.GetBuild(context.Background(), task.BuildID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if build.PkgbuildRef != "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" {
		t.Errorf("build pkgbuild_ref = %q, want the dispatched external head", build.PkgbuildRef)
	}
	if build.Commit != "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2" {
		t.Errorf("build commit = %q, want the branch commit", build.Commit)
	}
	extData, err := os.ReadFile(filepath.Join(ext, ".SRCINFO"))
	if err != nil {
		t.Fatalf("read external .SRCINFO: %v", err)
	}
	if build.SrcinfoHash != srcinfo.Hash(extData) {
		t.Errorf("srcinfo_hash = %q, want the external .SRCINFO hash %q", build.SrcinfoHash, srcinfo.Hash(extData))
	}
}

// TestIngestPkgbuildSourceRouting asserts the ingest path of a
// pkgbuild_source task: the reported checkout commit (the external head
// actually built) rides build.PkgbuildRef so the package record keeps the
// branch commit in last_commit and records the built external head in
// pkgbuild_ref.
func TestIngestPkgbuildSourceRouting(t *testing.T) {
	ext := makeGitRepo(t, map[string]string{".SRCINFO": srcinfoBody})
	env := newTestEnv(t)
	state := &gitState{
		Commit:  "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		Dotfile: "[pkgbuild_source]\nurl = \"file://" + ext + "\"\n",
	}
	routeCloneExec(env, fakeGitFor(t, env.log, state))

	c := pkgbuildChange("extpkg", "extpkg", "file://"+ext)
	if err := env.o.Enqueue(context.Background(), c, false); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	taskID := env.activeTaskFor(t, "extpkg")
	artifacts := testArtifacts("extpkg", "1.0.0-1")
	for _, a := range artifacts {
		env.stage(t, taskID, a.File)
	}
	worker := "w-extpkg"
	env.registerWorker(t, worker, "host", "host", 1)
	claimedID, token := env.claim(t, worker)
	if claimedID != taskID {
		t.Fatalf("claimed %s, want %s", claimedID, taskID)
	}
	const builtExternal = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	res := ResultReq{Status: "succeeded", Commit: builtExternal, Artifacts: artifacts}
	if err := env.o.ReportResult(context.Background(), claimedID, token, res); err != nil {
		t.Fatalf("ReportResult: %v", err)
	}

	pkg, err := env.store.GetPackageByBase(context.Background(), "extpkg")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if pkg.PkgbuildRef != builtExternal {
		t.Errorf("package pkgbuild_ref = %q, want the built external head", pkg.PkgbuildRef)
	}
	if pkg.LastCommit != "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2" {
		t.Errorf("last_commit = %q, want the branch commit preserved", pkg.LastCommit)
	}
	// The build row keeps its enqueue-time snapshot (the dispatched
	// external head and the branch commit); the built head advances the
	// package record only.
	task, err := env.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	build, err := env.store.GetBuild(context.Background(), task.BuildID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if build.PkgbuildRef != "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" {
		t.Errorf("build pkgbuild_ref = %q, want the dispatched external head snapshot", build.PkgbuildRef)
	}
	if build.Commit != "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2" {
		t.Errorf("build commit = %q, want the branch commit", build.Commit)
	}
}

// TestIngestPlainCommitRouting guards the plain branch path: the reported
// checkout commit still advances last_commit and pkgbuild_ref stays empty
// when the branch has no pkgbuild_source.
func TestIngestPlainCommitRouting(t *testing.T) {
	env := newTestEnv(t)
	state := &gitState{Commit: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"}
	env.o.execCommand = fakeGitFor(t, env.log, state)

	taskID := env.enqueue(t, "plainpkg", "plainpkg")
	artifacts := testArtifacts("plainpkg", "1.0.0-1")
	for _, a := range artifacts {
		env.stage(t, taskID, a.File)
	}
	worker := "w-plainpkg"
	env.registerWorker(t, worker, "host", "host", 1)
	claimedID, token := env.claim(t, worker)
	res := ResultReq{Status: "succeeded", Commit: "cccccccccccccccccccccccccccccccccccccccc", Artifacts: artifacts}
	if err := env.o.ReportResult(context.Background(), claimedID, token, res); err != nil {
		t.Fatalf("ReportResult: %v", err)
	}

	pkg, err := env.store.GetPackageByBase(context.Background(), "plainpkg")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if pkg.LastCommit != "cccccccccccccccccccccccccccccccccccccccc" {
		t.Errorf("last_commit = %q, want the reported checkout commit", pkg.LastCommit)
	}
	if pkg.PkgbuildRef != "" {
		t.Errorf("package pkgbuild_ref = %q, want empty for a plain branch", pkg.PkgbuildRef)
	}
}

// srcinfoBody is a minimal .SRCINFO body for the external repositories.
const srcinfoBody = "pkgbase = extpkg\n" +
	"\tpkgdesc = test package\n" +
	"\tpkgver = 1.0\n" +
	"\tpkgrel = 1\n" +
	"\tarch = x86_64\n" +
	"pkgname = extpkg\n"
