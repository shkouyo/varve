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
	"reflect"
	"strings"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/repo"
	"git.0x0f.dev/varve/internal/storage"
)

// stageClaim reports a succeeded result for a freshly claimed task.
func (e *testEnv) reportSucceeded(t *testing.T, taskID, token string, artifacts []repo.Artifact, commit string) error {
	t.Helper()
	return e.o.ReportResult(context.Background(), taskID, token,
		ResultReq{Status: "succeeded", Commit: commit, Artifacts: artifacts})
}

// TestReportSucceeded covers the happy path: manifest verification,
// ingest with the resolved worker name, the SQLite transaction (succeeded
// + package update), staging cleanup and commit recording.
func TestReportSucceeded(t *testing.T) {
	env := newTestEnv(t)
	artifacts := testArtifacts("foo", "1.0-1")
	taskID := env.enqueue(t, "foo", "foo", "maint@example.org")
	for _, a := range artifacts {
		env.stage(t, taskID, a.File)
	}
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, token := env.claim(t, "w1")
	if claimed != taskID {
		t.Fatalf("claimed %s, want %s", claimed, taskID)
	}

	if err := env.reportSucceeded(t, claimed, token, artifacts, "deadbeef"); err != nil {
		t.Fatalf("ReportResult: %v", err)
	}

	task, err := env.store.GetTask(context.Background(), claimed)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "succeeded" {
		t.Errorf("state = %q, want succeeded", task.State)
	}
	build, err := env.store.GetBuild(context.Background(), task.BuildID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if build.Commit != "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2" {
		t.Errorf("build commit = %q, want the dispatched commit (fallback)", build.Commit)
	}
	// The actual checked-out commit reaches the sidecar via the build
	// handed to Ingest: the side file [build].commit is the authoritative
	// record.
	if env.up.lastBuild == nil || env.up.lastBuild.Commit != "deadbeef" {
		t.Errorf("ingest build commit = %+v, want the reported commit deadbeef", env.up.lastBuild)
	}
	if build.Artifacts == nil || len(build.Artifacts) != len(artifacts) {
		t.Errorf("build artifacts = %+v", build.Artifacts)
	}
	pkg, err := env.store.GetPackageByBase(context.Background(), "foo")
	if err != nil {
		t.Fatalf("GetPackageByBase: %v", err)
	}
	if pkg.CurrentVersion != "1.0-1" || pkg.Pkgdesc != "test package" || pkg.LastBuildID != task.BuildID {
		t.Errorf("package update = %+v, want version 1.0-1 / pkgdesc / last build", pkg)
	}
	// The built commit must advance packages.last_commit, otherwise
	// detection re-enqueues the unchanged branch on every round
	// (infinite rebuild loop). The detect side (TestPollOncePlainPackage)
	// asserts a recorded build suppresses the next change.
	if pkg.LastCommit != "deadbeef" {
		t.Errorf("last_commit = %q, want the built commit deadbeef", pkg.LastCommit)
	}
	// The .SRCINFO metadata parsed at ingest must reach the package row:
	// pkgname/source/pkgver/pkgrel used to be dropped.
	if !reflect.DeepEqual(pkg.Pkgname, []string{"testpkg"}) ||
		!reflect.DeepEqual(pkg.Source, []string{"https://example.org/foo.tar.gz"}) ||
		pkg.Pkgver != "1.2.3" || pkg.Pkgrel != "1" || pkg.Epoch != 1 {
		t.Errorf("pkgname/source/pkgver/pkgrel/epoch = %v/%v/%q/%q/%d, want testpkg/url/1.2.3/1/1",
			pkg.Pkgname, pkg.Source, pkg.Pkgver, pkg.Pkgrel, pkg.Epoch)
	}
	if pkg.URL != "https://example.org/foo" ||
		!reflect.DeepEqual(pkg.Licenses, []string{"MIT"}) ||
		!reflect.DeepEqual(pkg.Conflicts, []string{"testpkg-legacy"}) ||
		!reflect.DeepEqual(pkg.Provides, []string{"testpkg-provided"}) {
		t.Errorf("url/licenses/conflicts/provides = %q/%v/%v/%v, want the staged .SRCINFO values",
			pkg.URL, pkg.Licenses, pkg.Conflicts, pkg.Provides)
	}
	if env.up.worker != "w1" {
		t.Errorf("ingest workerName = %q, want w1", env.up.worker)
	}
	// Staging cleaned up.
	for _, a := range artifacts {
		if _, err := env.fs.Stat(context.Background(), env.fs.StagingPath(claimed, a.File)); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("staging %s not cleaned up", a.File)
		}
	}
	// No notification on success; key material cleared; token dropped.
	if len(env.not.calls) != 0 {
		t.Errorf("unexpected notifications: %+v", env.not.calls)
	}
	if len(env.sig.cleared) != 1 || env.sig.cleared[0] != claimed {
		t.Errorf("signer cleared = %v, want [%s]", env.sig.cleared, claimed)
	}
	if err := env.reportSucceeded(t, claimed, token, artifacts, ""); !errors.Is(err, ErrConflict) {
		t.Errorf("late report = %v, want ErrConflict", err)
	}
}

// TestReportVerifyFailures covers the manifest verification failures: a
// sha256 mismatch, a missing artifact and a failed signature verification
// all finalize failed(verify) with notification and staging cleanup.
func TestReportVerifyFailures(t *testing.T) {
	t.Run("sha256 mismatch", func(t *testing.T) {
		env := newTestEnv(t)
		artifacts := testArtifacts("foo", "1.0-1")
		artifacts[0].SHA256 = strings.Repeat("0", 64)
		taskID := env.enqueue(t, "foo", "foo", "maint@example.org")
		for _, a := range artifacts {
			env.stage(t, taskID, a.File)
		}
		env.registerWorker(t, "w1", "host", "host", 1)
		claimed, token := env.claim(t, "w1")
		if err := env.reportSucceeded(t, claimed, token, artifacts, ""); err != nil {
			t.Fatalf("ReportResult: %v", err)
		}
		env.assertFailedVerify(t, claimed, "sha256")
	})

	t.Run("invalid pkgname", func(t *testing.T) {
		env := newTestEnv(t)
		artifacts := testArtifacts("foo", "1.0-1")
		artifacts[0].Pkgname = "-q"
		taskID := env.enqueue(t, "foo", "foo", "maint@example.org")
		for _, a := range artifacts {
			env.stage(t, taskID, a.File)
		}
		env.registerWorker(t, "w1", "host", "host", 1)
		claimed, token := env.claim(t, "w1")
		if err := env.reportSucceeded(t, claimed, token, artifacts, ""); err != nil {
			t.Fatalf("ReportResult: %v", err)
		}
		env.assertFailedVerify(t, claimed, "pkgname")
	})

	t.Run("missing artifact", func(t *testing.T) {
		env := newTestEnv(t)
		artifacts := testArtifacts("foo", "1.0-1")
		taskID := env.enqueue(t, "foo", "foo", "maint@example.org")
		// Only the srcinfo is staged; the package file is missing.
		env.stage(t, taskID, ".SRCINFO")
		env.registerWorker(t, "w1", "host", "host", 1)
		claimed, token := env.claim(t, "w1")
		if err := env.reportSucceeded(t, claimed, token, artifacts, ""); err != nil {
			t.Fatalf("ReportResult: %v", err)
		}
		env.assertFailedVerify(t, claimed, "missing artifact")
	})

	t.Run("signature verification", func(t *testing.T) {
		env := newTestEnv(t)
		env.cfg.Repo.Sign = "packages"
		env.sig.verifyErr = errors.New("bad signature")
		artifacts := testArtifacts("foo", "1.0-1")
		artifacts = append(artifacts, repo.Artifact{
			File: "foo-1.0-1-x86_64.pkg.tar.zst.sig", Kind: "signature",
			Size:   int64(len(stagedContent("foo-1.0-1-x86_64.pkg.tar.zst.sig"))),
			SHA256: sha256Hex(stagedContent("foo-1.0-1-x86_64.pkg.tar.zst.sig")),
		})
		taskID := env.enqueue(t, "foo", "foo", "maint@example.org")
		for _, a := range artifacts {
			env.stage(t, taskID, a.File)
		}
		env.registerWorker(t, "w1", "host", "host", 1)
		claimed, token := env.claim(t, "w1")
		if err := env.reportSucceeded(t, claimed, token, artifacts, ""); err != nil {
			t.Fatalf("ReportResult: %v", err)
		}
		env.assertFailedVerify(t, claimed, "signature")
	})
}

// assertFailedVerify checks the common post-verify-failure state.
func (e *testEnv) assertFailedVerify(t *testing.T, taskID, wantErrFragment string) {
	t.Helper()
	task, err := e.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "failed" {
		t.Errorf("state = %q, want failed", task.State)
	}
	build, err := e.store.GetBuild(context.Background(), task.BuildID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if !strings.Contains(build.Error, "verify") || !strings.Contains(build.Error, wantErrFragment) {
		t.Errorf("build error = %q, want verify + %q", build.Error, wantErrFragment)
	}
	if len(e.not.calls) != 1 || e.not.calls[0].Stage != "verify" {
		t.Errorf("notifications = %+v, want one verify notification", e.not.calls)
	}
}

// TestReportIngestOrder asserts the ingest sequence recorded by the
// fakes: verification reads happen before Ingest, and the staging cleanup
// only after Ingest (the SQLite commit sits between them and is
// observable as the terminal state once the report returns).
func TestReportIngestOrder(t *testing.T) {
	env := newTestEnv(t)
	artifacts := testArtifacts("foo", "1.0-1")
	taskID := env.enqueue(t, "foo", "foo")
	for _, a := range artifacts {
		env.stage(t, taskID, a.File)
	}
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, token := env.claim(t, "w1")
	if err := env.reportSucceeded(t, claimed, token, artifacts, ""); err != nil {
		t.Fatalf("ReportResult: %v", err)
	}

	ops := env.log.read()
	firstGet, ingest, firstDelete := -1, -1, -1
	for i, l := range ops {
		switch {
		case strings.HasPrefix(l, "get "):
			if firstGet < 0 {
				firstGet = i
			}
		case strings.HasPrefix(l, "ingest "):
			ingest = i
		case strings.HasPrefix(l, "delete "):
			if firstDelete < 0 {
				firstDelete = i
			}
		}
	}
	if firstGet < 0 || ingest < 0 || firstDelete < 0 {
		t.Fatalf("incomplete op sequence: %v", ops)
	}
	if firstGet > ingest {
		t.Errorf("verification (%d) must precede Ingest (%d): %v", firstGet, ingest, ops)
	}
	if ingest > firstDelete {
		t.Errorf("Ingest (%d) must precede staging cleanup (%d): %v", ingest, firstDelete, ops)
	}
	task, err := env.store.GetTask(context.Background(), claimed)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "succeeded" {
		t.Errorf("SQLite commit did not land: state = %q", task.State)
	}
}

// TestReportIngestFailure covers the ingest error path: the task fails with
// stage "ingest", a notification is sent and the staging area is preserved
// for a retry.
func TestReportIngestFailure(t *testing.T) {
	env := newTestEnv(t)
	env.up.ingestErr = errors.New("repo-add failed: boom")
	artifacts := testArtifacts("foo", "1.0-1")
	taskID := env.enqueue(t, "foo", "foo", "maint@example.org")
	for _, a := range artifacts {
		env.stage(t, taskID, a.File)
	}
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, token := env.claim(t, "w1")
	if err := env.reportSucceeded(t, claimed, token, artifacts, ""); err != nil {
		t.Fatalf("ReportResult: %v", err)
	}

	task, err := env.store.GetTask(context.Background(), claimed)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "failed" {
		t.Errorf("state = %q, want failed", task.State)
	}
	build, err := env.store.GetBuild(context.Background(), task.BuildID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if !strings.Contains(build.Error, "ingest") || !strings.Contains(build.Error, "boom") {
		t.Errorf("build error = %q, want ingest stage", build.Error)
	}
	// The failed build row records the reported artifacts, so the page
	// matches the repository state (the files were already moved).
	if len(build.Artifacts) != len(artifacts) {
		t.Errorf("failed build artifacts = %+v, want the reported manifest recorded", build.Artifacts)
	}
	if len(env.not.calls) != 1 || env.not.calls[0].Stage != "ingest" {
		t.Errorf("notifications = %+v, want one ingest notification", env.not.calls)
	}
	// Staging is preserved (the task is terminal, so the sweep is the
	// cleanup backstop).
	for _, a := range artifacts {
		if _, err := env.fs.Stat(context.Background(), env.fs.StagingPath(claimed, a.File)); err != nil {
			t.Errorf("staging %s was not preserved: %v", a.File, err)
		}
	}
}

// TestReportCancellationPriority covers cancellation priority: after a
// cancel request only a cancelled report is accepted; any other status is
// a conflict.
func TestReportCancellationPriority(t *testing.T) {
	env := newTestEnv(t)
	artifacts := testArtifacts("foo", "1.0-1")
	taskID := env.enqueue(t, "foo", "foo")
	for _, a := range artifacts {
		env.stage(t, taskID, a.File)
	}
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, token := env.claim(t, "w1")

	if err := env.o.CancelTask(context.Background(), claimed); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if err := env.reportSucceeded(t, claimed, token, artifacts, ""); !errors.Is(err, ErrConflict) {
		t.Errorf("succeeded report after cancel = %v, want ErrConflict", err)
	}
	if err := env.o.ReportResult(context.Background(), claimed, token,
		ResultReq{Status: "failed", Error: &ResultError{Stage: "makepkg", Summary: "killed"}}); !errors.Is(err, ErrConflict) {
		t.Errorf("failed report after cancel = %v, want ErrConflict", err)
	}
	// The cancelled report is accepted.
	if err := env.o.ReportResult(context.Background(), claimed, token, ResultReq{Status: "cancelled"}); err != nil {
		t.Fatalf("cancelled report: %v", err)
	}
	task, err := env.store.GetTask(context.Background(), claimed)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "cancelled" {
		t.Errorf("state = %q, want cancelled", task.State)
	}
}

// TestReportFinalizesAfterClientDisconnect is the regression test for the
// orphaned-task failure mode behind the 502 incident: the worker's result
// POST times out and the client disconnects while the ingest is still in
// flight. The terminal write must survive the canceled request context, or
// the task would stay "running" forever with cancellation and stall
// recovery both ineffective.
func TestReportFinalizesAfterClientDisconnect(t *testing.T) {
	run := func(t *testing.T, ingestErr error, wantState string) {
		t.Helper()
		env := newTestEnv(t)
		artifacts := testArtifacts("foo", "1.0-1")
		taskID := env.enqueue(t, "foo", "foo")
		for _, a := range artifacts {
			env.stage(t, taskID, a.File)
		}
		env.registerWorker(t, "w1", "host", "host", 1)
		claimed, token := env.claim(t, "w1")

		env.up.entered = make(chan struct{})
		env.up.block = make(chan struct{})
		env.up.ingestErr = ingestErr

		ctx, cancel := context.WithCancel(context.Background())
		reportDone := make(chan struct{})
		var reportErr error
		go func() {
			reportErr = env.o.ReportResult(ctx, claimed, token,
				ResultReq{Status: "succeeded", Artifacts: artifacts})
			close(reportDone)
		}()

		select {
		case <-env.up.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("ingest never started")
		}
		cancel() // the worker's result POST timed out and disconnected

		close(env.up.block)
		select {
		case <-reportDone:
		case <-time.After(5 * time.Second):
			t.Fatal("report did not finish")
		}
		if reportErr != nil {
			t.Fatalf("report: %v", reportErr)
		}
		task, err := env.store.GetTask(context.Background(), claimed)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if task.State != wantState {
			t.Fatalf("state = %q, want %q (finalize must not be lost with the canceled request)", task.State, wantState)
		}
	}

	t.Run("ingest failure finalizes failed", func(t *testing.T) {
		run(t, errors.New("ingest failed mid-flight"), "failed")
	})
	t.Run("ingest success finalizes succeeded", func(t *testing.T) {
		run(t, nil, "succeeded")
	})
}
