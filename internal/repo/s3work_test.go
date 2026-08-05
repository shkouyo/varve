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

package repo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestS3RepoUpdateFlow asserts the s3 work dir flow (DETAIL §6.7 case 5):
// the database and the new packages are downloaded, the commands run with
// cwd = work dir, the regenerated database and packages are uploaded back
// and the work dir is cleared afterwards.
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
	if !strings.Contains(execs[0], "repo-add "+workDir+" "+testDBName+" "+testPkgFile) {
		t.Errorf("repo-add must run in the work dir, got %q", execs[0])
	}
	if i := e.logIndex("exec repo-add " + workDir); i < 0 {
		t.Error("missing local repo-add")
	}

	// Upload sequence: the database and the packages are put back after the
	// commands ran.
	addIdx := e.logIndex("exec repo-add " + workDir)
	if i := e.logIndex("put " + testDBName); i < 0 || i < addIdx {
		t.Errorf("database upload-back (idx %d) must follow repo-add (idx %d)", i, addIdx)
	}
	if i := e.logIndex("put " + testPkgFile); i < 0 || i < addIdx {
		t.Errorf("package upload-back (idx %d) must follow repo-add (idx %d)", i, addIdx)
	}

	// The work dir is cleared.
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Errorf("work dir %s still present after ingest (err=%v)", workDir, err)
	}
}

// TestS3WorkDirCleanupFailureWarns asserts a failed work dir cleanup only
// warns and does not block the ingest (DETAIL §6.5); stale files are
// overwritten by the next ingest.
func TestS3WorkDirCleanupFailureWarns(t *testing.T) {
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
