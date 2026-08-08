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
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/detect"
)

// aurExecScript builds a fake exec.Command constructor backed by a shell
// script. The script records every argv line plus the GIT_SSH_COMMAND
// environment into the opLog, then runs body (a shell fragment).
func aurExecScript(t *testing.T, log *opLog, body string) func(ctx context.Context, name string, arg ...string) *exec.Cmd {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakeexec")
	logQ := strconv.Quote(log.path)
	script := "#!/bin/sh\n" +
		"{ printf '%s' \"$1\"; shift; for a in \"$@\"; do printf ' %s' \"$a\"; done; printf '\\n'; } >> " + logQ + "\n" +
		"printf 'GIT_SSH_COMMAND=%s\\n' \"$GIT_SSH_COMMAND\" >> " + logQ + "\n" +
		body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake exec script: %v", err)
	}
	return func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, path, append([]string{name}, arg...)...)
		cmd.Env = os.Environ()
		return cmd
	}
}

// TestAURPusherPush asserts the push command construction: the remote
// URL <user>@<server>:<name>.git, the <branch>:master refspec, the mirror
// working directory and the GIT_SSH_COMMAND identity injection.
func TestAURPusherPush(t *testing.T) {
	log := newOpLog(t)
	pusher := &AURPusher{
		cfg:         &config.AURConfig{Server: "aur.archlinux.org", KeyFile: "/data/aur_key", User: "aur"},
		execCommand: aurExecScript(t, log, ""),
	}
	err := pusher.Push(context.Background(), "/data/source/pkgbuilds.git", "foo-branch", "foo")
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	want := []string{
		"git -C /data/source/pkgbuilds.git push aur@aur.archlinux.org:foo.git foo-branch:master",
		`GIT_SSH_COMMAND=ssh -i "/data/aur_key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new`,
	}
	got := log.read()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("exec log = %v, want %v", got, want)
	}
}

// TestAURPusherCustomEndpoint asserts a custom server/user pair flows into
// the remote URL.
func TestAURPusherCustomEndpoint(t *testing.T) {
	log := newOpLog(t)
	pusher := &AURPusher{
		cfg:         &config.AURConfig{Server: "aur.example.net", KeyFile: "/data/k", User: "publisher"},
		execCommand: aurExecScript(t, log, ""),
	}
	if err := pusher.Push(context.Background(), "/data/source/x.git", "main", "pkg"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	got := log.read()
	if len(got) == 0 || !strings.Contains(got[0], "publisher@aur.example.net:pkg.git") {
		t.Errorf("remote url missing from %v", got)
	}
}

// TestAURPusherDisabled asserts a pusher without an SSH key never invokes
// git and reports ErrAURDisabled.
func TestAURPusherDisabled(t *testing.T) {
	log := newOpLog(t)
	pusher := &AURPusher{
		cfg:         &config.AURConfig{Server: "aur.archlinux.org", User: "aur"},
		execCommand: aurExecScript(t, log, ""),
	}
	if err := pusher.Push(context.Background(), "/data/source/x.git", "main", "pkg"); !errors.Is(err, ErrAURDisabled) {
		t.Errorf("Push without key = %v, want ErrAURDisabled", err)
	}
	if got := log.read(); len(got) != 0 {
		t.Errorf("exec log = %v, want no git invocation", got)
	}
	// A nil config behaves the same.
	if err := (&AURPusher{}).Push(context.Background(), "/data/source/x.git", "main", "pkg"); !errors.Is(err, ErrAURDisabled) {
		t.Errorf("Push with nil config = %v, want ErrAURDisabled", err)
	}
}

// TestAURPusherNonFastForward asserts a git rejection (the AUR master has
// commits the branch does not contain) surfaces as ErrAURNonFastForward
// with the remote output attached, and is never forced.
func TestAURPusherNonFastForward(t *testing.T) {
	log := newOpLog(t)
	pusher := &AURPusher{
		cfg: &config.AURConfig{Server: "aur.archlinux.org", KeyFile: "/data/k", User: "aur"},
		execCommand: aurExecScript(t, log, `printf '%s' ' ! [rejected]        foo-branch -> master (non-fast-forward)' >&2
exit 1`),
	}
	err := pusher.Push(context.Background(), "/data/source/x.git", "foo-branch", "foo")
	if !errors.Is(err, ErrAURNonFastForward) {
		t.Errorf("Push = %v, want ErrAURNonFastForward", err)
	}
	if err == nil || !strings.Contains(err.Error(), "non-fast-forward") {
		t.Errorf("Push error = %v, want remote output attached", err)
	}
	got := log.read()
	if len(got) == 0 || strings.Contains(strings.Join(got, " "), "--force") {
		t.Errorf("push must never be forced, got %v", got)
	}
}

// TestAURPusherGenericError asserts unrelated push failures are reported
// as plain errors.
func TestAURPusherGenericError(t *testing.T) {
	log := newOpLog(t)
	pusher := &AURPusher{
		cfg: &config.AURConfig{Server: "aur.archlinux.org", KeyFile: "/data/k", User: "aur"},
		execCommand: aurExecScript(t, log, `printf '%s' 'fatal: unable to access' >&2
exit 1`),
	}
	err := pusher.Push(context.Background(), "/data/source/x.git", "main", "foo")
	if err == nil || errors.Is(err, ErrAURNonFastForward) || errors.Is(err, ErrAURDisabled) {
		t.Errorf("Push = %v, want a generic push error", err)
	}
}

// aurCall records one AURPublisher.Push invocation.
type aurCall struct {
	mirrorDir string
	branch    string
	aurName   string
}

// fakeAURPusher records Push calls and can be told to fail.
type fakeAURPusher struct {
	mu      sync.Mutex
	calls   []aurCall
	pushErr error
}

// Push implements AURPublisher.
func (f *fakeAURPusher) Push(_ context.Context, mirrorDir, branch, aurName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, aurCall{mirrorDir: mirrorDir, branch: branch, aurName: aurName})
	return f.pushErr
}

func (f *fakeAURPusher) snapshot() []aurCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]aurCall(nil), f.calls...)
}

// enqueueAUR enqueues a change carrying the [aur] configuration and
// returns the active task id.
func (e *testEnv) enqueueAUR(t *testing.T, pkgbase, branch, aurName string, submit bool) string {
	t.Helper()
	c := detectChange(pkgbase, branch)
	c.AUR = detect.AURConfig{Name: aurName, Submit: submit}
	c.Reason = detect.ReasonCommit
	if err := e.o.Enqueue(context.Background(), c, false); err != nil {
		t.Fatalf("Enqueue %s: %v", pkgbase, err)
	}
	return e.activeTaskFor(t, pkgbase)
}

// finishSucceeded runs the claim + report stage of one task with the
// standard artifact set and the commit the harness reports (deadbeef).
func (e *testEnv) finishSucceeded(t *testing.T, taskID, pkgbase string) {
	t.Helper()
	artifacts := testArtifacts(pkgbase, "1.0-1")
	for _, a := range artifacts {
		e.stage(t, taskID, a.File)
	}
	worker := "w-" + pkgbase
	e.registerWorker(t, worker, "host", "host", 1)
	claimedID, token := e.claim(t, worker)
	if claimedID != taskID {
		t.Fatalf("claimed %s, want %s", claimedID, taskID)
	}
	res := ResultReq{Status: "succeeded", Commit: "deadbeef", Artifacts: artifacts}
	if err := e.o.ReportResult(context.Background(), claimedID, token, res); err != nil {
		t.Fatalf("ReportResult: %v", err)
	}
}

// TestPublishAURCommitChange covers the full trigger: a branch whose
// dotfile opts in ([aur].submit with a name) and whose successful build
// advanced the branch commit is pushed, and the outcome is recorded with
// an empty error.
func TestPublishAURCommitChange(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.AUR.KeyFile = "/data/aur_key"
	pusher := &fakeAURPusher{}
	env.o.aurPusher = pusher

	env.finishSucceeded(t, env.enqueueAUR(t, "aurpkg", "main", "aur-name", true), "aurpkg")

	calls := pusher.snapshot()
	if len(calls) != 1 || calls[0] != (aurCall{mirrorDir: env.o.mirrorDir, branch: "main", aurName: "aur-name"}) {
		t.Fatalf("Push calls = %+v, want one push of main as aur-name", calls)
	}
	pkg, err := env.store.GetPackageByBase(context.Background(), "aurpkg")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if pkg.LastAURCommit != "deadbeef" || pkg.LastAURError != "" || pkg.LastAURPushAt == nil {
		t.Errorf("recorded push = commit %q error %q at %v", pkg.LastAURCommit, pkg.LastAURError, pkg.LastAURPushAt)
	}
	if len(env.not.aurCalls) != 0 {
		t.Errorf("unexpected AUR notifications: %+v", env.not.aurCalls)
	}
}

// TestPublishAURNoPush asserts the trigger gate: without the submit flag
// or without an AUR package name, or with AUR publishing disabled by the
// controller config, no push happens and nothing is recorded.
func TestPublishAURNoPush(t *testing.T) {
	for _, tc := range []struct {
		name    string
		aurName string
		submit  bool
		keyFile string
		pusher  *fakeAURPusher
	}{
		{name: "no submit flag", aurName: "aur-name", submit: false, keyFile: "/data/k", pusher: &fakeAURPusher{}},
		{name: "no aur name", aurName: "", submit: true, keyFile: "/data/k", pusher: &fakeAURPusher{}},
		{name: "controller disabled", aurName: "aur-name", submit: true, keyFile: "", pusher: &fakeAURPusher{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.cfg.AUR.KeyFile = tc.keyFile
			env.o.aurPusher = tc.pusher
			env.finishSucceeded(t, env.enqueueAUR(t, "aurpkg", "main", tc.aurName, tc.submit), "aurpkg")

			if calls := tc.pusher.snapshot(); len(calls) != 0 {
				t.Errorf("Push calls = %+v, want none", calls)
			}
			pkg, err := env.store.GetPackageByBase(context.Background(), "aurpkg")
			if err != nil {
				t.Fatalf("GetPackageByBase: %v", err)
			}
			if pkg.LastAURPushAt != nil || pkg.LastAURCommit != "" || pkg.LastAURError != "" {
				t.Errorf("AUR record = %+v, want untouched", pkg)
			}
		})
	}
}

// TestPublishAURUnchangedCommit asserts the commit gate: a successful
// build whose branch commit did not change (upstream-only or manual
// rebuild) is not pushed.
func TestPublishAURUnchangedCommit(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.AUR.KeyFile = "/data/aur_key"
	pusher := &fakeAURPusher{}
	env.o.aurPusher = pusher

	// First build advances the commit -> push.
	env.finishSucceeded(t, env.enqueueAUR(t, "aurpkg", "main", "aur-name", true), "aurpkg")
	if calls := pusher.snapshot(); len(calls) != 1 {
		t.Fatalf("first build Push calls = %d, want 1", len(calls))
	}
	// Second build of the same branch tip -> commit unchanged -> no push.
	// Drop the round-set entry first so the same pkgbase can be enqueued
	// again within one test.
	env.o.roundMu.Lock()
	delete(env.o.roundSet, "aurpkg")
	env.o.roundMu.Unlock()
	env.finishSucceeded(t, env.enqueueAUR(t, "aurpkg", "main", "aur-name", true), "aurpkg")
	if calls := pusher.snapshot(); len(calls) != 1 {
		t.Errorf("second build Push calls = %d, want still 1 (unchanged commit)", len(calls))
	}
	pkg, err := env.store.GetPackageByBase(context.Background(), "aurpkg")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if pkg.LastAURCommit != "deadbeef" || pkg.LastAURError != "" {
		t.Errorf("recorded push = %+v, want the first push only", pkg)
	}
}

// TestPublishAURFailure asserts a rejected push is recorded with the error
// text and the maintainers are notified with the AUR push details.
func TestPublishAURFailure(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.AUR.KeyFile = "/data/aur_key"
	pusher := &fakeAURPusher{pushErr: ErrAURNonFastForward}
	env.o.aurPusher = pusher

	c := detectChange("aurpkg", "main", "a@example.org")
	c.AUR = detect.AURConfig{Name: "aur-name", Submit: true}
	c.Reason = detect.ReasonCommit
	if err := env.o.Enqueue(context.Background(), c, false); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	taskID := env.activeTaskFor(t, "aurpkg")
	env.finishSucceeded(t, taskID, "aurpkg")

	got, err := env.store.GetPackageByBase(context.Background(), "aurpkg")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if got.LastAURCommit != "deadbeef" || got.LastAURError == "" {
		t.Errorf("recorded push = commit %q error %q, want the failure recorded", got.LastAURCommit, got.LastAURError)
	}
	if len(env.not.aurCalls) != 1 {
		t.Fatalf("AUR notifications = %+v, want one", env.not.aurCalls)
	}
	info := env.not.aurCalls[0]
	if info.Pkgbase != "aurpkg" || info.Branch != "main" || info.AURName != "aur-name" || info.Commit != "deadbeef" || info.Error == "" {
		t.Errorf("AUR notification = %+v", info)
	}
}

// TestPublishAURMaintainerEmails asserts the failure notification uses the
// maintainer email addresses.
func TestPublishAURMaintainerEmails(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.AUR.KeyFile = "/data/aur_key"
	pusher := &fakeAURPusher{pushErr: errors.New("rejected")}
	env.o.aurPusher = pusher

	c := detectChange("aurpkg", "main", "a@example.org", "b@example.org")
	c.AUR = detect.AURConfig{Name: "aur-name", Submit: true}
	c.Reason = detect.ReasonCommit
	if err := env.o.Enqueue(context.Background(), c, false); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	taskID := env.activeTaskFor(t, "aurpkg")
	env.finishSucceeded(t, taskID, "aurpkg")

	if len(env.not.aurCalls) != 1 {
		t.Fatalf("AUR notifications = %d, want one", len(env.not.aurCalls))
	}
	if env.not.aurRecipients == nil || len(env.not.aurRecipients) != 2 ||
		env.not.aurRecipients[0] != "a@example.org" || env.not.aurRecipients[1] != "b@example.org" {
		t.Errorf("AUR notification recipients = %v", env.not.aurRecipients)
	}
}

// TestPublishAURNoMaintainerSilent asserts a push failure without
// maintainers is recorded but produces no notification.
func TestPublishAURNoMaintainerSilent(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.AUR.KeyFile = "/data/aur_key"
	env.o.aurPusher = &fakeAURPusher{pushErr: errors.New("rejected")}

	env.finishSucceeded(t, env.enqueueAUR(t, "aurpkg", "main", "aur-name", true), "aurpkg")

	if len(env.not.aurCalls) != 0 {
		t.Errorf("AUR notifications = %+v, want none without maintainers", env.not.aurCalls)
	}
	got, err := env.store.GetPackageByBase(context.Background(), "aurpkg")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if got.LastAURError == "" {
		t.Error("push failure not recorded")
	}
}
