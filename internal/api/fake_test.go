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

package api

import (
	"bytes"
	"context"
	"io"
	"sync"
	"time"

	"git.0x0f.dev/varve/internal/detect"
	"git.0x0f.dev/varve/internal/dispatch"
	"git.0x0f.dev/varve/internal/sign"
)

// Compile-time assertion: the fake implements the full orchestrator
// contract.
var _ dispatch.Orchestrator = (*fakeOrchestrator)(nil)

// fakeOrchestrator is an in-memory implementation of
// dispatch.Orchestrator for contract tests. Each method either runs a
// programmable hook (hook* fields) or the built-in default semantics:
// claim-token validation, log offset checks, one-shot signing keys,
// staged file storage with offset validation, and request recording for
// assertions. All methods are safe for concurrent use.
type fakeOrchestrator struct {
	mu sync.Mutex

	// Programmable hooks; a nil hook falls back to default behavior.
	hookRegister     func(reg dispatch.RegisterReq) (*dispatch.RegisterResp, error)
	hookHeartbeat    func(hb dispatch.HeartbeatReq) (*dispatch.HeartbeatResp, error)
	hookPoll         func(poll dispatch.PollReq) (*dispatch.PollResp, error)
	hookGetTask      func(taskID, token string) (*dispatch.TaskDetail, error)
	hookAppendLog    func(taskID, token string, seg dispatch.LogSegment) (*dispatch.LogAck, error)
	hookReportResult func(taskID, token string, res dispatch.ResultReq) error
	hookIssueKey     func(taskID, token string) (*sign.KeyMaterial, error)
	hookUpload       func(taskID, token, name string, r io.Reader, size, offset int64) (*dispatch.FileMeta, error)
	hookDownload     func(taskID, token, name string) (io.ReadCloser, error)
	hookDeregister   func(name string) error

	// Default-behavior state.
	workerID    int64
	workerName  string
	nextTask    *dispatch.TaskDetail
	claimToken  string
	tasks       map[string]*dispatch.TaskDetail
	files       map[string][]byte // staged file contents by name
	keyMaterial *sign.KeyMaterial
	keyIssued   bool
	reported    bool
	cancelled   bool
	logSize     int64
	serverTime  time.Time

	// Recorded requests for assertions.
	lastRegister   dispatch.RegisterReq
	lastHeartbeat  dispatch.HeartbeatReq
	lastPoll       dispatch.PollReq
	lastLog        dispatch.LogSegment
	lastResult     dispatch.ResultReq
	lastDeregister string

	// Instrumentation.
	calls       map[string]int
	uploadPeak  int   // largest single read chunk observed during uploads
	uploadTotal int64 // total bytes streamed into uploads
}

// newFake returns a fake with deterministic defaults: a fixed server time
// and a fixed one-shot signing key material.
func newFake() *fakeOrchestrator {
	return &fakeOrchestrator{
		tasks:      make(map[string]*dispatch.TaskDetail),
		files:      make(map[string][]byte),
		calls:      make(map[string]int),
		serverTime: time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
		keyMaterial: &sign.KeyMaterial{
			KeyID:             "ABCD1234",
			ArmoredPrivateKey: "-----BEGIN PGP PRIVATE KEY BLOCK-----\nabc\n-----END PGP PRIVATE KEY BLOCK-----",
			Passphrase:        "secret",
		},
	}
}

// checkToken mirrors the orchestrator's claim-token check.
func (f *fakeOrchestrator) checkToken(token string) error {
	if f.claimToken != "" && token != f.claimToken {
		return dispatch.ErrForbidden
	}
	return nil
}

// Enqueue implements detect.Sink (no-op for the API tests).
func (f *fakeOrchestrator) Enqueue(ctx context.Context, c detect.Change, force bool) error {
	return nil
}

// Register implements Orchestrator.
func (f *fakeOrchestrator) Register(ctx context.Context, reg dispatch.RegisterReq) (*dispatch.RegisterResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["register"]++
	f.lastRegister = reg
	if f.hookRegister != nil {
		return f.hookRegister(reg)
	}
	f.workerName = reg.Name
	if f.workerID == 0 {
		f.workerID = 1
	}
	return &dispatch.RegisterResp{ID: f.workerID, Name: reg.Name}, nil
}

// Heartbeat implements Orchestrator.
func (f *fakeOrchestrator) Heartbeat(ctx context.Context, hb dispatch.HeartbeatReq) (*dispatch.HeartbeatResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["heartbeat"]++
	f.lastHeartbeat = hb
	if f.hookHeartbeat != nil {
		return f.hookHeartbeat(hb)
	}
	return &dispatch.HeartbeatResp{ServerTime: f.serverTime}, nil
}

// Poll implements Orchestrator.
func (f *fakeOrchestrator) Poll(ctx context.Context, poll dispatch.PollReq) (*dispatch.PollResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["poll"]++
	f.lastPoll = poll
	if f.hookPoll != nil {
		return f.hookPoll(poll)
	}
	return &dispatch.PollResp{Task: f.nextTask, ClaimToken: f.claimToken}, nil
}

// GetTask implements Orchestrator.
func (f *fakeOrchestrator) GetTask(ctx context.Context, taskID, token string) (*dispatch.TaskDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["gettask"]++
	if f.hookGetTask != nil {
		return f.hookGetTask(taskID, token)
	}
	if err := f.checkToken(token); err != nil {
		return nil, err
	}
	task, ok := f.tasks[taskID]
	if !ok {
		return nil, dispatch.ErrNotFound
	}
	return task, nil
}

// AppendLog implements Orchestrator with strict offset semantics.
func (f *fakeOrchestrator) AppendLog(ctx context.Context, taskID, token string, seg dispatch.LogSegment) (*dispatch.LogAck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["appendlog"]++
	f.lastLog = seg
	if f.hookAppendLog != nil {
		return f.hookAppendLog(taskID, token, seg)
	}
	if err := f.checkToken(token); err != nil {
		return nil, err
	}
	if seg.Offset != f.logSize {
		return nil, &dispatch.OffsetError{Current: f.logSize}
	}
	f.logSize += int64(len(seg.Data))
	return &dispatch.LogAck{Offset: f.logSize, Cancelled: f.cancelled}, nil
}

// ReportResult implements Orchestrator; a second report conflicts.
func (f *fakeOrchestrator) ReportResult(ctx context.Context, taskID, token string, res dispatch.ResultReq) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["result"]++
	f.lastResult = res
	if f.hookReportResult != nil {
		return f.hookReportResult(taskID, token, res)
	}
	if err := f.checkToken(token); err != nil {
		return err
	}
	if f.reported {
		return dispatch.ErrConflict
	}
	f.reported = true
	return nil
}

// IssueSigningKey implements Orchestrator; each task may claim it once.
func (f *fakeOrchestrator) IssueSigningKey(ctx context.Context, taskID, token string) (*sign.KeyMaterial, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["signingkey"]++
	if f.hookIssueKey != nil {
		return f.hookIssueKey(taskID, token)
	}
	if err := f.checkToken(token); err != nil {
		return nil, err
	}
	if f.keyIssued {
		return nil, sign.ErrAlreadyExported
	}
	f.keyIssued = true
	return f.keyMaterial, nil
}

// UploadFile implements Orchestrator with offset validation and streaming
// instrumentation (the largest read chunk is recorded for the memory-cap
// assertion).
func (f *fakeOrchestrator) UploadFile(ctx context.Context, taskID, token, name string, r io.Reader, size, offset int64) (*dispatch.FileMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["upload"]++
	if f.hookUpload != nil {
		return f.hookUpload(taskID, token, name, r, size, offset)
	}
	if err := f.checkToken(token); err != nil {
		return nil, err
	}
	current := int64(len(f.files[name]))
	if current != offset {
		return nil, &dispatch.OffsetError{Current: current}
	}
	rec := &chunkRecorder{r: r, peak: &f.uploadPeak, total: &f.uploadTotal}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rec); err != nil {
		return nil, err
	}
	f.files[name] = append(f.files[name], buf.Bytes()...)
	return &dispatch.FileMeta{Name: name, Offset: int64(len(f.files[name]))}, nil
}

// DownloadFile implements Orchestrator over the staged file map.
func (f *fakeOrchestrator) DownloadFile(ctx context.Context, taskID, token, name string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["download"]++
	if f.hookDownload != nil {
		return f.hookDownload(taskID, token, name)
	}
	if err := f.checkToken(token); err != nil {
		return nil, err
	}
	data, ok := f.files[name]
	if !ok {
		return nil, dispatch.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// Deregister implements Orchestrator.
func (f *fakeOrchestrator) Deregister(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["deregister"]++
	f.lastDeregister = name
	if f.hookDeregister != nil {
		return f.hookDeregister(name)
	}
	return nil
}

// The admin/dashboard methods are unused by the API tests; they return
// neutral values so the fake satisfies the full interface.

// CancelTask implements Orchestrator.
func (f *fakeOrchestrator) CancelTask(ctx context.Context, taskID string) error { return nil }

// RebuildPackage implements Orchestrator.
func (f *fakeOrchestrator) RebuildPackage(ctx context.Context, pkgbase string) error { return nil }

// DisableWorker implements Orchestrator.
func (f *fakeOrchestrator) DisableWorker(ctx context.Context, name string) error { return nil }

// EnableWorker implements Orchestrator.
func (f *fakeOrchestrator) EnableWorker(ctx context.Context, name string) error { return nil }

// RemoveWorker implements Orchestrator.
func (f *fakeOrchestrator) RemoveWorker(ctx context.Context, name string) error { return nil }

// Stats implements Orchestrator.
func (f *fakeOrchestrator) Stats(ctx context.Context) (*dispatch.Stats, error) {
	return &dispatch.Stats{}, nil
}

// ValidateConflicts implements Orchestrator.
func (f *fakeOrchestrator) ValidateConflicts(ctx context.Context) error { return nil }

// ReadLog implements Orchestrator.
func (f *fakeOrchestrator) ReadLog(ctx context.Context, buildID string) ([]byte, error) {
	return nil, nil
}

// TailLog implements Orchestrator.
func (f *fakeOrchestrator) TailLog(ctx context.Context, buildID string, offset int64, w io.Writer) (int64, error) {
	return 0, nil
}

// chunkRecorder wraps a reader and tracks the largest single read
// returned plus the total bytes read (the memory-cap proxy).
type chunkRecorder struct {
	r     io.Reader
	peak  *int
	total *int64
}

// Read implements io.Reader.
func (c *chunkRecorder) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > *c.peak {
		*c.peak = n
	}
	*c.total += int64(n)
	return n, err
}
