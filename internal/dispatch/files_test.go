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
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/sign"
	"git.0x0f.dev/varve/internal/storage"
)

// TestUploadFileOffsetSemantics covers the resumable upload contract: the
// client offset must equal the staged size, mismatches return an
// OffsetError carrying the current size, and matching segments stream into
// the staging area.
func TestUploadFileOffsetSemantics(t *testing.T) {
	env := newTestEnv(t)
	taskID := env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, token := env.claim(t, "w1")
	if claimed != taskID {
		t.Fatalf("claimed %s", claimed)
	}
	name := env.fs.StagingPath(claimed, "pkg.pkg.tar.zst")

	// First segment at offset 0.
	meta, err := env.o.UploadFile(ctx(), claimed, token, "pkg.pkg.tar.zst", strings.NewReader("AAAA"), 4, 0)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if meta.Offset != 4 {
		t.Errorf("offset = %d, want 4", meta.Offset)
	}
	// Resumed segment.
	meta, err = env.o.UploadFile(ctx(), claimed, token, "pkg.pkg.tar.zst", strings.NewReader("BBBB"), 4, 4)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if meta.Offset != 8 {
		t.Errorf("offset = %d, want 8", meta.Offset)
	}
	data, err := env.fs.GetBytes(ctx(), name)
	if err != nil {
		t.Fatalf("staged content: %v", err)
	}
	if string(data) != "AAAABBBB" {
		t.Errorf("staged = %q, want AAAABBBB", data)
	}
	// Mismatched offset → OffsetError with the current size.
	_, err = env.o.UploadFile(ctx(), claimed, token, "pkg.pkg.tar.zst", strings.NewReader("X"), 1, 3)
	var offErr *OffsetError
	if !errors.As(err, &offErr) || !errors.Is(err, ErrConflict) {
		t.Fatalf("UploadFile mismatch = %v, want OffsetError(ErrConflict)", err)
	}
	if offErr.Current != 8 {
		t.Errorf("reported offset = %d, want 8", offErr.Current)
	}
	// Bad token.
	if _, err := env.o.UploadFile(ctx(), claimed, "nope", "x", strings.NewReader(""), 0, 8); !errors.Is(err, ErrForbidden) {
		t.Errorf("UploadFile bad token = %v, want ErrForbidden", err)
	}
}

// TestCustomStagingDirChain drives the consumer path end to end against a
// real local backend whose staging tree lives in a configured directory
// outside the repository root: UploadFile stages into the custom tree,
// DownloadFile reads it back, and cleanupStaging removes the task
// directory under it.
func TestCustomStagingDirChain(t *testing.T) {
	env := newTestEnv(t)
	root := t.TempDir()
	staging := t.TempDir() // absolute staging dir outside the root
	backend, err := storage.OpenLocal(root, staging)
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	env.o.storage = backend

	const name = "pkg.pkg.tar.zst"
	taskID := env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, token := env.claim(t, "w1")
	if claimed != taskID {
		t.Fatalf("claimed %s, want %s", claimed, taskID)
	}

	// Upload a first segment; the bytes must land under the custom tree.
	meta, err := env.o.UploadFile(ctx(), claimed, token, name, strings.NewReader("AAAA"), 4, 0)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if meta.Offset != 4 {
		t.Errorf("offset = %d, want 4", meta.Offset)
	}
	physical := filepath.Join(staging, claimed, name)
	if _, err := os.Stat(physical); err != nil {
		t.Fatalf("staged file not under custom dir %s: %v", staging, err)
	}

	// DownloadFile streams the staged content back.
	rc, err := env.o.DownloadFile(ctx(), claimed, token, name)
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read download: %v", err)
	}
	if string(data) != "AAAA" {
		t.Errorf("download = %q, want %q", data, "AAAA")
	}

	// cleanupStaging removes the staged files and the task directory.
	env.o.cleanupStaging(ctx(), claimed, []string{name})
	if _, err := os.Stat(physical); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staged file not cleaned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staging, claimed)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("task staging dir not cleaned: %v", err)
	}
	// The repository root stays empty.
	if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("artifact leaked into root: %v", err)
	}
}

// TestDownloadFile covers the staged file read path and the not-found case.
func TestDownloadFile(t *testing.T) {
	env := newTestEnv(t)
	env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, token := env.claim(t, "w1")
	if _, err := env.o.UploadFile(ctx(), claimed, token, "art.txt", strings.NewReader("payload"), 7, 0); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	rc, err := env.o.DownloadFile(ctx(), claimed, token, "art.txt")
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("downloaded = %q, want payload", got)
	}
	if _, err := env.o.DownloadFile(ctx(), claimed, token, "missing.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DownloadFile(missing) = %v, want ErrNotFound", err)
	}
}

// TestIssueSigningKeyOneShot covers the per-task one-shot key material:
// the first claim succeeds and a second returns sign.ErrAlreadyExported.
func TestIssueSigningKeyOneShot(t *testing.T) {
	env := newTestEnv(t)
	env.enqueue(t, "foo", "foo")
	env.registerWorker(t, "w1", "host", "host", 1)
	claimed, token := env.claim(t, "w1")

	km, err := env.o.IssueSigningKey(ctx(), claimed, token)
	if err != nil {
		t.Fatalf("IssueSigningKey: %v", err)
	}
	if km.KeyID != "ABCD" || km.Passphrase == "" {
		t.Errorf("key material = %+v", km)
	}
	if _, err := env.o.IssueSigningKey(ctx(), claimed, token); !errors.Is(err, sign.ErrAlreadyExported) {
		t.Errorf("second claim = %v, want sign.ErrAlreadyExported", err)
	}
	if _, err := env.o.IssueSigningKey(ctx(), claimed, "nope"); !errors.Is(err, ErrForbidden) {
		t.Errorf("bad token = %v, want ErrForbidden", err)
	}
	// The key material is cleared when the task reaches a terminal state.
	env.up.ingestErr = errors.New("boom")
	if err := env.reportSucceeded(t, claimed, token, testArtifacts("foo", "1.0-1"), ""); err != nil {
		t.Fatalf("ReportResult: %v", err)
	}
	if len(env.sig.cleared) == 0 || env.sig.cleared[0] != claimed {
		t.Errorf("signer not cleared: %v", env.sig.cleared)
	}
}
