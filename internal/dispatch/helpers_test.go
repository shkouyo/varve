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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/detect"
	"git.0x0f.dev/varve/internal/mail"
	"git.0x0f.dev/varve/internal/repo"
	"git.0x0f.dev/varve/internal/sign"
	"git.0x0f.dev/varve/internal/storage"
)

// sha256Hex returns the hex sha256 of s.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// opLog: append-only line log shared between the in-process fakes and the
// child processes of the fake exec command (the canonical re-exec pattern
// keeps the fakes self-contained).
// ---------------------------------------------------------------------------

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
	fmt.Fprintln(f, line)
	f.Close()
}

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

// ---------------------------------------------------------------------------
// fakeStorage: in-memory Backend + Mover + Appender recording every
// operation in the opLog.
// ---------------------------------------------------------------------------

type fakeStorage struct {
	log   *opLog
	mu    sync.Mutex
	files map[string][]byte
}

func newFakeStorage(t *testing.T) *fakeStorage {
	t.Helper()
	return &fakeStorage{log: newOpLog(t), files: make(map[string][]byte)}
}

func (f *fakeStorage) Put(ctx context.Context, name string, r io.Reader, size int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.files[name] = data
	f.mu.Unlock()
	f.log.add("put " + name)
	return nil
}

func (f *fakeStorage) Get(ctx context.Context, name string, w io.Writer) error {
	f.mu.Lock()
	data, ok := f.files[name]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", storage.ErrNotFound, name)
	}
	_, err := w.Write(data)
	f.log.add("get " + name)
	return err
}

func (f *fakeStorage) Delete(ctx context.Context, name string) error {
	f.mu.Lock()
	delete(f.files, name)
	f.mu.Unlock()
	f.log.add("delete " + name)
	return nil
}

func (f *fakeStorage) List(ctx context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for name := range f.files {
		if ok, _ := filepath.Match(prefix, name); ok {
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
		return storage.FileInfo{}, fmt.Errorf("%w: %s", storage.ErrNotFound, name)
	}
	return storage.FileInfo{Size: int64(len(data))}, nil
}

// StagingPath returns the default virtual staging path of a task artifact.
func (f *fakeStorage) StagingPath(taskID, fileName string) string {
	return "staging/" + taskID + "/" + fileName
}

// StagingDir returns "" because the fake has no physical staging tree.
func (f *fakeStorage) StagingDir() string { return "" }

func (f *fakeStorage) Move(ctx context.Context, src, dst string) error {
	f.mu.Lock()
	data, ok := f.files[src]
	if ok {
		f.files[dst] = data
		delete(f.files, src)
	}
	f.mu.Unlock()
	f.log.add("move " + src + " " + dst)
	return nil
}

func (f *fakeStorage) Append(ctx context.Context, name string, r io.Reader, offset int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	existing := f.files[name]
	if int64(len(existing)) != offset {
		f.mu.Unlock()
		return fmt.Errorf("fake append offset %d != size %d", offset, len(existing))
	}
	f.files[name] = append(existing, data...)
	f.mu.Unlock()
	f.log.add("append " + name)
	return nil
}

// GetBytes reads one object fully (test convenience).
func (f *fakeStorage) GetBytes(ctx context.Context, name string) ([]byte, error) {
	var buf bytes.Buffer
	if err := f.Get(ctx, name, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------
// fakeUpdater / fakeSigner / fakeNotifier
// ---------------------------------------------------------------------------

type fakeUpdater struct {
	log       *opLog
	ingestErr error
	removeErr error
	worker    string
	lastBuild *db.Build
	lastTask  *db.Task
	removed   []string
	entered   chan struct{} // closed on the first Ingest entry (test sync)
	block     chan struct{} // when non-nil, Ingest blocks until closed
	enterOnce sync.Once
}

func (f *fakeUpdater) Ingest(ctx context.Context, task *db.Task, build *db.Build, workerName string, manifest []repo.Artifact) error {
	f.enterOnce.Do(func() {
		if f.entered != nil {
			close(f.entered)
		}
	})
	f.log.add(fmt.Sprintf("ingest %s worker=%s files=%d", task.ID, workerName, len(manifest)))
	f.worker = workerName
	f.lastBuild = build
	f.lastTask = task
	if f.block != nil {
		<-f.block
	}
	if f.ingestErr != nil {
		return f.ingestErr
	}
	return nil
}

// Remove implements repo.Updater for the cascade path.
func (f *fakeUpdater) Remove(ctx context.Context, pkgbase string) error {
	f.log.add("remove " + pkgbase)
	f.removed = append(f.removed, pkgbase)
	return f.removeErr
}

type fakeSigner struct {
	log       *opLog
	verifyErr error
	exported  map[string]bool
	cleared   []string
}

func newFakeSigner(t *testing.T) *fakeSigner {
	t.Helper()
	return &fakeSigner{log: newOpLog(t), exported: make(map[string]bool)}
}

func (f *fakeSigner) VerifyDetached(ctx context.Context, sigPath, pkgPath string) error {
	f.log.add("verify " + filepath.Base(sigPath) + " " + filepath.Base(pkgPath))
	return f.verifyErr
}

func (f *fakeSigner) ExportForTask(ctx context.Context, taskID string) (*sign.KeyMaterial, error) {
	if f.exported[taskID] {
		return nil, sign.ErrAlreadyExported
	}
	f.exported[taskID] = true
	f.log.add("export " + taskID)
	return &sign.KeyMaterial{KeyID: "ABCD", ArmoredPrivateKey: "-----BEGIN PGP PRIVATE KEY BLOCK-----", Passphrase: "pw"}, nil
}

func (f *fakeSigner) ClearTask(taskID string) {
	f.cleared = append(f.cleared, taskID)
	f.log.add("clear " + taskID)
}

type fakeNotifier struct {
	log           *opLog
	calls         []mail.FailureInfo
	aurCalls      []mail.AURPushInfo
	aurRecipients []string
}

func newFakeNotifier(t *testing.T) *fakeNotifier {
	t.Helper()
	return &fakeNotifier{log: newOpLog(t)}
}

func (f *fakeNotifier) SendFailure(ctx context.Context, to []string, info mail.FailureInfo) error {
	f.calls = append(f.calls, info)
	f.log.add("notify " + info.Pkgbase + " " + info.Stage)
	return nil
}

func (f *fakeNotifier) SendAURFailure(ctx context.Context, to []string, info mail.AURPushInfo) error {
	f.aurCalls = append(f.aurCalls, info)
	f.aurRecipients = append(f.aurRecipients, to...)
	f.log.add("notify-aur " + info.Pkgbase)
	return nil
}

// ---------------------------------------------------------------------------
// Fake git: the helper process replays a state file for rev-parse / show
// / archive, and every invocation is recorded.
// ---------------------------------------------------------------------------

type gitState struct {
	Commit  string            `json:"commit"`
	Srcinfo string            `json:"srcinfo,omitempty"`
	Dotfile string            `json:"dotfile,omitempty"`
	Extras  map[string]string `json:"extras,omitempty"`
	Archive []byte            `json:"archive,omitempty"` // git archive stdout
	Fail    string            `json:"fail,omitempty"`    // "rev-parse" | "show" | "archive"
}

func fakeGitFor(t *testing.T, log *opLog, state *gitState) func(context.Context, string, ...string) *exec.Cmd {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "git-state.json")
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal git state: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0o644); err != nil {
		t.Fatalf("write git state: %v", err)
	}
	return func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		args := []string{"-test.run=TestHelperProcess", "--", name}
		args = append(args, arg...)
		cmd := exec.CommandContext(ctx, os.Args[0], args...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"FAKE_GIT_STATE="+statePath,
			"FAKE_EXEC_LOG="+log.path,
		)
		return cmd
	}
}

// TestHelperProcess is the re-exec helper: when invoked by fakeGitFor it
// acts as the fake git, otherwise it is a no-op test.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args[1:]
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		os.Exit(127)
	}
	prog := args[0]
	rest := args[1:]
	if logPath := os.Getenv("FAKE_EXEC_LOG"); logPath != "" {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintln(f, prog, strings.Join(rest, " "))
			f.Close()
		}
	}
	if prog != "git" {
		os.Exit(127)
	}
	var state gitState
	if raw := os.Getenv("FAKE_GIT_STATE"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &state)
		if data, err := os.ReadFile(raw); err == nil {
			_ = json.Unmarshal(data, &state)
		}
	}
	for i, a := range rest {
		switch a {
		case "rev-parse":
			if state.Fail == "rev-parse" {
				os.Exit(1)
			}
			fmt.Fprint(os.Stdout, state.Commit)
			os.Exit(0)
		case "show":
			ref, file, _ := strings.Cut(rest[i+1], ":")
			_ = ref
			var content string
			switch {
			case file == ".SRCINFO":
				content = state.Srcinfo
			case strings.HasSuffix(file, ".varve.toml"):
				content = state.Dotfile
			default:
				content = state.Extras[file]
			}
			if state.Fail == "show" {
				os.Exit(1)
			}
			fmt.Fprint(os.Stdout, content)
			os.Exit(0)
		case "archive":
			if state.Fail == "archive" {
				os.Exit(1)
			}
			_, _ = os.Stdout.Write(state.Archive)
			os.Exit(0)
		}
	}
	os.Exit(127)
}

// ---------------------------------------------------------------------------
// Orchestrator test harness: real db (:memory:), fakes everywhere,
// injected clock and git. env.now starts at the real wall clock so claim
// timestamps (which db writes with time.Now internally) stay in the past
// relative to injected advances.
// ---------------------------------------------------------------------------

type testEnv struct {
	o     *OrchestratorImpl
	store *db.Store
	fs    *fakeStorage
	up    *fakeUpdater
	sig   *fakeSigner
	not   *fakeNotifier
	logs  *Logs
	cfg   *config.ControllerConfig
	log   *opLog
	now   time.Time
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	cfg := &config.ControllerConfig{
		Source: config.SourceConfig{PollInterval: time.Hour},
		Worker: config.WorkerLimits{
			HeartbeatTimeout: 90 * time.Second,
			StallTimeout:     10 * time.Minute,
			BuildTimeout:     30 * time.Minute,
		},
		Repo:    config.RepoConfig{Sign: "off"},
		Logs:    config.LogsConfig{Dir: t.TempDir(), Retention: 90 * 24 * time.Hour, MaxBuilds: 1000},
		Storage: config.StorageConfig{Backend: "local", Local: config.LocalConfig{Root: t.TempDir()}},
		Server:  config.ServerConfig{WebURL: "https://varve.example.org"},
	}
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	log := newOpLog(t)
	env := &testEnv{
		store: store,
		fs:    newFakeStorage(t),
		up:    &fakeUpdater{},
		sig:   newFakeSigner(t),
		not:   newFakeNotifier(t),
		logs:  NewLogs(t.TempDir()),
		cfg:   cfg,
		log:   log,
		now:   time.Now().UTC(),
	}
	env.up.log = log
	env.sig.log = log
	env.not.log = log
	env.fs.log = log
	state := &gitState{Commit: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"}
	env.o = NewOrchestrator(cfg, store, env.fs, env.sig, env.up, env.not, env.logs)
	env.o.now = func() time.Time { return env.now }
	env.o.execCommand = fakeGitFor(t, log, state)
	env.o.mirrorDir = "/data/source/fake.git"
	t.Cleanup(func() { env.o.Stop(); store.Close() })
	return env
}

// advance moves the injected clock forward.
func (e *testEnv) advance(d time.Duration) {
	e.now = e.now.Add(d)
}

// enqueue helper: submit a change for a branch with optional maintainers
// and return the created task id (the task stays active).
func (e *testEnv) enqueue(t *testing.T, pkgbase, branch string, maintainers ...string) string {
	t.Helper()
	c := detectChange(pkgbase, branch, maintainers...)
	if err := e.o.Enqueue(context.Background(), c, false); err != nil {
		t.Fatalf("Enqueue %s: %v", pkgbase, err)
	}
	return e.activeTaskFor(t, pkgbase)
}

// enqueueArch is enqueue with an explicit canonical architecture set.
func (e *testEnv) enqueueArch(t *testing.T, pkgbase, branch, arch string) string {
	t.Helper()
	c := detectChangeArch(pkgbase, branch, arch)
	if err := e.o.Enqueue(context.Background(), c, false); err != nil {
		t.Fatalf("Enqueue %s: %v", pkgbase, err)
	}
	return e.activeTaskFor(t, pkgbase)
}

// activeTaskFor resolves the active task id of a pkgbase.
func (e *testEnv) activeTaskFor(t *testing.T, pkgbase string) string {
	t.Helper()
	pkg, err := e.store.GetPackageByBase(context.Background(), pkgbase)
	if err != nil {
		t.Fatalf("GetPackageByBase %s: %v", pkgbase, err)
	}
	tasks, err := e.store.ListActiveTasks(context.Background())
	if err != nil {
		t.Fatalf("ListActiveTasks: %v", err)
	}
	for _, tk := range tasks {
		if tk.PackageID == pkg.ID {
			return tk.ID
		}
	}
	t.Fatalf("no active task for %s", pkgbase)
	return ""
}

// registerWorker registers a node and returns its id.
func (e *testEnv) registerWorker(t *testing.T, name, role, mode string, capacity int) int64 {
	t.Helper()
	return e.registerWorkerFull(t, name, role, mode, "x86_64", capacity)
}

// registerWorkerArch registers a node with an explicit architecture.
func (e *testEnv) registerWorkerArch(t *testing.T, name, arch string, capacity int) int64 {
	t.Helper()
	return e.registerWorkerFull(t, name, "host", "host", arch, capacity)
}

// registerWorkerFull registers a node with full control over the fields.
func (e *testEnv) registerWorkerFull(t *testing.T, name, role, mode, arch string, capacity int) int64 {
	t.Helper()
	resp, err := e.o.Register(context.Background(), RegisterReq{Name: name, Role: role, Mode: mode, Arch: arch, Capacity: capacity, Version: "0.1.0"})
	if err != nil {
		t.Fatalf("Register %s: %v", name, err)
	}
	return resp.ID
}

// claim polls for one task and returns the claimed task id plus its token.
func (e *testEnv) claim(t *testing.T, worker string) (string, string) {
	t.Helper()
	resp, err := e.o.Poll(context.Background(), PollReq{Name: worker, Arch: "x86_64"})
	if err != nil {
		t.Fatalf("Poll %s: %v", worker, err)
	}
	if resp.Task == nil {
		t.Fatalf("Poll %s: no task claimable", worker)
	}
	return resp.Task.ID, resp.ClaimToken
}

// detectChange builds a detect.Change for a branch. The maintainers
// arguments are email addresses mapped to email-only maintainer entries.
func detectChange(pkgbase, branch string, maintainers ...string) detect.Change {
	ms := make([]db.Maintainer, 0, len(maintainers))
	for _, m := range maintainers {
		ms = append(ms, db.Maintainer{Email: m})
	}
	return detect.Change{
		Package:     detect.Package{Pkgbase: pkgbase, Branch: branch, VCSKind: "", Arch: "x86_64"},
		Maintainers: ms,
		Reason:      detect.ReasonSrcinfo,
	}
}

// detectChangeArch builds a detect.Change with an explicit architecture
// set (canonical "any" or "|"-joined form).
func detectChangeArch(pkgbase, branch, arch string) detect.Change {
	c := detectChange(pkgbase, branch)
	c.Package.Arch = arch
	return c
}

// stagedContent returns the deterministic content a file is staged with, so
// manifest sha256 values can be computed in advance.
func stagedContent(file string) string {
	if file == ".SRCINFO" {
		return "pkgbase = testpkg\npkgname = testpkg\npkgver = 1:1.2.3\npkgrel = 1\npkgdesc = test package\nurl = https://example.org/foo\nlicense = MIT\nsource = https://example.org/foo.tar.gz\nconflict = testpkg-legacy\nprovides = testpkg-provided\narch = x86_64\n"
	}
	return "content-of-" + file
}

// testArtifacts builds a standard manifest (package + .SRCINFO) whose
// sha256 values match the deterministic staged content.
func testArtifacts(pkgname, version string) []repo.Artifact {
	pkgFile := pkgname + "-" + version + "-x86_64.pkg.tar.zst"
	return []repo.Artifact{
		{File: pkgFile, Kind: "package", Pkgname: pkgname, Version: version, Arch: "x86_64",
			Size: int64(len(stagedContent(pkgFile))), SHA256: sha256Hex(stagedContent(pkgFile))},
		{File: ".SRCINFO", Kind: "srcinfo",
			Size: int64(len(stagedContent(".SRCINFO"))), SHA256: sha256Hex(stagedContent(".SRCINFO"))},
	}
}

// stage uploads deterministic content for a task artifact.
func (e *testEnv) stage(t *testing.T, taskID, file string) {
	t.Helper()
	if err := e.fs.Put(context.Background(), e.fs.StagingPath(taskID, file),
		strings.NewReader(stagedContent(file)), -1); err != nil {
		t.Fatalf("stage %s: %v", file, err)
	}
}

// buildSucceeded runs one full successful cycle: enqueue, register a
// worker, claim, stage the manifest files and report succeeded.
func (e *testEnv) buildSucceeded(t *testing.T, pkgbase string, artifacts []repo.Artifact) {
	t.Helper()
	taskID := e.enqueue(t, pkgbase, pkgbase)
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
