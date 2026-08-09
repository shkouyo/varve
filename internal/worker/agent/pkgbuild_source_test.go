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
	exec := newFakeExec()
	exec.scripts["git clone --depth 1 --branch master file:///ext/repo.git "+taskDir] = writeScript(t,
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
	exec := newFakeExec()
	exec.scripts["git clone --depth 1 --branch master file:///ext/repo.git "+taskDir] = writeScript(t,
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
	exec := newFakeExec()
	exec.scripts["git clone --depth 1 --branch master file:///ext/repo.git "+taskDir] = writeScript(t,
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
