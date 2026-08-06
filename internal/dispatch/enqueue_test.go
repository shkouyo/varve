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
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/detect"
	"git.0x0f.dev/varve/internal/repo"
	"git.0x0f.dev/varve/internal/storage"
)

// TestEnqueueNameConflict covers the packages-table check: a change whose
// pkgbase equals a pkgname produced by the last build of another package
// is rejected with ErrConflict.
func TestEnqueueNameConflict(t *testing.T) {
	env := newTestEnv(t)
	// foo ships a subpackage named libfoo.
	env.buildSucceeded(t, "foo", []repo.Artifact{
		{File: "foo-1.0-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "foo", Version: "1.0-1", Arch: "x86_64",
			Size: int64(len(stagedContent("foo-1.0-1-x86_64.pkg.tar.zst"))), SHA256: sha256Hex(stagedContent("foo-1.0-1-x86_64.pkg.tar.zst"))},
		{File: "libfoo-1.0-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "libfoo", Version: "1.0-1", Arch: "x86_64",
			Size: int64(len(stagedContent("libfoo-1.0-1-x86_64.pkg.tar.zst"))), SHA256: sha256Hex(stagedContent("libfoo-1.0-1-x86_64.pkg.tar.zst"))},
		{File: ".SRCINFO", Kind: "srcinfo",
			Size: int64(len(stagedContent(".SRCINFO"))), SHA256: sha256Hex(stagedContent(".SRCINFO"))},
	})
	// A new branch whose pkgbase is libfoo collides with foo's subpackage.
	err := env.o.Enqueue(context.Background(), detectChange("libfoo", "libfoo"), false)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Enqueue(libfoo) = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "libfoo") {
		t.Errorf("conflict error %q should name the colliding pkgbase", err)
	}
	// The queue is not blocked: another unrelated branch enqueues fine.
	env.enqueue(t, "bar", "bar")
}

// TestEnqueueDedupActive covers the partial unique index: a second active
// task for the same package is rejected, even with force=true (admin
// rebuilds cannot double-queue).
func TestEnqueueDedupActive(t *testing.T) {
	env := newTestEnv(t)
	env.enqueue(t, "foo", "foo")

	err := env.o.Enqueue(context.Background(), detectChange("foo", "foo"), false)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second Enqueue = %v, want ErrConflict", err)
	}
	err = env.o.Enqueue(context.Background(), detectChange("foo", "foo"), true)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("force Enqueue while active = %v, want ErrConflict", err)
	}
}

// TestEnqueueSkipUnsupportedArch covers the architecture queue gate: a
// change whose declared architectures have no intersection with what the
// deployment can build (static baseline + registered workers, any status)
// is rejected with ErrArchUnsupported and creates neither a package row
// nor a task, so nothing sits in the queue unclaimable forever. "any"
// packages and packages with at least one supported element pass, and the
// baseline default alone (no workers registered) accepts x86_64.
func TestEnqueueSkipUnsupportedArch(t *testing.T) {
	ctx := context.Background()

	t.Run("no intersection skips without side effects", func(t *testing.T) {
		env := newTestEnv(t)
		env.registerWorkerArch(t, "x86-w", "x86_64", 1)

		for _, c := range []detect.Change{
			detectChangeArch("exotic", "exotic", "armv7h"),
			detectChangeArch("multi-exotic", "multi-exotic", "armv7h|riscv64"),
		} {
			err := env.o.Enqueue(ctx, c, false)
			if !errors.Is(err, ErrArchUnsupported) {
				t.Fatalf("Enqueue(%s) = %v, want ErrArchUnsupported", c.Package.Pkgbase, err)
			}
			if _, gerr := env.store.GetPackageByBase(ctx, c.Package.Pkgbase); !errors.Is(gerr, db.ErrNotFound) {
				t.Errorf("%s: package row created (err = %v), want none", c.Package.Pkgbase, gerr)
			}
		}
	})

	t.Run("unsupported also rejects forced admin rebuild", func(t *testing.T) {
		env := newTestEnv(t)
		if err := env.o.Enqueue(ctx, detectChangeArch("exotic", "exotic", "armv7h"), true); !errors.Is(err, ErrArchUnsupported) {
			t.Fatalf("force Enqueue = %v, want ErrArchUnsupported", err)
		}
	})

	t.Run("any package always passes", func(t *testing.T) {
		env := newTestEnv(t)
		env.registerWorkerArch(t, "x86-w", "x86_64", 1)
		env.enqueueArch(t, "archless", "archless", "any")
	})

	t.Run("multi-arch passes when one element is supported", func(t *testing.T) {
		env := newTestEnv(t)
		env.registerWorkerArch(t, "x86-w", "x86_64", 1)
		env.enqueueArch(t, "both", "both", "aarch64|x86_64")
	})

	t.Run("fresh deployment with no workers accepts baseline", func(t *testing.T) {
		env := newTestEnv(t)
		env.enqueue(t, "plain", "plain") // default change arch x86_64
	})
}

// TestEnqueueRoundDedup covers the round set: the same pkgbase enqueued
// twice in one detection round conflicts.
func TestEnqueueRoundDedup(t *testing.T) {
	env := newTestEnv(t)
	env.enqueue(t, "foo", "foo")
	// Mark the task terminal so only the round set can catch the duplicate.
	task, err := env.store.GetTask(context.Background(), env.activeTaskFor(t, "foo"))
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if err := env.store.WithTx(context.Background(), func(tx *db.Tx) error {
		return tx.FinalizeTask(context.Background(), task.ID, "failed", "x", env.now, nil, nil)
	}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	err = env.o.Enqueue(context.Background(), detectChange("foo", "foo"), false)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("round duplicate = %v, want ErrConflict", err)
	}
}

// TestEnqueueArchive covers archive mode: with cfg.Source.FetchKey set,
// Enqueue snapshots the branch via "git archive --format=tar.zst
// <commit>" into staging/<task-id>/source.tar.zst.
func TestEnqueueArchive(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Source.FetchKey = "/keys/id_rsa"
	env.cfg.Source.URL = "git@git.example.org:pkgbuilds.git"
	state := &gitState{Commit: "c0ffee0000000000000000000000000000000000", Archive: []byte("fake-zstd-tar")}
	env.o.execCommand = fakeGitFor(t, env.log, state)
	taskID := env.enqueue(t, "foo", "foo")

	if _, err := env.fs.Stat(context.Background(), storage.StagingPath(taskID, sourceArchiveName)); err != nil {
		t.Fatalf("source archive not staged: %v", err)
	}
	got, err := env.fs.GetBytes(context.Background(), storage.StagingPath(taskID, sourceArchiveName))
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if string(got) != "fake-zstd-tar" {
		t.Errorf("archive content = %q", got)
	}
	lines := env.log.read()
	found := false
	for _, l := range lines {
		if strings.Contains(l, "git") && strings.Contains(l, "archive") &&
			strings.Contains(l, "--format=tar.zst") && strings.Contains(l, state.Commit) {
			found = true
		}
	}
	if !found {
		t.Errorf("git archive invocation not recorded; ops = %v", lines)
	}
}

// TestEnqueueNoArchive covers clone mode: with no FetchKey the source is not
// packaged and the staging area stays empty.
func TestEnqueueNoArchive(t *testing.T) {
	env := newTestEnv(t)
	taskID := env.enqueue(t, "foo", "foo")
	for _, l := range env.log.read() {
		if strings.Contains(l, "archive") {
			t.Errorf("unexpected archive invocation: %q", l)
		}
	}
	if _, err := env.fs.Stat(context.Background(), storage.StagingPath(taskID, sourceArchiveName)); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("source archive staged in clone mode: %v", err)
	}
}

// TestEnqueueArchiveFailure covers the failure decision: a failed
// snapshot finalizes the task as failed (stage ingest), notifies and
// does not block the queue.
func TestEnqueueArchiveFailure(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Source.FetchKey = "/keys/id_rsa"
	env.o.execCommand = fakeGitFor(t, env.log, &gitState{Commit: "c0ffee0000000000000000000000000000000000", Fail: "archive"})

	if err := env.o.Enqueue(context.Background(), detectChange("foo", "foo", "maint@example.org"), false); err != nil {
		t.Fatalf("Enqueue with failing archive = %v, want nil (queue not blocked)", err)
	}
	builds, _, err := env.store.ListBuilds(context.Background(), 1, 10, false)
	if err != nil {
		t.Fatalf("ListBuilds: %v", err)
	}
	if len(builds) != 1 || builds[0].Status != "failed" {
		t.Fatalf("builds = %+v, want one failed build", builds)
	}
	if !strings.Contains(builds[0].Error, "ingest") {
		t.Errorf("build error = %q, want ingest stage", builds[0].Error)
	}
	if len(env.not.calls) != 1 || env.not.calls[0].Stage != "ingest" {
		t.Errorf("notifications = %+v, want one ingest notification", env.not.calls)
	}
	// The queue keeps flowing for other branches (with a working git).
	env.o.execCommand = fakeGitFor(t, env.log, &gitState{Commit: "c0ffee0000000000000000000000000000000000"})
	env.enqueue(t, "bar", "bar")
}
