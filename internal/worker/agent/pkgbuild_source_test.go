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

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/api"
)

// taskForPkgbuild decodes a TaskDetail carrying a pkgbuild_source block
// (the wire JSON form is the only way the agent package can build one,
// since api re-exports the dispatch type without a named alias).
func taskForPkgbuild(id, url, branch, directory string) *api.TaskDetail {
	var task api.TaskDetail
	body := `{
		"id": "` + id + `",
		"package": {"pkgbase": "foo", "branch": "main", "arch": "x86_64"},
		"source": {"mode": "clone", "url": "https://example.invalid/branch.git", "branch": "main"},
		"pkgbuild_source": {"url": "` + url + `", "branch": "` + branch + `", "directory": "` + directory + `"},
		"hooks": {},
		"collect": {},
		"signing": {"required": false},
		"build": {}
	}`
	if err := json.Unmarshal([]byte(body), &task); err != nil {
		panic(err)
	}
	return &task
}

// TestTaskPkgbuildSourcePrepare asserts a pkgbuild_source task clones the
// external repository (never the branch) and builds from the optional
// directory subpath, which is moved to the checkout root so the rest of
// the flow works unchanged.
func TestTaskPkgbuildSourcePrepare(t *testing.T) {
	extURL := "file:///ext/repo.git"
	task := taskForPkgbuild("t-1", extURL, "master", "pkgs/foo")

	f := &fakeClient{taskDetail: task}
	r := runOneShotRunner(t, f)
	taskDir := r.workDir + "/t-1"
	extDir := taskDir + ".ext"
	exec := newFakeExec()
	exec.scripts["git clone --depth 1 --branch master file:///ext/repo.git "+extDir] = writeScript(t,
		"mkdir -p pkgs/foo\n"+
			"cat > pkgs/foo/.SRCINFO <<'EOF'\n"+testSrcinfo+"EOF\n"+
			"cat > pkgs/foo/PKGBUILD <<'EOF'\n# external pkgbuild\nEOF\n")
	exec.scripts["makepkg -s --noconfirm"] = writeScript(t, "touch foo-1.0-1-x86_64.pkg.tar.zst")
	r.execCommand = exec.command

	r.executeTask(context.Background(), task, "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusSucceeded {
		if res.Error != nil {
			t.Logf("ERROR: %+v", *res.Error)
		}
		t.Fatalf("result = %+v, want succeeded", res)
	}
	gitArgs := exec.callArgs("git")
	if len(gitArgs) == 0 || !contains(gitArgs[0], "file:///ext/repo.git") {
		t.Errorf("git args = %v, want the external clone of file:///ext/repo.git", gitArgs)
	}
	// The directory contents are moved to the checkout root.
	if _, err := os.Stat(filepath.Join(taskDir, ".SRCINFO")); err != nil {
		t.Errorf("external .SRCINFO not at the checkout root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(taskDir, "PKGBUILD")); err != nil {
		t.Errorf("external PKGBUILD not at the checkout root: %v", err)
	}
	// The clone sibling is discarded: no unrelated external entry (the
	// checkout root would otherwise also hold pkgs/foo and .git).
	if _, err := os.Stat(extDir); !os.IsNotExist(err) {
		t.Errorf("pkgbuild source clone dir %s still present", extDir)
	}
	if len(f.uploads) != 2 || f.uploads[1].name != ".SRCINFO" {
		t.Errorf("uploads = %+v, want package + .SRCINFO", f.uploads)
	}
}

// TestTaskPkgbuildSourceDefaultBranch asserts the master default branch is
// used when the task omits the branch key.
func TestTaskPkgbuildSourceDefaultBranch(t *testing.T) {
	task := taskForPkgbuild("t-1", "file:///ext/repo.git", "", "")

	f := &fakeClient{taskDetail: task}
	r := runOneShotRunner(t, f)
	taskDir := r.workDir + "/t-1"
	extDir := taskDir + ".ext"
	exec := newFakeExec()
	exec.scripts["git clone --depth 1 --branch master file:///ext/repo.git "+extDir] = writeScript(t,
		"cat > .SRCINFO <<'EOF'\n"+testSrcinfo+"EOF\n")
	exec.scripts["makepkg -s --noconfirm"] = writeScript(t, "touch foo-1.0-1-x86_64.pkg.tar.zst")
	r.execCommand = exec.command

	r.executeTask(context.Background(), task, "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusSucceeded {
		if res.Error != nil {
			t.Logf("ERROR: %+v", *res.Error)
		}
		t.Fatalf("result = %+v, want succeeded", res)
	}
	gitArgs := exec.callArgs("git")
	if len(gitArgs) == 0 || !strings.Contains(strings.Join(gitArgs[0], " "), "--branch master") {
		t.Errorf("git args = %v, want the master branch default", gitArgs)
	}
}

// TestTaskPkgbuildSourceMissingDirectory asserts a directory subpath that
// does not exist in the external checkout fails the task at the prepare
// stage with a clear message.
func TestTaskPkgbuildSourceMissingDirectory(t *testing.T) {
	task := taskForPkgbuild("t-1", "file:///ext/repo.git", "master", "pkgs/foo")

	f := &fakeClient{taskDetail: task}
	r := runOneShotRunner(t, f)
	taskDir := r.workDir + "/t-1"
	extDir := taskDir + ".ext"
	exec := newFakeExec()
	exec.scripts["git clone --depth 1 --branch master file:///ext/repo.git "+extDir] = writeScript(t,
		"cat > .SRCINFO <<'EOF'\n"+testSrcinfo+"EOF\n") // no pkgs/foo
	r.execCommand = exec.command

	r.executeTask(context.Background(), task, "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusFailed {
		t.Fatalf("result = %+v, want failed", res)
	}
	if res.Error == nil || res.Error.Stage != stagePrepare || !strings.Contains(res.Error.Summary, "no directory") {
		t.Errorf("error = %+v, want prepare failure naming the missing directory", res.Error)
	}
}

// TestTaskPkgbuildSourceRootCollision is the regression test for the
// monorepo layout that broke prepare in production: the external root and
// the pkgbuild directory both carry a LICENSE. The build root must receive
// only the promoted directory contents; the root LICENSE and every other
// unrelated entry stay in the discarded clone.
func TestTaskPkgbuildSourceRootCollision(t *testing.T) {
	task := taskForPkgbuild("t-1", "file:///ext/repo.git", "master", "pi")

	f := &fakeClient{taskDetail: task}
	r := runOneShotRunner(t, f)
	taskDir := r.workDir + "/t-1"
	extDir := taskDir + ".ext"
	exec := newFakeExec()
	exec.scripts["git clone --depth 1 --branch master file:///ext/repo.git "+extDir] = writeScript(t,
		"cat > LICENSE <<'EOF'\nroot license\nEOF\n"+
			"cat > README.md <<'EOF'\nroot readme\nEOF\n"+
			"mkdir -p pi\n"+
			"cat > pi/LICENSE <<'EOF'\npi license\nEOF\n"+
			"cat > pi/.SRCINFO <<'EOF'\n"+testSrcinfo+"EOF\n"+
			"cat > pi/PKGBUILD <<'EOF'\n# pi pkgbuild\nEOF\n")
	exec.scripts["makepkg -s --noconfirm"] = writeScript(t, "touch foo-1.0-1-x86_64.pkg.tar.zst")
	r.execCommand = exec.command

	r.executeTask(context.Background(), task, "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusSucceeded {
		if res.Error != nil {
			t.Logf("ERROR: %+v", *res.Error)
		}
		t.Fatalf("result = %+v, want succeeded", res)
	}
	if data, err := os.ReadFile(filepath.Join(taskDir, "LICENSE")); err != nil || string(data) != "pi license\n" {
		t.Errorf("build root LICENSE = %q, %v; want the pkgbuild copy", data, err)
	}
	for _, name := range []string{"README.md", ".git", "pi"} {
		if _, err := os.Stat(filepath.Join(taskDir, name)); !os.IsNotExist(err) {
			t.Errorf("unrelated external entry %q leaked into the build root", name)
		}
	}
	if _, err := os.Stat(extDir); !os.IsNotExist(err) {
		t.Errorf("pkgbuild source clone dir %s still present", extDir)
	}
}

// TestTaskPkgbuildSourceGeneratesSrcinfo asserts a pkgbuild_source task
// whose external directory carries a PKGBUILD but no .SRCINFO renders the
// artifact via makepkg and uploads it (the branch tree itself never has a
// .SRCINFO, and neither must the external checkout).
func TestTaskPkgbuildSourceGeneratesSrcinfo(t *testing.T) {
	task := taskForPkgbuild("t-1", "file:///ext/repo.git", "master", "pi")

	f := &fakeClient{taskDetail: task}
	r := runOneShotRunner(t, f)
	taskDir := r.workDir + "/t-1"
	extDir := taskDir + ".ext"
	exec := newFakeExec()
	exec.scripts["git clone --depth 1 --branch master file:///ext/repo.git "+extDir] = writeScript(t,
		"mkdir -p pi\n"+
			"cat > pi/PKGBUILD <<'EOF'\n# pkgbuild only, no .SRCINFO\nEOF\n")
	exec.scripts["makepkg --printsrcinfo"] = writeScript(t, "cat <<'EOF'\n"+testSrcinfo+"EOF\n")
	exec.scripts["makepkg -s --noconfirm"] = writeScript(t, "touch foo-1.0-1-x86_64.pkg.tar.zst")
	r.execCommand = exec.command

	r.executeTask(context.Background(), task, "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusSucceeded {
		if res.Error != nil {
			t.Logf("ERROR: %+v", *res.Error)
		}
		t.Fatalf("result = %+v, want succeeded", res)
	}
	if _, err := os.Stat(filepath.Join(taskDir, ".SRCINFO")); err != nil {
		t.Errorf("generated .SRCINFO not at the checkout root: %v", err)
	}
	if len(f.uploads) != 2 || f.uploads[1].name != ".SRCINFO" {
		t.Errorf("uploads = %+v, want package + generated .SRCINFO", f.uploads)
	}
}

// TestTaskPkgbuildSourceMissingPkgbuild asserts an external directory with
// neither a PKGBUILD nor a .SRCINFO fails at the prepare stage with a
// readable summary instead of silently building nothing.
func TestTaskPkgbuildSourceMissingPkgbuild(t *testing.T) {
	task := taskForPkgbuild("t-1", "file:///ext/repo.git", "master", "pi")

	f := &fakeClient{taskDetail: task}
	r := runOneShotRunner(t, f)
	taskDir := r.workDir + "/t-1"
	extDir := taskDir + ".ext"
	exec := newFakeExec()
	exec.scripts["git clone --depth 1 --branch master file:///ext/repo.git "+extDir] = writeScript(t,
		"mkdir -p pi\n") // empty directory: no PKGBUILD, no .SRCINFO
	exec.scripts["makepkg --printsrcinfo"] = writeScript(t,
		"echo '==> ERROR: PKGBUILD not found in the directory' 1>&2\nexit 1\n")
	r.execCommand = exec.command

	r.executeTask(context.Background(), task, "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusFailed {
		t.Fatalf("result = %+v, want failed", res)
	}
	if res.Error == nil || res.Error.Stage != stagePrepare || !strings.Contains(res.Error.Summary, "makepkg --printsrcinfo") {
		t.Errorf("error = %+v, want prepare failure naming the .SRCINFO render", res.Error)
	}
}

// TestTaskPkgbuildSourceRealClone drives the full pkgbuild_source flow
// against a real local clone of a monorepo-shaped external repository (a
// root LICENSE colliding with the pkgbuild directory LICENSE): the task
// succeeds, reports the external head and keeps the build root clean.
func TestTaskPkgbuildSourceRealClone(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "master")
	runGit(t, dir, "config", "user.email", "test@example.invalid")
	runGit(t, dir, "config", "user.name", "Test")
	if err := os.MkdirAll(dir+"/pi", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	files := map[string]string{
		"LICENSE":     "root license\n",
		"README.md":   "root readme\n",
		"pi/LICENSE":  "pi license\n",
		"pi/.SRCINFO": testSrcinfo,
		"pi/PKGBUILD": "# pi pkgbuild\n",
	}
	for name, body := range files {
		if err := os.WriteFile(dir+"/"+name, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "initial")
	wantCommit := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

	task := taskForPkgbuild("t-1", "file://"+dir, "master", "pi")

	f := &fakeClient{taskDetail: task}
	r := runOneShotRunner(t, f)
	exec := newFakeExec()
	exec.realGit = true
	exec.scripts["makepkg -s --noconfirm"] = writeScript(t, "touch foo-1.0-1-x86_64.pkg.tar.zst")
	r.execCommand = exec.command

	r.executeTask(context.Background(), task, "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusSucceeded {
		if res.Error != nil {
			t.Logf("ERROR: %+v", *res.Error)
		}
		t.Fatalf("result = %+v, want succeeded", res)
	}
	if res.Commit != wantCommit {
		t.Errorf("commit = %q, want the external head %q", res.Commit, wantCommit)
	}
	taskDir := r.workDir + "/t-1"
	if data, err := os.ReadFile(filepath.Join(taskDir, "LICENSE")); err != nil || string(data) != "pi license\n" {
		t.Errorf("build root LICENSE = %q, %v; want the pkgbuild copy", data, err)
	}
	if _, err := os.Stat(filepath.Join(taskDir, "README.md")); !os.IsNotExist(err) {
		t.Errorf("root README.md leaked into the build root")
	}
}

// TestMoveTreeUpCollision asserts the promotion helper keeps its collision
// guard: the flow never calls it with a non-empty root, so a hit here is
// a programming error worth surfacing loudly.
func TestMoveTreeUpCollision(t *testing.T) {
	root := t.TempDir()
	sub := root + "/pkg"
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(root+"/LICENSE", []byte("root"), 0o644); err != nil {
		t.Fatalf("write root LICENSE: %v", err)
	}
	if err := os.WriteFile(sub+"/LICENSE", []byte("pkg"), 0o644); err != nil {
		t.Fatalf("write pkg LICENSE: %v", err)
	}
	err := moveTreeUp(sub, root)
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("moveTreeUp error = %v, want a collision error", err)
	}
}
