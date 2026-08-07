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

package detect

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/detect/srcinfo"
)

// fakeSink records submitted changes and can be told to fail the first
// Submit (conflict simulation).
type fakeSink struct {
	mu      sync.Mutex
	changes []Change
	err     error // returned by every Submit when non-nil
	errOnce bool  // fail exactly the next Submit
}

// Submit implements Sink.
func (f *fakeSink) Submit(_ context.Context, c Change) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errOnce {
		f.errOnce = false
		e := f.err
		f.err = nil // the one-shot failure is consumed
		return e
	}
	if f.err != nil {
		return f.err
	}
	f.changes = append(f.changes, c)
	return nil
}

// snapshot returns a copy of the recorded changes.
func (f *fakeSink) snapshot() []Change {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Change(nil), f.changes...)
}

// fakeExecScript builds a fake exec.Command constructor backed by a real
// shell script. The script receives the intended command name in $1 and
// its arguments in $2... and may emit canned output, record calls or
// fail.
func fakeExecScript(t *testing.T, body string) func(ctx context.Context, name string, arg ...string) *exec.Cmd {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakeexec")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake exec script: %v", err)
	}
	return func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, path, append([]string{name}, arg...)...)
		return cmd
	}
}

// openStore opens a fresh file-backed database for one test and returns
// the store plus the database path (used to seed packages rows directly).
func openStore(t *testing.T) (*db.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "varve.db")
	s, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// seedPackageRow inserts one packages row with the given last successful
// build records. detect never writes the database itself; tests seed it
// the same way a successful build would have updated it.
func seedPackageRow(t *testing.T, dbPath, pkgbase, srcinfoHash, upstreamRef string) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`INSERT INTO packages
		(pkgbase, branch, vcs_kind, arch, enabled, current_version, pkgdesc,
		 last_srcinfo_hash, last_upstream_ref, maintainers)
		VALUES (?, '', '', 'x86_64', 1, '', '', ?, ?, '[]')`,
		pkgbase, srcinfoHash, upstreamRef); err != nil {
		t.Fatalf("seed package %s: %v", pkgbase, err)
	}
}

// newTestDetector builds a Detector for one test with a temp mirror root,
// a quiet logger and the default (real) execCommand. Callers override
// execCommand, sink or cfg as needed.
func newTestDetector(t *testing.T, url string, store *db.Store, sink Sink) *Detector {
	t.Helper()
	cfg := &config.SourceConfig{URL: url, PollInterval: time.Hour}
	d, err := newDetector(cfg, store, sink, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("newDetector: %v", err)
	}
	d.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return d
}

// withShimPath prepends the package testdata/bin directory to PATH so the
// vcs subpackage resolves git/svn through the PATH shims (the vcs
// execCommand var is not replaceable from outside that package).
func withShimPath(t *testing.T) {
	t.Helper()
	shimDir, err := filepath.Abs(filepath.Join("testdata", "bin"))
	if err != nil {
		t.Fatalf("abs shim dir: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// --- git helpers (tests use real git against local file:// repos only) ---

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFiles(t *testing.T, repo string, files map[string]string, msg string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		writeFile(t, p, content)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", msg)
}

// branchSpec is one branch of a multi-branch source repository.
type branchSpec struct {
	name  string
	files map[string]string
}

// newSourceRepo creates a file:// source repository with one branch.
func newSourceRepo(t *testing.T, branch string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	runGit(t, dir, "init", "-b", branch)
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.org")
	commitFiles(t, dir, files, "init")
	return dir
}

// newMultiBranchRepo creates a file:// source repository whose first
// branch is the initial one and whose remaining branches branch off it.
func newMultiBranchRepo(t *testing.T, branches []branchSpec) string {
	t.Helper()
	if len(branches) == 0 {
		t.Fatal("newMultiBranchRepo requires at least one branch")
	}
	dir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	runGit(t, dir, "init", "-b", branches[0].name)
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.org")
	for i, b := range branches {
		if i > 0 {
			runGit(t, dir, "checkout", "-b", b.name)
			// Isolate the branch: drop the parent branch's files so each
			// branch only contains its own spec.
			runGit(t, dir, "rm", "-rfq", "--ignore-unmatch", ".")
		}
		commitFiles(t, dir, b.files, "branch "+b.name)
	}
	return dir
}

// cloneMirror mirrors src into a fresh bare repository and returns the
// mirror directory.
func cloneMirror(t *testing.T, src string) string {
	t.Helper()
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	runGit(t, filepath.Dir(mirror), "clone", "--mirror", "file://"+src, mirror)
	return mirror
}

// readLines reads a record file into trimmed lines.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// countLines returns the number of lines in a record file that contain
// needle.
func countLines(t *testing.T, path, needle string) int {
	t.Helper()
	n := 0
	for _, line := range readLines(t, path) {
		if strings.Contains(line, needle) {
			n++
		}
	}
	return n
}

// srcinfoBody renders a minimal .SRCINFO for pkgbase.
func srcinfoBody(pkgbase, pkgver, pkgrel string) string {
	return "pkgbase = " + pkgbase + "\n" +
		"\tpkgdesc = test package\n" +
		"\tpkgver = " + pkgver + "\n" +
		"\tpkgrel = " + pkgrel + "\n" +
		"\tarch = x86_64\n" +
		"pkgname = " + pkgbase + "\n"
}

// srcinfoWithSource renders a .SRCINFO carrying one source entry.
func srcinfoWithSource(pkgbase, pkgver, pkgrel, source string) string {
	return "pkgbase = " + pkgbase + "\n" +
		"\tpkgdesc = test package\n" +
		"\tpkgver = " + pkgver + "\n" +
		"\tpkgrel = " + pkgrel + "\n" +
		"\tarch = x86_64\n" +
		"\tsource = " + source + "\n" +
		"pkgname = " + pkgbase + "\n"
}

// hashOf computes the srcinfo hash of a file content string, matching what
// the pipeline computes on the raw bytes.
func hashOf(content string) string {
	return srcinfo.Hash([]byte(content))
}
