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
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/api"
)

const testSrcinfo = `pkgbase = foo
pkgver = 1.0
pkgrel = 1
arch = x86_64
pkgname = foo
`

// TestTaskNineStepSequence drives a full one-shot task with signing and
// asserts the controller call sequence and the log progress samples.
func TestTaskNineStepSequence(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1")}
	f.keyMaterial = &api.KeyMaterial{ArmoredPrivateKey: "-----BEGIN PGP PRIVATE KEY BLOCK-----\nFAKE\n", Passphrase: "secret"}
	r := runOneShotRunner(t, f)

	taskDir := r.workDir + "/t-1"
	signScript := writeScript(t, "for last in \"$@\"; do :; done\ntouch \"$last.sig\"")
	mkpkg := writeScript(t, "echo 'building'\nsleep 0.05\ntouch foo-1.0-1-x86_64.pkg.tar.zst")
	exec := newFakeExec()
	exec.scripts["git clone --depth 1 --branch main https://example.invalid/repo.git "+taskDir] =
		writeScript(t, "cat > .SRCINFO <<'EOF'\n"+testSrcinfo+"EOF")
	exec.scripts["makepkg -s --noconfirm"] = mkpkg
	exec.scripts["gpg --batch --pinentry-mode loopback --passphrase secret --detach-sign "+filepath.Join(taskDir, "foo-1.0-1-x86_64.pkg.tar.zst")] = signScript
	r.execCommand = exec.command
	r.logInterval = 10 * time.Millisecond
	r.logThreshold = 1 << 20

	task := taskFor("t-1")
	task.Signing.Required = true
	f.taskDetail = task
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	res := f.lastResult()
	if res == nil || res.Status != statusSucceeded {
		if res.Error != nil {
			t.Logf("ERROR: %+v", *res.Error)
		}
		t.Fatalf("result = %+v, want succeeded", res)
	}

	// Ordering: GetTask → AppendLog → GetSigningKey → UploadFile×N →
	// ReportResult, with the uploads in package/signature/srcinfo order.
	order := []string{"GetTask", "AppendLog", "GetSigningKey", "UploadFile", "ReportResult"}
	prev := -1
	for _, want := range order {
		idx := f.callIndex(want)
		if idx < 0 {
			t.Fatalf("missing call %s (calls=%v)", want, f.calls)
		}
		if idx < prev {
			t.Errorf("call %s (index %d) out of order after index %d", want, idx, prev)
		}
		prev = idx
	}
	if f.callCount("AppendLog") == 0 {
		t.Error("no log segments were appended")
	}
	gotUploads := []string{}
	for _, u := range f.uploads {
		gotUploads = append(gotUploads, u.name)
	}
	wantUploads := []string{"foo-1.0-1-x86_64.pkg.tar.zst", "foo-1.0-1-x86_64.pkg.tar.zst.sig", ".SRCINFO"}
	if strings.Join(gotUploads, ",") != strings.Join(wantUploads, ",") {
		t.Errorf("upload order = %v, want %v", gotUploads, wantUploads)
	}

	// One-shot segments carry a resource sample in progress.
	sawProgress := false
	for _, seg := range f.segments {
		if seg.Progress != nil && seg.Progress.TaskID == "t-1" {
			sawProgress = true
		}
	}
	if !sawProgress {
		t.Error("no log segment carried a progress sample")
	}

	// gpg detach-sign used the loopback pinentry mode.
	gpgArgs := exec.callArgs("gpg")
	if len(gpgArgs) < 2 {
		t.Fatalf("gpg calls = %d, want import + detach-sign", len(gpgArgs))
	}
	if strings.Join(gpgArgs[0], " ") != "--batch --import "+filepath.Join(taskDir, ".gnupg", "private.asc") {
		t.Errorf("gpg import args = %v", gpgArgs[0])
	}
	if !contains(gpgArgs[1], "--pinentry-mode") || !contains(gpgArgs[1], "loopback") {
		t.Errorf("gpg detach-sign args missing loopback pinentry: %v", gpgArgs[1])
	}

	// The temporary GNUPGHOME is removed at task end.
	if _, err := os.Stat(filepath.Join(taskDir, ".gnupg")); !os.IsNotExist(err) {
		t.Errorf("temporary GNUPGHOME still present: %v", err)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestTaskPreBuildHookFailureAborts asserts a failing pre_build hook fails
// the task with stage hook:pre_build and runs on_failure.
func TestTaskPreBuildHookFailureAborts(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1")}
	r := runOneShotRunner(t, f)
	exec := flowExec(t, r.workDir, "t-1", testSrcinfo, []string{"foo-1.0-1-x86_64.pkg.tar.zst"}, map[string]string{
		"sh -c false": writeScript(t, "exit 3"),
	})
	r.execCommand = exec.command

	task := taskFor("t-1")
	task.Hooks.PreBuild = []string{"false"}
	task.Hooks.OnFailure = []string{"onfail"}
	r.executeTask(context.Background(), task, "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusFailed {
		t.Fatalf("result = %+v, want failed", res)
	}
	if res.Error == nil || res.Error.Stage != "hook:pre_build" {
		t.Errorf("error = %+v, want stage hook:pre_build", res.Error)
	}
	if !strings.Contains(res.Error.Summary, "false") {
		t.Errorf("summary = %q, want the failing hook name", res.Error.Summary)
	}
	assertHookCalled(t, exec, "onfail")
	assertHookNotCalled(t, exec, "onsuccess")
	if f.callCount("UploadFile") != 0 {
		t.Errorf("upload ran after a pre_build failure")
	}
}

// TestTaskPostBuildHookFailureWarns asserts a failing post_build hook is
// warned about but does not abort the build.
func TestTaskPostBuildHookFailureWarns(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1")}
	r := runOneShotRunner(t, f)
	exec := flowExec(t, r.workDir, "t-1", testSrcinfo, []string{"foo-1.0-1-x86_64.pkg.tar.zst"}, map[string]string{
		"sh -c postfail": writeScript(t, "exit 1"),
	})
	r.execCommand = exec.command

	task := taskFor("t-1")
	task.Hooks.PostBuild = []string{"postfail"}
	r.executeTask(context.Background(), task, "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusSucceeded {
		t.Fatalf("result = %+v, want succeeded despite the post_build hook", res)
	}
	assertHookCalled(t, exec, "postfail")
}

// TestTaskOnSuccessHookRuns asserts the on_success hooks run after a
// successful report.
func TestTaskOnSuccessHookRuns(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1")}
	r := runOneShotRunner(t, f)
	exec := flowExec(t, r.workDir, "t-1", testSrcinfo, []string{"foo-1.0-1-x86_64.pkg.tar.zst"}, nil)
	r.execCommand = exec.command

	task := taskFor("t-1")
	task.Hooks.OnSuccess = []string{"onsuccess"}
	r.executeTask(context.Background(), task, "tok")

	if res := f.lastResult(); res == nil || res.Status != statusSucceeded {
		if res.Error != nil {
			t.Logf("ERROR: %+v", *res.Error)
		}
		t.Fatalf("result = %+v, want succeeded", res)
	}
	assertHookCalled(t, exec, "onsuccess")
}

// TestTaskMakepkgFailureIncludesTail asserts a non-zero makepkg exit fails
// the task with stage=makepkg and a summary containing the log tail.
func TestTaskMakepkgFailureIncludesTail(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1")}
	r := runOneShotRunner(t, f)
	exec := flowExec(t, r.workDir, "t-1", testSrcinfo, nil, map[string]string{
		"makepkg -s --noconfirm": writeScript(t, "echo '== boom =='\nexit 2"),
	})
	r.execCommand = exec.command

	task := taskFor("t-1")
	r.executeTask(context.Background(), task, "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusFailed {
		t.Fatalf("result = %+v, want failed", res)
	}
	if res.Error == nil || res.Error.Stage != "makepkg" {
		t.Errorf("error = %+v, want stage makepkg", res.Error)
	}
	if !strings.Contains(res.Error.Summary, "== boom ==") {
		t.Errorf("summary = %q, want the log tail", res.Error.Summary)
	}
}

// TestTaskCollectEmptyFails asserts a build with no collectable artifacts
// fails with stage=collect (no empty manifests).
func TestTaskCollectEmptyFails(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1")}
	r := runOneShotRunner(t, f)
	exec := flowExec(t, r.workDir, "t-1", testSrcinfo, nil, nil)
	r.execCommand = exec.command

	r.executeTask(context.Background(), taskFor("t-1"), "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusFailed {
		t.Fatalf("result = %+v, want failed", res)
	}
	if res.Error == nil || res.Error.Stage != "collect" {
		t.Errorf("error = %+v, want stage collect", res.Error)
	}
	if f.callCount("UploadFile") != 0 {
		t.Errorf("upload ran despite an empty collect")
	}
}

// TestTaskLargeOutputDelivered drives a full task whose makepkg emits over
// 2 MiB of stdout against a client that mirrors the production controller:
// a 1 MiB per-segment cap and a real append round-trip latency. The whole
// output must arrive in bounded segments and reassemble losslessly — an
// unbounded single batch (the buffer can grow far past the threshold while
// the flush loop is busy) would be rejected by the cap and stall the log
// stream mid-build.
func TestTaskLargeOutputDelivered(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1")}
	f.logMaxSegment = 1 << 20 // mirrors api.maxLogSegmentLen
	f.logAppendDelay = 2 * time.Millisecond
	r := runOneShotRunner(t, f)
	taskDir := r.workDir + "/t-1"

	exec := newFakeExec()
	exec.scripts["git clone --depth 1 --branch main https://example.invalid/repo.git "+taskDir] =
		writeScript(t, "cat > .SRCINFO <<'EOF'\n"+testSrcinfo+"EOF")
	const noise = 2 << 20 // 2 MiB of build output
	exec.scripts["makepkg -s --noconfirm"] = writeScript(t,
		"head -c 2097152 /dev/zero | tr '\\0' x\ntouch foo-1.0-1-x86_64.pkg.tar.zst")
	r.execCommand = exec.command
	r.logThreshold = 64 * 1024

	r.executeTask(context.Background(), taskFor("t-1"), "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusSucceeded {
		if res.Error != nil {
			t.Logf("ERROR: %+v", *res.Error)
		}
		t.Fatalf("result = %+v, want succeeded", res)
	}

	var joined []byte
	for i, seg := range f.segments {
		if len(seg.Data) > r.logThreshold {
			t.Errorf("segment %d length = %d, exceeds the log threshold %d", i, len(seg.Data), r.logThreshold)
		}
		joined = append(joined, seg.Data...)
	}
	if got := strings.Count(string(joined), "x"); got != noise {
		t.Errorf("delivered %d noise bytes, want the full %d-byte output (log %d bytes total)",
			got, noise, len(joined))
	}
}

// TestTaskPrepareGeneratesSrcinfo covers the build-time .SRCINFO
// generation: a checkout without .SRCINFO (the file is a generated
// artifact) has it rendered via "makepkg --printsrcinfo" in the checkout,
// uploaded as the srcinfo artifact and parsed for the manifest.
func TestTaskPrepareGeneratesSrcinfo(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1")}
	r := runOneShotRunner(t, f)
	taskDir := r.workDir + "/t-1"
	exec := newFakeExec()
	exec.scripts["git clone --depth 1 --branch main https://example.invalid/repo.git "+taskDir] =
		writeScript(t, "touch PKGBUILD") // clone produces no .SRCINFO
	exec.scripts["makepkg --printsrcinfo"] = writeScript(t, "cat <<'EOF'\n"+testSrcinfo+"EOF")
	exec.scripts["makepkg -s --noconfirm"] = writeScript(t, "touch foo-1.0-1-x86_64.pkg.tar.zst")
	r.execCommand = exec.command

	r.executeTask(context.Background(), taskFor("t-1"), "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusSucceeded {
		if res.Error != nil {
			t.Logf("ERROR: %+v", *res.Error)
		}
		t.Fatalf("result = %+v, want succeeded", res)
	}
	if len(f.uploads) != 2 || f.uploads[1].name != ".SRCINFO" {
		t.Fatalf("uploads = %+v, want package + .SRCINFO", f.uploads)
	}
	got, err := os.ReadFile(filepath.Join(taskDir, ".SRCINFO"))
	if err != nil {
		t.Fatalf("read generated .SRCINFO: %v", err)
	}
	if string(got) != testSrcinfo {
		t.Errorf("generated .SRCINFO = %q, want the printsrcinfo output", got)
	}
}

// TestTaskPrepareGenerateSrcinfoFails asserts a failed "makepkg
// --printsrcinfo" fails the task at the prepare stage (the missing-file
// case is no longer a direct failure, but a generation failure is).
func TestTaskPrepareGenerateSrcinfoFails(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1")}
	r := runOneShotRunner(t, f)
	taskDir := r.workDir + "/t-1"
	exec := newFakeExec()
	exec.scripts["git clone --depth 1 --branch main https://example.invalid/repo.git "+taskDir] =
		writeScript(t, "true") // clone produces no checkout
	exec.scripts["makepkg --printsrcinfo"] = writeScript(t, "echo 'PKGBUILD broken' >&2\nexit 1")
	r.execCommand = exec.command

	r.executeTask(context.Background(), taskFor("t-1"), "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusFailed {
		t.Fatalf("result = %+v, want failed", res)
	}
	if res.Error == nil || res.Error.Stage != "prepare" || !strings.Contains(res.Error.Summary, "printsrcinfo") {
		t.Errorf("error = %+v, want prepare/printsrcinfo", res.Error)
	}
}

// TestTaskArchivePrepare downloads and extracts the source snapshot.
func TestTaskArchivePrepare(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1")}
	f.downloadBody = io.NopCloser(bytes.NewReader([]byte("fake archive bytes")))
	r := runOneShotRunner(t, f)
	taskDir := r.workDir + "/t-1"
	exec := newFakeExec()
	exec.scripts["tar --zstd -xf "+filepath.Join(taskDir, "source.tar.zst")] =
		writeScript(t, "touch .SRCINFO")
	exec.scripts["makepkg -s --noconfirm"] = writeScript(t, "touch foo-1.0-1-x86_64.pkg.tar.zst")
	r.execCommand = exec.command

	task := taskFor("t-1")
	task.Source.Mode = "archive"
	task.Source.Archive = "staging/t-1/source.tar.zst"
	r.executeTask(context.Background(), task, "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusSucceeded {
		if res.Error != nil {
			t.Logf("ERROR: %+v", *res.Error)
		}
		t.Fatalf("result = %+v, want succeeded", res)
	}
	if f.downloadName != "staging/t-1/source.tar.zst" {
		t.Errorf("downloaded %q, want the archive name", f.downloadName)
	}
	tarArgs := exec.callArgs("tar")
	if len(tarArgs) == 0 || !contains(tarArgs[0], "--zstd") {
		t.Errorf("tar args = %v, want --zstd extraction", tarArgs)
	}
	// Archive snapshots carry no git metadata: the commit stays empty and
	// the controller falls back.
	if res.Commit != "" {
		t.Errorf("archive mode commit = %q, want empty", res.Commit)
	}
}

// TestTaskCommitRecorded: a real clone (local file:// only) makes the agent
// report the actually checked-out commit.
func TestTaskCommitRecorded(t *testing.T) {
	url, wantCommit := makeRepo(t, testSrcinfo)

	f := &fakeClient{taskDetail: taskFor("t-1")}
	r := runOneShotRunner(t, f)
	exec := newFakeExec()
	exec.realGit = true
	exec.scripts["makepkg -s --noconfirm"] = writeScript(t, "touch foo-1.0-1-x86_64.pkg.tar.zst")
	r.execCommand = exec.command

	task := taskFor("t-1")
	task.Source.URL = url
	r.executeTask(context.Background(), task, "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusSucceeded {
		if res.Error != nil {
			t.Logf("ERROR: %+v", *res.Error)
		}
		t.Fatalf("result = %+v, want succeeded", res)
	}
	if res.Commit != wantCommit {
		t.Errorf("commit = %q, want the checked-out %q", res.Commit, wantCommit)
	}
}

// TestTaskCommitFallback: when rev-parse fails the agent reports an empty
// commit and the controller falls back to the dispatched one.
func TestTaskCommitFallback(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1")}
	r := runOneShotRunner(t, f)
	exec := flowExec(t, r.workDir, "t-1", testSrcinfo, []string{"foo-1.0-1-x86_64.pkg.tar.zst"}, map[string]string{
		"git rev-parse HEAD": writeScript(t, "echo 'not a git repo' 1>&2\nexit 128"),
	})
	r.execCommand = exec.command

	task := taskFor("t-1")
	r.executeTask(context.Background(), task, "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusSucceeded {
		if res.Error != nil {
			t.Logf("ERROR: %+v", *res.Error)
		}
		t.Fatalf("result = %+v, want succeeded", res)
	}
	if res.Commit != "" {
		t.Errorf("commit = %q, want empty fallback", res.Commit)
	}
}

// TestTaskTimeout asserts the deadline terminates makepkg and reports
// failed(timeout).
func TestTaskTimeout(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1")}
	r := runOneShotRunner(t, f)
	exec := flowExec(t, r.workDir, "t-1", testSrcinfo, nil, map[string]string{
		"makepkg -s --noconfirm": writeScript(t, "while :; do echo tick; sleep 0.2; done"),
	})
	r.execCommand = exec.command

	task := taskFor("t-1")
	task.Build.TimeoutSeconds = 1
	task.Hooks.OnFailure = []string{"onfail"}
	start := time.Now()
	r.executeTask(context.Background(), task, "tok")
	elapsed := time.Since(start)

	res := f.lastResult()
	if res == nil || res.Status != statusFailed {
		t.Fatalf("result = %+v, want failed", res)
	}
	if res.Error == nil || res.Error.Stage != "timeout" {
		t.Errorf("error = %+v, want stage timeout", res.Error)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("timeout fired after %v, want the deadline to elapse", elapsed)
	}
	assertHookCalled(t, exec, "onfail")
}

// TestTaskLateReportConflictIgnored asserts a 409 on the result report is
// logged and ignored.
func TestTaskLateReportConflictIgnored(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1")}
	f.reportErr = &api.APIError{Status: 409, Code: "conflict"}
	f.reportErrTo = 1
	r := runOneShotRunner(t, f)
	exec := flowExec(t, r.workDir, "t-1", testSrcinfo, []string{"foo-1.0-1-x86_64.pkg.tar.zst"}, nil)
	r.execCommand = exec.command

	r.executeTask(context.Background(), taskFor("t-1"), "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusSucceeded {
		t.Fatalf("result = %+v, want succeeded (conflict ignored)", res)
	}
}

func assertHookCalled(t *testing.T, exec *fakeExec, hook string) {
	t.Helper()
	for _, args := range exec.callArgs("sh") {
		if len(args) == 2 && args[0] == "-c" && args[1] == hook {
			return
		}
	}
	t.Errorf("hook %q was not executed", hook)
}

func assertHookNotCalled(t *testing.T, exec *fakeExec, hook string) {
	t.Helper()
	for _, args := range exec.callArgs("sh") {
		if len(args) == 2 && args[0] == "-c" && args[1] == hook {
			t.Errorf("hook %q unexpectedly executed", hook)
		}
	}
}
