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

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/detect"
	"git.0x0f.dev/varve/internal/dispatch"
	"git.0x0f.dev/varve/internal/mail"
	"git.0x0f.dev/varve/internal/repo"
	"git.0x0f.dev/varve/internal/sign"
	"git.0x0f.dev/varve/internal/storage"
)

// Test ports of the serve tests: distinct addresses make the recorder
// events unambiguous. The startServer injectable never binds real sockets.
const (
	testAPIPort = "127.0.0.1:18081"
	testWebPort = "127.0.0.1:18082"
)

// controllerConfig renders a valid controller TOML with every runtime path
// under dir. sign selects repo.sign (default "off"); gpgKey is the
// gpg.key_id, only consulted when sign is not "off".
func controllerConfig(dir, dbPath, repoRoot, logsDir, sign, gpgKey string) string {
	return fmt.Sprintf(`
[server]
api_listen = %q
web_listen = %q

[api]
token = "test-token"

[database]
path = %q

[storage]
backend = "local"

[storage.local]
root = %q

[repo]
sign = %q
keep_versions = 1

[gpg]
key_id = %q

[source]
url = "git@example.invalid:pkgbuilds.git"
poll_interval = "5m"
exclude_branches = ["main"]

[mail]
enabled = false
tls = "starttls"

[web]
download_enabled = false
download_base_uri = ""
admin_user = "admin"
admin_password = "test-password"

[logs]
dir = %q
`, testAPIPort, testWebPort, dbPath, repoRoot, sign, gpgKey, logsDir)
}

// writeControllerConfig writes the rendered config with 0600 permissions
// (the loader warns otherwise) and returns its path.
func writeControllerConfig(t *testing.T, dir string, sign, gpgKey string) string {
	t.Helper()
	path := filepath.Join(dir, "varve.toml")
	data := controllerConfig(dir,
		filepath.Join(dir, "varve.db"),
		filepath.Join(dir, "repo"),
		filepath.Join(dir, "logs"),
		sign, gpgKey)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// requireErrorContaining fails the test unless err is non-nil and its
// message contains want.
func requireErrorContaining(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err, want)
	}
}

// replaceVar swaps an injectable package variable and restores it at test
// cleanup (DETAIL §13.4).
func replaceVar[T any](t *testing.T, dst *T, val T) {
	t.Helper()
	old := *dst
	*dst = val
	t.Cleanup(func() { *dst = old })
}

// TestRunServeStartupFailures is the runServe failure matrix (DETAIL
// §13.4): config missing, validation failure, db migration failure, gpg
// missing and D6 conflict rejection. Every case must abort startup with an
// error naming the failing step.
func TestRunServeStartupFailures(t *testing.T) {
	t.Run("config missing", func(t *testing.T) {
		err := runServe([]string{"--config", filepath.Join(t.TempDir(), "nope.toml")})
		requireErrorContaining(t, err, "config")
	})

	t.Run("validation failure", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := writeControllerConfig(t, dir, "off", "")
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		broken := strings.Replace(string(data), "download_enabled = false", "download_enabled = true", 1)
		if broken == string(data) {
			t.Fatal("test config missing download_enabled = false")
		}
		if err := os.WriteFile(cfgPath, []byte(broken), 0o600); err != nil {
			t.Fatal(err)
		}
		err = runServe([]string{"--config", cfgPath})
		requireErrorContaining(t, err, "download_base_uri")
	})

	t.Run("db migration failure", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := writeControllerConfig(t, dir, "off", "")
		// The database path lives under a non-existent parent: sqlite
		// cannot create the file and the migration fails at open.
		broken := strings.Replace(string(mustRead(t, cfgPath)),
			filepath.Join(dir, "varve.db"), filepath.Join(dir, "missing", "varve.db"), 1)
		if err := os.WriteFile(cfgPath, []byte(broken), 0o600); err != nil {
			t.Fatal(err)
		}
		err := runServe([]string{"--config", cfgPath})
		requireErrorContaining(t, err, "db:")
	})

	t.Run("gpg missing", func(t *testing.T) {
		// The real sign.NewSigner needs a gpg keyring under /data/gnupg
		// (decision A7). Substitute an erroring constructor to exercise
		// the serve wiring of step 4 deterministically.
		replaceVar(t, &newSigner, func(*config.GPGConfig) (*sign.Signer, error) {
			return nil, errors.New("sign: gpg: executable file not found in $PATH")
		})
		dir := t.TempDir()
		cfgPath := writeControllerConfig(t, dir, "packages", "DEADBEEF")
		err := runServe([]string{"--config", cfgPath})
		requireErrorContaining(t, err, "gpg")
	})

	t.Run("conflict rejects startup", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "varve.db")
		seed, err := db.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()

		// Package "a" whose latest successful build produces pkgname "b",
		// plus a package whose pkgbase is exactly "b": the D6 name
		// collision (proposal §7.5).
		pkgA := &db.Package{Pkgbase: "a", Branch: "master", VCSKind: "git"}
		if err := seed.UpsertPackage(ctx, pkgA); err != nil {
			t.Fatal(err)
		}
		task := &db.Task{ID: "seed-a", PackageID: pkgA.ID, State: "running"}
		if err := seed.CreateTask(ctx, task, &db.Build{Branch: "master", Commit: "c1", SrcinfoHash: "h1"}); err != nil {
			t.Fatal(err)
		}
		err = seed.WithTx(ctx, func(tx *db.Tx) error {
			return tx.FinalizeTask(ctx, task.ID, "succeeded", "", time.Now().UTC(),
				[]db.Artifact{{File: "b-1-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "b", Version: "1-1", Arch: "x86_64"}}, nil)
		})
		if err != nil {
			t.Fatal(err)
		}
		pkgB := &db.Package{Pkgbase: "b", Branch: "master"}
		if err := seed.UpsertPackage(ctx, pkgB); err != nil {
			t.Fatal(err)
		}
		if err := seed.Close(); err != nil {
			t.Fatal(err)
		}

		cfgPath := writeControllerConfig(t, dir, "off", "")
		err = runServe([]string{"--config", cfgPath})
		requireErrorContaining(t, err, "conflict")
	})
}

// TestRunServeGracefulShutdown drives the full serve path with recorder
// fakes (DETAIL §13.4): the startup order of the injected components and
// the graceful shutdown order mandated by DETAIL §13.2 step 10 (signal →
// orch.Stop → detect stopped → api closed → web closed) are asserted from
// the recorded event list. The real config, database and storage backends
// run against t.TempDir().
func TestRunServeGracefulShutdown(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeControllerConfig(t, dir, "off", "")

	rec := newRecorder()
	fakeOrch := &fakeOrchestrator{rec: rec}
	replaceVar(t, &newOrchestrator, func(cfg *config.ControllerConfig, store *db.Store, backend storage.Backend,
		signer signerSurface, updater repo.Updater, notifier mail.Notifier, logs *dispatch.Logs) orchestrator {
		rec.record("orch.New")
		return fakeOrch
	})
	replaceVar(t, &newDetector, func(cfg *config.SourceConfig, store *db.Store, sink detect.Sink) (detector, error) {
		rec.record("detect.New")
		return &fakeDetector{rec: rec}, nil
	})
	replaceVar(t, &startServer, func(addr string, h http.Handler, errCh chan<- error) (httpServer, error) {
		rec.record("server.start:" + addr)
		return &fakeServer{rec: rec, name: addr}, nil
	})
	replaceVar(t, &waitSignal, func() error {
		rec.record("signal")
		return nil
	})

	if err := runServe([]string{"--config", cfgPath}); err != nil {
		t.Fatalf("runServe: %v", err)
	}

	got := rec.snapshot()
	want := []string{
		"orch.New",
		"orch.ValidateConflicts",
		"detect.New",
		"server.start:" + testAPIPort,
		"server.start:" + testWebPort,
		"signal",
		"orch.Stop",
		"detect.stopped",
		"close:" + testAPIPort,
		"close:" + testWebPort,
	}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	// The real startup steps ran against the temp dir: the database file
	// exists (step 2) and the storage root was created (step 3).
	if _, err := os.Stat(filepath.Join(dir, "varve.db")); err != nil {
		t.Errorf("database was not opened: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "repo")); err != nil {
		t.Errorf("storage root was not created: %v", err)
	}
}

// TestRunServeSignerWiring pins the bug fix M4 wiring contract: with
// repo.sign="off" the orchestrator injectable must receive a true nil
// signer, and with signing enabled a non-nil one. The pre-fix shape — a
// typed nil *sign.Signer inside the interface — defeats dispatch's nil
// checks and crashes when a task reaches a terminal state (V2
// acceptance, signer-typed-nil panic).
func TestRunServeSignerWiring(t *testing.T) {
	run := func(t *testing.T, sign, gpgKey string) signerSurface {
		t.Helper()
		dir := t.TempDir()
		cfgPath := writeControllerConfig(t, dir, sign, gpgKey)
		got := make(chan signerSurface, 1)
		rec := newRecorder()
		replaceVar(t, &newOrchestrator, func(cfg *config.ControllerConfig, store *db.Store, backend storage.Backend,
			signer signerSurface, updater repo.Updater, notifier mail.Notifier, logs *dispatch.Logs) orchestrator {
			got <- signer
			return &fakeOrchestrator{rec: rec}
		})
		replaceVar(t, &newDetector, func(cfg *config.SourceConfig, store *db.Store, sink detect.Sink) (detector, error) {
			return &fakeDetector{rec: rec}, nil
		})
		replaceVar(t, &startServer, func(addr string, h http.Handler, errCh chan<- error) (httpServer, error) {
			return &fakeServer{rec: rec, name: addr}, nil
		})
		replaceVar(t, &waitSignal, func() error { return nil })
		if err := runServe([]string{"--config", cfgPath}); err != nil {
			t.Fatalf("runServe: %v", err)
		}
		return <-got
	}

	t.Run("sign off delivers true nil", func(t *testing.T) {
		if s := run(t, "off", ""); s != nil {
			t.Fatalf("orchestrator signer = %#v, want true nil (a typed nil *sign.Signer would panic dispatch finalization)", s)
		}
	})
	t.Run("sign packages delivers the signer", func(t *testing.T) {
		replaceVar(t, &newSigner, func(*config.GPGConfig) (*sign.Signer, error) {
			return &sign.Signer{}, nil
		})
		if s := run(t, "packages", "DEADBEEF"); s == nil {
			t.Fatal("orchestrator signer = nil, want the configured signer")
		}
	})
}

// TestRunDispatch covers the DESIGN §2.12 command-line dispatch: the
// rebuild-index subcommand runs through the real config/db/storage stack,
// and unknown subcommands are rejected.
func TestRunDispatch(t *testing.T) {
	t.Run("rebuild-index via run", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := writeControllerConfig(t, dir, "off", "")
		repoRoot := filepath.Join(dir, "repo")
		if err := os.MkdirAll(repoRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		sidecar := `pkgbase = "foo"
branch = "master"

[[artifacts]]
file = "foo-1.0-1-x86_64.pkg.tar.zst"
kind = "package"
pkgname = "foo"
version = "1.0-1"
arch = "x86_64"
size = 7
sha256 = "cc"

[build]
commit = "c9"
srcinfo_hash = "h9"
`
		if err := os.WriteFile(filepath.Join(repoRoot, "foo.meta.toml"), []byte(sidecar), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := run([]string{"rebuild-index", "--config", cfgPath}); err != nil {
			t.Fatalf("run: %v", err)
		}
		store, err := db.Open(filepath.Join(dir, "varve.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		pkg, err := store.GetPackageByBase(context.Background(), "foo")
		if err != nil {
			t.Fatalf("package foo not indexed: %v", err)
		}
		if pkg.CurrentVersion != "1.0-1" {
			t.Errorf("current_version = %q, want %q", pkg.CurrentVersion, "1.0-1")
		}
	})

	t.Run("unknown subcommand", func(t *testing.T) {
		err := run([]string{"bogus"})
		requireErrorContaining(t, err, "unknown subcommand")
	})

	t.Run("invalid serve arguments", func(t *testing.T) {
		err := runServe([]string{"--config"})
		requireErrorContaining(t, err, "unexpected arguments")
	})
}

// ---------------------------------------------------------------------------
// Recorder fakes (DETAIL §13.4)
// ---------------------------------------------------------------------------

// recorder collects ordered events from the fakes; safe for concurrent use
// (the detector goroutine records on context cancellation).
type recorder struct {
	mu     sync.Mutex
	events []string
}

func newRecorder() *recorder { return &recorder{} }

func (r *recorder) record(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// fakeOrchestrator is an orchestrator recorder for the serve tests: every
// method serve actually invokes records an event (ValidateConflicts, Stop);
// the rest are neutral stubs satisfying the full contract, including
// detect.Sink (Submit).
type fakeOrchestrator struct {
	rec         *recorder
	validateErr error
}

var _ orchestrator = (*fakeOrchestrator)(nil)

func (f *fakeOrchestrator) Submit(ctx context.Context, c detect.Change) error { return nil }
func (f *fakeOrchestrator) Enqueue(ctx context.Context, c detect.Change, force bool) error {
	return nil
}
func (f *fakeOrchestrator) Register(ctx context.Context, reg dispatch.RegisterReq) (*dispatch.RegisterResp, error) {
	return nil, nil
}
func (f *fakeOrchestrator) Heartbeat(ctx context.Context, hb dispatch.HeartbeatReq) (*dispatch.HeartbeatResp, error) {
	return nil, nil
}
func (f *fakeOrchestrator) Poll(ctx context.Context, poll dispatch.PollReq) (*dispatch.PollResp, error) {
	return nil, nil
}
func (f *fakeOrchestrator) GetTask(ctx context.Context, taskID, token string) (*dispatch.TaskDetail, error) {
	return nil, nil
}
func (f *fakeOrchestrator) AppendLog(ctx context.Context, taskID, token string, seg dispatch.LogSegment) (*dispatch.LogAck, error) {
	return nil, nil
}
func (f *fakeOrchestrator) ReportResult(ctx context.Context, taskID, token string, res dispatch.ResultReq) error {
	return nil
}
func (f *fakeOrchestrator) IssueSigningKey(ctx context.Context, taskID, token string) (*sign.KeyMaterial, error) {
	return nil, nil
}
func (f *fakeOrchestrator) UploadFile(ctx context.Context, taskID, token, name string, r io.Reader, size, offset int64) (*dispatch.FileMeta, error) {
	return nil, nil
}
func (f *fakeOrchestrator) DownloadFile(ctx context.Context, taskID, token, name string) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeOrchestrator) Deregister(ctx context.Context, name string) error { return nil }
func (f *fakeOrchestrator) CancelTask(ctx context.Context, taskID string) error {
	return nil
}
func (f *fakeOrchestrator) RebuildPackage(ctx context.Context, pkgbase string) error {
	return nil
}
func (f *fakeOrchestrator) DisableWorker(ctx context.Context, name string) error {
	return nil
}
func (f *fakeOrchestrator) RemoveWorker(ctx context.Context, name string) error { return nil }
func (f *fakeOrchestrator) Stats(ctx context.Context) (*dispatch.Stats, error) {
	return &dispatch.Stats{}, nil
}
func (f *fakeOrchestrator) ValidateConflicts(ctx context.Context) error {
	f.rec.record("orch.ValidateConflicts")
	return f.validateErr
}
func (f *fakeOrchestrator) ReadLog(ctx context.Context, buildID string) ([]byte, error) {
	return nil, nil
}
func (f *fakeOrchestrator) TailLog(ctx context.Context, buildID string, offset int64, w io.Writer) (int64, error) {
	return 0, nil
}
func (f *fakeOrchestrator) Stop() { f.rec.record("orch.Stop") }

// fakeDetector records the moment its Run loop observes the cancellation
// (serve waits for this before closing the HTTP servers).
type fakeDetector struct {
	rec *recorder
}

func (d *fakeDetector) Run(ctx context.Context) error {
	<-ctx.Done()
	d.rec.record("detect.stopped")
	return nil
}

// fakeServer is the startServer recorder: it never binds a socket and
// records its Close.
type fakeServer struct {
	rec  *recorder
	name string
}

func (s *fakeServer) Close() error {
	s.rec.record("close:" + s.name)
	return nil
}

// mustRead reads a file, failing the test on error.
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
