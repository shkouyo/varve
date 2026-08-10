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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/storage"
)

// ---------------------------------------------------------------------------
// Shared test doubles: fakeStorage records every operation in one ordered
// shared log; the fake execCommand runs this test binary as a programmable
// helper process that appends to the same log.
// ---------------------------------------------------------------------------

// opLog is an append-only line log shared between the in-process fakeStorage
// and the child processes of the fake exec command.
type opLog struct {
	mu   sync.Mutex
	path string
}

func newOpLog(t *testing.T) *opLog {
	return &opLog{path: filepath.Join(t.TempDir(), "ops.log")}
}

func (l *opLog) add(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	f.WriteString(line + "\n")
	f.Close()
}

// read returns the recorded operation lines in order.
func (l *opLog) read() []string {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// fakeStorage is an in-memory Backend + Mover that records its call order in
// the shared opLog. moveErr programs a failing Move (source missing or
// arbitrary backend error).
type fakeStorage struct {
	mu      sync.Mutex
	log     *opLog
	files   map[string][]byte
	moveErr error
}

func newFakeStorage(log *opLog) *fakeStorage {
	return &fakeStorage{log: log, files: make(map[string][]byte)}
}

func (f *fakeStorage) Put(ctx context.Context, name string, r io.Reader, size int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.files[name] = data
	f.mu.Unlock()
	f.log.add(fmt.Sprintf("put %s %d", name, len(data)))
	return nil
}

func (f *fakeStorage) Get(ctx context.Context, name string, w io.Writer) error {
	f.mu.Lock()
	data, ok := f.files[name]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("storage: get %q: %w", name, storage.ErrNotFound)
	}
	f.log.add("get " + name)
	_, err := w.Write(data)
	return err
}

func (f *fakeStorage) Delete(ctx context.Context, name string) error {
	f.mu.Lock()
	delete(f.files, name)
	f.mu.Unlock()
	f.log.add("delete " + name)
	return nil
}

// StagingPath returns the default virtual staging path of a task artifact.
func (f *fakeStorage) StagingPath(taskID, fileName string) string {
	return "staging/" + taskID + "/" + fileName
}

// StagingDir returns "" because the fake has no physical staging tree.
func (f *fakeStorage) StagingDir() string { return "" }

func (f *fakeStorage) List(ctx context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for name := range f.files {
		if strings.HasPrefix(name, "staging/") {
			continue
		}
		ok, err := path.Match(prefix, name)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, name)
		}
	}
	return out, nil
}

func (f *fakeStorage) Stat(ctx context.Context, name string) (storage.FileInfo, error) {
	f.mu.Lock()
	data, ok := f.files[name]
	f.mu.Unlock()
	if !ok {
		return storage.FileInfo{}, fmt.Errorf("storage: stat %q: %w", name, storage.ErrNotFound)
	}
	f.log.add("stat " + name)
	return storage.FileInfo{Size: int64(len(data)), ModTime: time.Time{}}, nil
}

func (f *fakeStorage) Move(ctx context.Context, src, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.moveErr != nil {
		return f.moveErr
	}
	data, ok := f.files[src]
	if !ok {
		return fmt.Errorf("storage: move %q -> %q: source missing", src, dst)
	}
	f.files[dst] = data
	delete(f.files, src)
	f.log.add(fmt.Sprintf("move %s %s", src, dst))
	return nil
}

// backendWithoutMover hides the Mover capability behind the plain Backend
// interface, exercising the degraded Get+Put+Delete path.
type backendWithoutMover struct {
	storage.Backend
}

// fakeSigner returns a fixed GNUPGHOME fragment.
type fakeSigner struct{}

func (fakeSigner) GnuPGEnv() []string { return []string{"GNUPGHOME=/data/gnupg"} }

// execCfg programs the fake exec: per-program exit codes and stderr output.
type execCfg struct {
	exits  map[string]int
	stderr map[string]string
}

// fakeExecFor returns an execCommand replacement that runs this test binary
// as the programmable helper process (same-package tests replace execCommand
// with a recorder; the canonical re-exec pattern keeps the fake
// self-contained without external tooling).
func fakeExecFor(log *opLog, cfg execCfg) func(context.Context, string, ...string) *exec.Cmd {
	return func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		args := []string{"-test.run=TestHelperProcess", "--", name}
		args = append(args, arg...)
		cmd := exec.CommandContext(ctx, os.Args[0], args...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"FAKE_EXEC_LOG="+log.path,
		)
		key := execEnvKey(name)
		if code, ok := cfg.exits[name]; ok {
			cmd.Env = append(cmd.Env, "FAKE_EXEC_EXIT_"+key+"="+strconv.Itoa(code))
		}
		if s, ok := cfg.stderr[name]; ok {
			cmd.Env = append(cmd.Env, "FAKE_EXEC_STDERR_"+key+"="+s)
		}
		return cmd
	}
}

// execEnvKey sanitizes a program name for use in an environment variable.
func execEnvKey(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return '_'
	}, name)
}

// TestHelperProcess is the re-exec helper: when invoked by fakeExecFor it
// acts as the fake external command, otherwise it is a no-op test.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	os.Exit(helperMain())
}

func helperMain() int {
	rest := os.Args
	for i, a := range os.Args {
		if a == "--" {
			rest = os.Args[i+1:]
			break
		}
	}
	if len(rest) == 0 {
		return 2
	}
	prog := filepath.Base(rest[0])
	if lp := os.Getenv("FAKE_EXEC_LOG"); lp != "" {
		dir, err := os.Getwd()
		if err != nil {
			dir = "<unknown>"
		}
		lines := "exec " + prog + " " + dir + " " + strings.Join(rest[1:], " ") + "\n"
		// Record the signing environment so tests can assert the
		// GNUPGHOME injection reached the child process.
		if gnupg := os.Getenv("GNUPGHOME"); gnupg != "" {
			lines += "env GNUPGHOME=" + gnupg + "\n"
		}
		if f, err := os.OpenFile(lp, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			f.WriteString(lines)
			f.Close()
		}
	}
	// Emulate the repo database commands: like repo-add / repo-remove,
	// leave the gzip archives, the pacman-facing symlinks and (with
	// --sign) the detached signatures in the working directory.
	if prog == "repo-add" || prog == "repo-remove" {
		emulateRepoDB(rest[1:])
	}

	key := execEnvKey(prog)
	if s := os.Getenv("FAKE_EXEC_STDERR_" + key); s != "" {
		fmt.Fprint(os.Stderr, s)
	}
	if e := os.Getenv("FAKE_EXEC_EXIT_" + key); e != "" {
		if c, err := strconv.Atoi(e); err == nil {
			return c
		}
	}
	return 0
}

// emulateRepoDB mirrors the output repo-add / repo-remove leave in their
// working directory: the <name>.db.tar.gz gzip archive, the <name>.db
// symlink, the <name>.files.tar.gz / <name>.files pair and, with --sign,
// the matching .sig files. The archive carries fixed bytes so tests can
// assert the pacman-facing object holds identical content.
func emulateRepoDB(args []string) {
	var archive string
	sign := false
	for _, a := range args {
		if a == "--sign" {
			sign = true
			continue
		}
		if archive == "" && strings.HasSuffix(a, ".db.tar.gz") {
			archive = a
		}
	}
	if archive == "" {
		return
	}
	name := strings.TrimSuffix(filepath.Base(archive), ".db.tar.gz")
	pairs := [][2]string{
		{name + ".db", name + ".db.tar.gz"},
		{name + ".files", name + ".files.tar.gz"},
	}
	content := []byte("db-bytes")
	for _, p := range pairs {
		if err := os.WriteFile(p[1], content, 0o644); err != nil {
			return
		}
		os.Remove(p[0])
		if err := os.Symlink(p[1], p[0]); err != nil {
			return
		}
	}
	if !sign {
		return
	}
	for _, p := range pairs {
		if err := os.WriteFile(p[1]+".sig", []byte("sig-bytes"), 0o644); err != nil {
			return
		}
		os.Remove(p[0] + ".sig")
		if err := os.Symlink(p[1]+".sig", p[0]+".sig"); err != nil {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Ingest environment helpers
// ---------------------------------------------------------------------------

const (
	testPkgbase = "foo"
	testPkgFile = "foo-1.2.3-1-x86_64.pkg.tar.zst"
	testSigFile = "foo-1.2.3-1-x86_64.pkg.tar.zst.sig"
	testSrcinfo = ".SRCINFO"
	testTaskID  = "task-1"
	// testDBName / testFilesName are the canonical pacman-facing database
	// names; the *Archive variants are the gzip archives repo-add derives
	// them from.
	testDBName       = "varve.db"
	testDBArchive    = "varve.db.tar.gz"
	testFilesName    = "varve.files"
	testFilesArchive = "varve.files.tar.gz"
)

// testManifest returns the standard manifest used by most cases: one package,
// its detached signature and the .SRCINFO snapshot.
func testManifest() []Artifact {
	return []Artifact{
		{File: testPkgFile, Kind: "package", Pkgname: testPkgbase, Version: "1.2.3-1", Arch: "x86_64", Size: 100, SHA256: "s1"},
		{File: testSigFile, Kind: "signature", Size: 50, SHA256: "s2"},
		{File: testSrcinfo, Kind: "srcinfo", Size: 30, SHA256: "s3"},
	}
}

func srcinfoText(pkgbase string) []byte {
	return []byte("# generated by makepkg\npkgbase = " + pkgbase + "\npkgver = 1.2.3\npkgname = " + pkgbase + "\n")
}

// ingestEnv bundles the fake world of one test.
type ingestEnv struct {
	t     *testing.T
	cfg   *config.ControllerConfig
	fs    *fakeStorage
	upd   *updater
	log   *opLog
	task  *db.Task
	build *db.Build
	now   time.Time
	root  string
}

// newIngestEnv builds the updater under test. backend is "local" or "s3".
func newIngestEnv(t *testing.T, backend string, cfg execCfg) *ingestEnv {
	e := &ingestEnv{t: t}
	e.log = newOpLog(t)
	e.root = t.TempDir()
	if err := os.MkdirAll(e.root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	work := filepath.Join(t.TempDir(), "work")
	e.cfg = &config.ControllerConfig{
		Storage: config.StorageConfig{
			Backend: backend,
			Local:   config.LocalConfig{Root: e.root},
		},
		Repo: config.RepoConfig{Name: "varve", WorkDir: work, Sign: "off", KeepVersions: 1},
	}
	e.fs = newFakeStorage(e.log)
	e.now = time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	e.upd = NewUpdater(e.cfg, e.fs, fakeSigner{}, func() time.Time { return e.now })
	e.upd.execCommand = fakeExecFor(e.log, cfg)
	e.task = &db.Task{ID: testTaskID}
	e.build = &db.Build{Branch: "foo", Commit: "commit-new", UpstreamRef: "ref-9"}
	return e
}

// stage seeds the staging area with the manifest files.
func (e *ingestEnv) stage(manifest []Artifact) {
	for _, a := range manifest {
		data := []byte("content-" + a.File)
		if a.Kind == "srcinfo" {
			data = srcinfoText(testPkgbase)
		}
		e.fs.files[e.fs.StagingPath(e.task.ID, a.File)] = data
	}
}

// seedSidecar writes an old side file into the repository root.
func (e *ingestEnv) seedSidecar(sc *Sidecar) {
	data, err := MarshalSidecar(sc)
	if err != nil {
		e.t.Fatalf("seed sidecar: %v", err)
	}
	e.fs.files[sc.Pkgbase+".meta.toml"] = data
}

// seedRoot places a file directly into the repository root.
func (e *ingestEnv) seedRoot(name, content string) {
	e.fs.files[name] = []byte(content)
}

// storedSidecar reads and parses the written side file of pkgbase.
func (e *ingestEnv) storedSidecar(pkgbase string) *Sidecar {
	data, ok := e.fs.files[pkgbase+".meta.toml"]
	if !ok {
		e.t.Fatalf("sidecar %s.meta.toml not written", pkgbase)
	}
	sc, err := ParseSidecar(data)
	if err != nil {
		e.t.Fatalf("parse written sidecar: %v", err)
	}
	return sc
}

// logHas reports whether any recorded line contains substr.
func (e *ingestEnv) logHas(substr string) bool {
	for _, line := range e.log.read() {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// logIndex returns the index of the first line containing substr, or -1.
func (e *ingestEnv) logIndex(substr string) int {
	for i, line := range e.log.read() {
		if strings.Contains(line, substr) {
			return i
		}
	}
	return -1
}

// execLines returns the recorded external command lines in order.
func (e *ingestEnv) execLines() []string {
	var out []string
	for _, line := range e.log.read() {
		if strings.HasPrefix(line, "exec ") {
			out = append(out, line)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Ingest orchestration tests
// ---------------------------------------------------------------------------

// TestIngestMoveSequence asserts the move step: every package and signature
// entry moves from staging into the flat root, the .SRCINFO snapshot stays
// in staging (only its hash is recorded), and the side file plus the
// repo-add command follow.
func TestIngestMoveSequence(t *testing.T) {
	e := newIngestEnv(t, "local", execCfg{})
	e.stage(testManifest())
	if err := e.upd.Ingest(context.Background(), e.task, e.build, "proud-heron-7", testManifest()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// The staged source is gone, the root targets exist.
	for _, name := range []string{testPkgFile, testSigFile} {
		if _, ok := e.fs.files[e.fs.StagingPath(testTaskID, name)]; ok {
			t.Errorf("staging %s still present after move", name)
		}
		if _, ok := e.fs.files[name]; !ok {
			t.Errorf("root %s missing after move", name)
		}
	}
	// .SRCINFO stays in staging (not persisted).
	if _, ok := e.fs.files[e.fs.StagingPath(testTaskID, testSrcinfo)]; !ok {
		t.Error(".SRCINFO was moved out of staging; it must stay for the caller's staging cleanup")
	}

	// Relative order: pkgbase resolution -> moves -> sidecar -> repo-add.
	if i := e.logIndex("get staging/" + testTaskID + "/" + testSrcinfo); i < 0 {
		t.Error("missing staging .SRCINFO read (pkgbase resolution)")
	}
	if i := e.logIndex("move staging/" + testTaskID + "/" + testPkgFile + " " + testPkgFile); i < 0 {
		t.Error("missing package move")
	}
	if i := e.logIndex("move staging/" + testTaskID + "/" + testSigFile + " " + testSigFile); i < 0 {
		t.Error("missing signature move")
	}
	side := e.logIndex("put " + testPkgbase + ".meta.toml")
	exec := e.logIndex("exec repo-add " + e.root)
	if side < 0 || exec < 0 || side > exec {
		t.Errorf("sidecar write (idx %d) must precede repo-add (idx %d)", side, exec)
	}
	execs := e.execLines()
	if len(execs) != 1 {
		t.Fatalf("exec lines = %d, want 1: %v", len(execs), execs)
	}
	wantArgs := testDBArchive + " " + testPkgFile
	if !strings.Contains(execs[0], wantArgs) {
		t.Errorf("repo-add args = %q, want %q", execs[0], wantArgs)
	}
}

// TestIngestSidecarContent asserts the side file carries every field:
// pkgbase from the uploaded .SRCINFO, branch and resolved commit / upstream
// ref from the build record, srcinfo hash from the manifest, the injected
// ingest time and the worker name.
func TestIngestSidecarContent(t *testing.T) {
	cases := []struct {
		name    string
		pkgbase string
		pkgFile string
		vcsWant string
		seedOld *Sidecar
	}{
		{
			name:    "first-ingest-plain",
			pkgbase: "bar",
			pkgFile: "bar-1-1-x86_64.pkg.tar.zst",
			vcsWant: "",
		},
		{
			name:    "first-ingest-git-suffix",
			pkgbase: "baz-git",
			pkgFile: "baz-git-2-1-x86_64.pkg.tar.zst",
			vcsWant: "git",
		},
		{
			name:    "repeat-ingest-keeps-old-vcs",
			pkgbase: "qux",
			pkgFile: "qux-3-1-x86_64.pkg.tar.zst",
			vcsWant: "svn",
			seedOld: &Sidecar{Pkgbase: "qux", Branch: "qux", VCS: "svn",
				Artifacts: []Artifact{{File: "qux-2-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "qux"}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newIngestEnv(t, "local", execCfg{})
			if tc.seedOld != nil {
				e.seedSidecar(tc.seedOld)
			}
			m := []Artifact{
				{File: tc.pkgFile, Kind: "package", Pkgname: tc.pkgbase, Version: "1-1", Arch: "x86_64", Size: 1, SHA256: "h1"},
				{File: testSrcinfo, Kind: "srcinfo", Size: 1, SHA256: "h2"},
			}
			e.fs.files[e.fs.StagingPath(testTaskID, tc.pkgFile)] = []byte("pkg")
			e.fs.files[e.fs.StagingPath(testTaskID, testSrcinfo)] = srcinfoText(tc.pkgbase)
			e.build.Branch = tc.pkgbase

			if err := e.upd.Ingest(context.Background(), e.task, e.build, "worker-a", m); err != nil {
				t.Fatalf("Ingest: %v", err)
			}
			sc := e.storedSidecar(tc.pkgbase)
			if sc.Pkgbase != tc.pkgbase {
				t.Errorf("Pkgbase = %q, want %q", sc.Pkgbase, tc.pkgbase)
			}
			if sc.Branch != tc.pkgbase {
				t.Errorf("Branch = %q, want %q", sc.Branch, tc.pkgbase)
			}
			if sc.VCS != tc.vcsWant {
				t.Errorf("VCS = %q, want %q", sc.VCS, tc.vcsWant)
			}
			if !reflect.DeepEqual(sc.Artifacts, m) {
				t.Errorf("Artifacts mismatch:\n got %+v\nwant %+v", sc.Artifacts, m)
			}
			b := sc.Build
			if b.Commit != "commit-new" || b.UpstreamRef != "ref-9" || b.SrcinfoHash != "h2" || b.Worker != "worker-a" {
				t.Errorf("Build = %+v", b)
			}
			if !b.Time.Equal(e.now) {
				t.Errorf("Time = %v, want %v", b.Time, e.now)
			}
		})
	}
}

// TestIngestOldVersionCleanup asserts the keep_versions=1 pruning: old
// package files and their detached signatures are deleted, files of the new
// manifest survive, and the replaced pkgname is repo-removed before the new
// packages are added.
func TestIngestOldVersionCleanup(t *testing.T) {
	e := newIngestEnv(t, "local", execCfg{})
	oldPkg := "foo-old-1-1-x86_64.pkg.tar.zst"
	e.seedSidecar(&Sidecar{
		Pkgbase: testPkgbase,
		Branch:  testPkgbase,
		VCS:     "",
		Artifacts: []Artifact{
			{File: oldPkg, Kind: "package", Pkgname: "foo-old", Version: "1-1", Arch: "x86_64"},
			{File: oldPkg + ".sig", Kind: "signature"},
		},
	})
	// The previous build's files are present in the root.
	e.seedRoot(oldPkg, "old-pkg")
	e.seedRoot(oldPkg+".sig", "old-sig")

	m := testManifest()
	e.stage(m)
	if err := e.upd.Ingest(context.Background(), e.task, e.build, "w", m); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if _, ok := e.fs.files[oldPkg]; ok {
		t.Error("old package file still present")
	}
	if _, ok := e.fs.files[oldPkg+".sig"]; ok {
		t.Error("old signature still present")
	}
	for _, name := range []string{testPkgFile, testSigFile} {
		if _, ok := e.fs.files[name]; !ok {
			t.Errorf("new file %s was deleted", name)
		}
	}
	// remove (replaced pkgname) must precede add.
	rem := e.logIndex("exec repo-remove " + e.root + " " + testDBArchive + " foo-old")
	add := e.logIndex("exec repo-add " + e.root)
	if rem < 0 {
		t.Error("missing repo-remove for replaced pkgname foo-old")
	}
	if add < 0 {
		t.Error("missing repo-add")
	}
	if rem >= add {
		t.Errorf("repo-remove (idx %d) must precede repo-add (idx %d)", rem, add)
	}
}

// TestIngestOldSidecarCorrupt asserts a damaged previous side file only
// warns and is treated as "no previous version".
func TestIngestOldSidecarCorrupt(t *testing.T) {
	e := newIngestEnv(t, "local", execCfg{})
	e.seedRoot(testPkgbase+".meta.toml", "not toml at all [")
	e.stage(testManifest())
	if err := e.upd.Ingest(context.Background(), e.task, e.build, "w", testManifest()); err != nil {
		t.Fatalf("Ingest with corrupt old sidecar: %v", err)
	}
	if sc := e.storedSidecar(testPkgbase); sc.Pkgbase != testPkgbase {
		t.Errorf("sidecar Pkgbase = %q, want %q", sc.Pkgbase, testPkgbase)
	}
	execs := e.execLines()
	if len(execs) != 1 || !strings.Contains(execs[0], "repo-add") {
		t.Errorf("corrupt old sidecar must degrade to add-only, got %v", execs)
	}
}

// TestIngestEmptyManifestRejected asserts an ingest without any package
// artifact is refused before any side effect.
func TestIngestEmptyManifestRejected(t *testing.T) {
	e := newIngestEnv(t, "local", execCfg{})
	e.stage(testManifest())
	for name, m := range map[string][]Artifact{
		"nil":            nil,
		"empty":          {},
		"only-srcinfo":   {{File: testSrcinfo, Kind: "srcinfo", SHA256: "s3"}},
		"only-signature": {{File: testSigFile, Kind: "signature", SHA256: "s2"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := e.upd.Ingest(context.Background(), e.task, e.build, "w", m); err == nil {
				t.Error("Ingest: want error for package-less manifest, got nil")
			}
		})
	}
	if len(e.execLines()) != 0 {
		t.Error("no repo command may run for a rejected manifest")
	}
}

// TestIngestMissingSrcinfoRejected asserts pkgbase resolution requires the
// uploaded .SRCINFO snapshot.
func TestIngestMissingSrcinfoRejected(t *testing.T) {
	e := newIngestEnv(t, "local", execCfg{})
	m := []Artifact{
		{File: testPkgFile, Kind: "package", Pkgname: testPkgbase, Version: "1.2.3-1", Arch: "x86_64"},
	}
	e.stage(m)
	err := e.upd.Ingest(context.Background(), e.task, e.build, "w", m)
	if err == nil {
		t.Fatal("Ingest without srcinfo artifact: want error, got nil")
	}
	if !strings.Contains(err.Error(), "srcinfo") {
		t.Errorf("error must mention the missing srcinfo artifact, got %v", err)
	}
}

// TestIngestMoveFailureContext asserts a failed move returns an error
// carrying the file context, and that a retry where the destination already
// exists (a previously ingested entry) is treated as done (idempotent
// retry).
func TestIngestMoveFailureContext(t *testing.T) {
	e := newIngestEnv(t, "local", execCfg{})
	e.stage(testManifest())
	e.fs.moveErr = errors.New("fake backend down")
	err := e.upd.Ingest(context.Background(), e.task, e.build, "w", testManifest())
	if err == nil {
		t.Fatal("Ingest with failing move: want error, got nil")
	}
	if !strings.Contains(err.Error(), testPkgFile) {
		t.Errorf("move error must name the file, got %v", err)
	}

	// Retry with the destination already present: the move is skipped.
	e.seedRoot(testPkgFile, "already")
	e.seedRoot(testSigFile, "already-sig")
	e.fs.moveErr = nil
	if err := e.upd.Ingest(context.Background(), e.task, e.build, "w", testManifest()); err != nil {
		t.Fatalf("retry after partial move: %v", err)
	}
}

// TestIngestNoMover asserts the degraded Get+Put+Delete path when the
// backend does not advertise the Mover capability (s3 without Move).
func TestIngestNoMover(t *testing.T) {
	e := newIngestEnv(t, "local", execCfg{})
	e.stage(testManifest())
	e.upd.backend = backendWithoutMover{e.fs}
	if err := e.upd.Ingest(context.Background(), e.task, e.build, "w", testManifest()); err != nil {
		t.Fatalf("Ingest without Mover: %v", err)
	}
	for _, name := range []string{testPkgFile, testSigFile} {
		if !e.logHas("get staging/" + testTaskID + "/" + name) {
			t.Errorf("missing degraded Get for %s", name)
		}
		if !e.logHas("put " + name) {
			t.Errorf("missing degraded Put for %s", name)
		}
		if !e.logHas("delete staging/" + testTaskID + "/" + name) {
			t.Errorf("missing degraded Delete for %s", name)
		}
	}
}

// TestIngestIdempotent asserts a second Ingest of the same manifest succeeds
// without residue or error.
func TestIngestIdempotent(t *testing.T) {
	e := newIngestEnv(t, "local", execCfg{})
	m := testManifest()
	e.stage(m)
	if err := e.upd.Ingest(context.Background(), e.task, e.build, "w", m); err != nil {
		t.Fatalf("first Ingest: %v", err)
	}
	if err := e.upd.Ingest(context.Background(), e.task, e.build, "w", m); err != nil {
		t.Fatalf("second Ingest: %v", err)
	}
	if len(e.execLines()) != 2 {
		t.Errorf("exec lines = %d, want 2 (one per ingest)", len(e.execLines()))
	}
	if sc := e.storedSidecar(testPkgbase); sc.Pkgbase != testPkgbase || len(sc.Artifacts) != 3 {
		t.Errorf("sidecar after second ingest = %+v", sc)
	}
}

// TestIngestLocalAtomicNoTempResidue runs Ingest against the real local
// backend and asserts the atomic side file write leaves no temp files in the
// repository root (atomic write, no residue).
func TestIngestLocalAtomicNoTempResidue(t *testing.T) {
	e := newIngestEnv(t, "local", execCfg{})
	real, err := storage.OpenLocal(e.root, "")
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	e.upd.backend = real
	// Seed staging through the real backend.
	for _, a := range testManifest() {
		data := []byte("content-" + a.File)
		if a.Kind == "srcinfo" {
			data = srcinfoText(testPkgbase)
		}
		if err := real.Put(context.Background(), real.StagingPath(testTaskID, a.File), strings.NewReader(string(data)), int64(len(data))); err != nil {
			t.Fatalf("seed staging: %v", err)
		}
	}
	if err := e.upd.Ingest(context.Background(), e.task, e.build, "w", testManifest()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	entries, err := os.ReadDir(e.root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	for _, de := range entries {
		if strings.Contains(de.Name(), ".tmp") {
			t.Errorf("temp residue in repository root: %s", de.Name())
		}
	}
	// The side file is present and parses.
	data, err := os.ReadFile(filepath.Join(e.root, testPkgbase+".meta.toml"))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	sc, err := ParseSidecar(data)
	if err != nil {
		t.Fatalf("parse sidecar: %v", err)
	}
	if sc.Pkgbase != testPkgbase || sc.Build.Worker != "w" {
		t.Errorf("sidecar = %+v", sc)
	}
}
