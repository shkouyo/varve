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

package repo

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestS3RepoUpdateFlow asserts the s3 work dir flow: the database and the
// new packages are downloaded, the commands run with cwd = work dir, the
// regenerated database (in both forms) and packages are uploaded back and
// the work dir is cleared afterwards.
func TestS3RepoUpdateFlow(t *testing.T) {
	e := newIngestEnv(t, "s3", execCfg{})
	workDir := e.cfg.Repo.WorkDir
	// The bucket already holds a repository database.
	e.seedRoot(testDBName, "db-content")
	e.stage(testManifest())

	if err := e.upd.Ingest(context.Background(), e.task, e.build, "w", testManifest()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// Download sequence: database, then the new package file (the missing
	// .sig is skipped), all before the local commands.
	if i := e.logIndex("get " + testDBName); i < 0 {
		t.Error("missing database download")
	}
	if i := e.logIndex("get " + testPkgFile); i < 0 {
		t.Error("missing package download")
	}

	// The commands run with cwd = work dir.
	execs := e.execLines()
	if len(execs) != 1 {
		t.Fatalf("exec lines = %d, want 1: %v", len(execs), execs)
	}
	if !strings.Contains(execs[0], "repo-add "+workDir+" "+testDBArchive+" "+testPkgFile) {
		t.Errorf("repo-add must run in the work dir, got %q", execs[0])
	}
	if i := e.logIndex("exec repo-add " + workDir); i < 0 {
		t.Error("missing local repo-add")
	}

	// Upload sequence: the database (archive and pacman-facing name) and
	// the packages are put back after the commands ran.
	addIdx := e.logIndex("exec repo-add " + workDir)
	for _, name := range []string{testDBName, testDBArchive, testPkgFile} {
		if i := e.logIndex("put " + name); i < 0 || i < addIdx {
			t.Errorf("upload-back %s (idx %d) must follow repo-add (idx %d)", name, i, addIdx)
		}
	}

	// The bucket holds both forms of the database and the file database;
	// the pacman-facing objects carry the archive bytes (the fake repo-add
	// regenerated the archive after the download).
	for _, name := range []string{testDBName, testDBArchive, testFilesName, testFilesArchive} {
		if _, ok := e.fs.files[name]; !ok {
			t.Errorf("bucket missing %s", name)
		}
	}
	if !bytes.Equal(e.fs.files[testDBName], e.fs.files[testDBArchive]) {
		t.Errorf("%s content differs from %s", testDBName, testDBArchive)
	}
	if !bytes.Equal(e.fs.files[testFilesName], e.fs.files[testFilesArchive]) {
		t.Errorf("%s content differs from %s", testFilesName, testFilesArchive)
	}

	// The work dir is cleared.
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Errorf("work dir %s still present after ingest (err=%v)", workDir, err)
	}
}

// TestS3RepoUpdateDownloadArchiveFallback asserts the database download
// accepts the <name>.db.tar.gz object from an older install when the
// canonical <name>.db object is missing.
func TestS3RepoUpdateDownloadArchiveFallback(t *testing.T) {
	e := newIngestEnv(t, "s3", execCfg{})
	// Only the gzip archive object exists in the bucket.
	e.seedRoot(testDBArchive, "old-db-content")
	e.stage(testManifest())

	if err := e.upd.Ingest(context.Background(), e.task, e.build, "w", testManifest()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// The archive object was fetched and the ingest converged to the same
	// four database objects.
	if i := e.logIndex("get " + testDBArchive); i < 0 {
		t.Error("missing archive database download")
	}
	for _, name := range []string{testDBName, testDBArchive, testFilesName, testFilesArchive} {
		if _, ok := e.fs.files[name]; !ok {
			t.Errorf("bucket missing %s", name)
		}
	}
}

// TestS3RepoUpdateSignedDualSig asserts that with repo.sign ==
// "packages+db" the regenerated signatures are uploaded in both forms:
// <name>.db.sig / <name>.files.sig (pacman-facing) and the matching
// .tar.gz.sig archive twins, with identical bytes.
func TestS3RepoUpdateSignedDualSig(t *testing.T) {
	e := newIngestEnv(t, "s3", execCfg{})
	e.cfg.Repo.Sign = "packages+db"
	e.seedRoot(testDBName, "db-content")
	e.stage(testManifest())

	if err := e.upd.Ingest(context.Background(), e.task, e.build, "w", testManifest()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	for _, name := range []string{
		testDBName + ".sig", testDBArchive + ".sig",
		testFilesName + ".sig", testFilesArchive + ".sig",
	} {
		if _, ok := e.fs.files[name]; !ok {
			t.Errorf("bucket missing signature %s", name)
		}
	}
	if !bytes.Equal(e.fs.files[testDBName+".sig"], e.fs.files[testDBArchive+".sig"]) {
		t.Errorf("%s.sig content differs from %s.sig", testDBName, testDBArchive)
	}
	if !bytes.Equal(e.fs.files[testFilesName+".sig"], e.fs.files[testFilesArchive+".sig"]) {
		t.Errorf("%s.sig content differs from %s.sig", testFilesName, testFilesArchive)
	}
}

// TestS3WorkDirCleanupFailureWarns asserts a failed work dir cleanup only
// warns and does not block the ingest; stale files are overwritten by the
// next ingest.
func TestS3WorkDirCleanupFailureWarns(t *testing.T) {
	if os.Getenv("VARVE_TEST_SKIP_PERM") != "" {
		t.Skip("VARVE_TEST_SKIP_PERM set: permission-dependent behavior is not exercised")
	}
	if os.Geteuid() == 0 {
		t.Skip("permission-based cleanup failure cannot be provoked as root")
	}
	e := newIngestEnv(t, "s3", execCfg{})
	workDir := e.cfg.Repo.WorkDir
	if err := os.MkdirAll(filepath.Join(workDir, "blocked"), 0o755); err != nil {
		t.Fatalf("mkdir blocked: %v", err)
	}
	if err := os.Chmod(filepath.Join(workDir, "blocked"), 0o000); err != nil {
		t.Fatalf("chmod blocked: %v", err)
	}
	t.Cleanup(func() {
		os.Chmod(filepath.Join(workDir, "blocked"), 0o700)
		os.RemoveAll(workDir)
	})

	e.seedRoot(testDBName, "db-content")
	e.stage(testManifest())
	if err := e.upd.Ingest(context.Background(), e.task, e.build, "w", testManifest()); err != nil {
		t.Fatalf("Ingest with failing work dir cleanup: %v", err)
	}
}
