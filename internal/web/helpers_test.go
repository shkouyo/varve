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

package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/detect"
	"git.0x0f.dev/varve/internal/dispatch"
	"git.0x0f.dev/varve/internal/sign"
)

var testCtx = context.Background()

// fakeOrchestrator implements dispatch.Orchestrator with recorded admin
// calls and a scripted Stats result.
type fakeOrchestrator struct {
	mu sync.Mutex

	stats    *dispatch.Stats
	statsErr error

	rebuildErr, cancelErr, disableErr, enableErr, removeErr error

	rebuilds []string
	cancels  []string
	disables []string
	enables  []string
	removes  []string
}

func (f *fakeOrchestrator) Enqueue(ctx context.Context, c detect.Change, force bool) error {
	return nil
}
func (f *fakeOrchestrator) Register(ctx context.Context, reg dispatch.RegisterReq) (*dispatch.RegisterResp, error) {
	return &dispatch.RegisterResp{}, nil
}
func (f *fakeOrchestrator) Heartbeat(ctx context.Context, hb dispatch.HeartbeatReq) (*dispatch.HeartbeatResp, error) {
	return &dispatch.HeartbeatResp{}, nil
}
func (f *fakeOrchestrator) Poll(ctx context.Context, poll dispatch.PollReq) (*dispatch.PollResp, error) {
	return &dispatch.PollResp{}, nil
}
func (f *fakeOrchestrator) GetTask(ctx context.Context, taskID, token string) (*dispatch.TaskDetail, error) {
	return nil, dispatch.ErrNotFound
}
func (f *fakeOrchestrator) AppendLog(ctx context.Context, taskID, token string, seg dispatch.LogSegment) (*dispatch.LogAck, error) {
	return &dispatch.LogAck{}, nil
}
func (f *fakeOrchestrator) ReportResult(ctx context.Context, taskID, token string, res dispatch.ResultReq) error {
	return nil
}
func (f *fakeOrchestrator) IssueSigningKey(ctx context.Context, taskID, token string) (*sign.KeyMaterial, error) {
	return nil, dispatch.ErrNotFound
}
func (f *fakeOrchestrator) UploadFile(ctx context.Context, taskID, token, name string, r io.Reader, size, offset int64) (*dispatch.FileMeta, error) {
	return &dispatch.FileMeta{}, nil
}
func (f *fakeOrchestrator) DownloadFile(ctx context.Context, taskID, token, name string) (io.ReadCloser, error) {
	return nil, dispatch.ErrNotFound
}
func (f *fakeOrchestrator) Deregister(ctx context.Context, name string) error { return nil }

func (f *fakeOrchestrator) CancelTask(ctx context.Context, taskID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, taskID)
	return f.cancelErr
}
func (f *fakeOrchestrator) RebuildPackage(ctx context.Context, pkgbase string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rebuilds = append(f.rebuilds, pkgbase)
	return f.rebuildErr
}
func (f *fakeOrchestrator) DisableWorker(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disables = append(f.disables, name)
	return f.disableErr
}
func (f *fakeOrchestrator) EnableWorker(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enables = append(f.enables, name)
	return f.enableErr
}
func (f *fakeOrchestrator) RemoveWorker(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, name)
	return f.removeErr
}
func (f *fakeOrchestrator) Stats(ctx context.Context) (*dispatch.Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statsErr != nil {
		return nil, f.statsErr
	}
	return f.stats, nil
}
func (f *fakeOrchestrator) ValidateConflicts(ctx context.Context) error { return nil }
func (f *fakeOrchestrator) ReadLog(ctx context.Context, buildID string) ([]byte, error) {
	return nil, dispatch.ErrNotFound
}
func (f *fakeOrchestrator) TailLog(ctx context.Context, buildID string, offset int64, w io.Writer) (int64, error) {
	return offset, nil
}

// fakeLogReader implements LogReader over an in-memory log with
// scripted errors.
type fakeLogReader struct {
	mu sync.Mutex

	content []byte
	readErr error
	tailErr error
}

func newFakeLogReader(content string) *fakeLogReader {
	return &fakeLogReader{content: []byte(content)}
}

// ReadLog returns the full log, or readErr when set.
func (f *fakeLogReader) ReadLog(ctx context.Context, buildID string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return nil, f.readErr
	}
	return append([]byte(nil), f.content...), nil
}

// TailLog streams content[offset:] once; subsequent calls yield nothing.
// tailErr is returned once and then cleared.
func (f *fakeLogReader) TailLog(ctx context.Context, buildID string, offset int64, w io.Writer) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tailErr != nil {
		err := f.tailErr
		f.tailErr = nil
		return 0, err
	}
	if offset >= int64(len(f.content)) {
		return offset, nil
	}
	chunk := f.content[offset:]
	_, _ = w.Write(chunk)
	return int64(len(f.content)), nil
}

// testConfig returns a controller configuration with the web section
// populated.
func testConfig() *config.ControllerConfig {
	return &config.ControllerConfig{
		Web: config.WebConfig{
			DownloadEnabled: true,
			DownloadBaseURI: "https://dl.example.org/pool",
			RecentBuilds:    20,
			Admins:          []config.WebAdmin{{User: "admin", Password: "s3cret"}},
		},
	}
}

// newTestServer builds a server with a short SSE ping interval and no
// dependencies wired yet.
func newTestServer(t *testing.T, cfg *config.ControllerConfig, orch *fakeOrchestrator, store *db.Store, logs *fakeLogReader) *Server {
	t.Helper()
	s := New(cfg, orch, store, logs)
	s.pingInterval = 10 * time.Millisecond
	return s
}

// newTestDB opens an in-memory store.
func newTestDB(t *testing.T) *db.Store {
	t.Helper()
	s, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedPackage inserts a package row and fills p.ID (public write path:
// UpsertPackage, then the version/description via UpdatePackageAfterBuild).
func seedPackage(t *testing.T, s *db.Store, pkgbase, desc string) db.Package {
	t.Helper()
	p := db.Package{
		Pkgbase:     pkgbase,
		Branch:      "main",
		VCSKind:     "git",
		Arch:        "x86_64",
		Pkgdesc:     desc,
		Maintainers: []string{"alice@example.com"},
	}
	if err := s.UpsertPackage(testCtx, &p); err != nil {
		t.Fatalf("upsert package %q: %v", pkgbase, err)
	}
	return p
}

// seedCounter makes task ids unique across seeds within a test binary.
var seedCounter int64

// seedBuild creates a task + build row and finalizes it into state with
// the given artifacts and samples (public write path: CreateTask +
// WithTx/FinalizeTask). Returns the build with its id filled.
func seedBuild(t *testing.T, s *db.Store, pkg db.Package, state string, artifacts []db.Artifact, samples []db.Sample) db.Build {
	t.Helper()
	now := time.Now().UTC()
	seedCounter++
	task := &db.Task{
		ID:             "task-" + pkg.Pkgbase + "-" + itoa(seedCounter),
		PackageID:      pkg.ID,
		State:          "queued",
		CreatedAt:      now.Add(-time.Hour),
		LastProgressAt: now.Add(-time.Hour),
	}
	build := &db.Build{
		PackageID:   pkg.ID,
		Branch:      pkg.Branch,
		Commit:      "deadbeef",
		SrcinfoHash: "srcinfo-hash",
	}
	if err := s.CreateTask(testCtx, task, build); err != nil {
		t.Fatalf("create task for %q: %v", pkg.Pkgbase, err)
	}
	if err := s.WithTx(testCtx, func(tx *db.Tx) error {
		return tx.FinalizeTask(testCtx, task.ID, state, "", now, artifacts, samples)
	}); err != nil {
		t.Fatalf("finalize task %s: %v", task.ID, err)
	}
	return *build
}

// seedActiveTask creates a task + build row in a non-terminal state with
// the given task id.
func seedActiveTask(t *testing.T, s *db.Store, pkg db.Package, state, id string) {
	t.Helper()
	now := time.Now().UTC()
	task := &db.Task{ID: id, PackageID: pkg.ID, State: state, CreatedAt: now, LastProgressAt: now}
	build := &db.Build{PackageID: pkg.ID, Branch: pkg.Branch, Commit: "deadbeef", SrcinfoHash: "srcinfo-hash"}
	if err := s.CreateTask(testCtx, task, build); err != nil {
		t.Fatalf("create task %q: %v", id, err)
	}
}

// seedActiveBuild creates a task + build row in a non-terminal state
// without finalizing it (used for SSE ping/disconnect tests).
func seedActiveBuild(t *testing.T, s *db.Store, pkg db.Package, state string) db.Build {
	t.Helper()
	now := time.Now().UTC()
	seedCounter++
	task := &db.Task{
		ID:             "task-" + pkg.Pkgbase + "-" + itoa(seedCounter),
		PackageID:      pkg.ID,
		State:          state,
		CreatedAt:      now,
		LastProgressAt: now,
	}
	build := &db.Build{
		PackageID:   pkg.ID,
		Branch:      pkg.Branch,
		Commit:      "deadbeef",
		SrcinfoHash: "srcinfo-hash",
	}
	if err := s.CreateTask(testCtx, task, build); err != nil {
		t.Fatalf("create active task for %q: %v", pkg.Pkgbase, err)
	}
	return *build
}

// setPackageBuild records the outcome of a build on the package row
// (version, description, last_build_id) through the public tx path.
func setPackageBuild(t *testing.T, s *db.Store, pkgbase string, buildID string) {
	t.Helper()
	err := s.WithTx(testCtx, func(tx *db.Tx) error {
		return tx.UpdatePackageAfterBuild(testCtx, pkgbase, db.PackageUpdate{
			CurrentVersion: "1.2.3-1",
			Pkgdesc:        "A demo package",
			SrcinfoHash:    "srcinfo-hash",
			UpstreamRef:    "upstream-ref",
			BuildID:        buildID,
		})
	})
	if err != nil {
		t.Fatalf("update package %q after build: %v", pkgbase, err)
	}
}

// seedWorker registers a worker row.
func seedWorker(t *testing.T, s *db.Store, name string) db.Worker {
	t.Helper()
	now := time.Now().UTC().Add(-time.Minute)
	w := db.Worker{
		Name:          name,
		Role:          "host",
		Mode:          "host",
		Arch:          "x86_64",
		Capacity:      2,
		Status:        "online",
		Version:       "1.0.0",
		LastHeartbeat: &now,
	}
	if err := s.RegisterWorker(testCtx, &w); err != nil {
		t.Fatalf("register worker %q: %v", name, err)
	}
	return w
}

// get issues a request against the server handler and returns the
// recorder.
func get(t *testing.T, s *Server, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// newRequest builds a bare request.
func newRequest(t *testing.T, method, path string) *http.Request {
	t.Helper()
	return httptest.NewRequest(method, path, nil)
}

// serve runs a request through the server handler.
func serve(t *testing.T, s *Server, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// getAuth issues an authenticated request (Basic Auth header). The
// Origin header matches the default httptest host, as browsers send on
// same-site requests; the admin POST handlers require it.
func getAuth(t *testing.T, s *Server, method, path, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.SetBasicAuth(user, pass)
	req.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// postForm issues an authenticated url-encoded form POST (no-JavaScript
// admin actions) with a same-site Origin header.
func postForm(t *testing.T, s *Server, path, user, pass string, values map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	for k, v := range values {
		form.Set(k, v)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(user, pass)
	req.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// mustContain asserts that the body contains all needles.
func mustContain(t *testing.T, body string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(body, n) {
			t.Errorf("body does not contain %q\nbody:\n%s", n, body)
		}
	}
}
