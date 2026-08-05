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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/repo"
)

const (
	goldenRegBody = `{"name":"proud-heron-7","role":"host","mode":"host","arch":"x86_64","capacity":2,"version":"0.1.0"}`
	goldenLogBody = `{"offset":0,"data":"==> Making package\n"}`
	logDataLen    = int64(19) // len("==> Making package\n")
)

// TestWireGoldenEndpoints pins each endpoint's request → response golden
// sample byte-for-byte (DETAIL §9.7 item 1, DESIGN §5.3). Every case uses
// a fresh fake and server so no state leaks between samples.
func TestWireGoldenEndpoints(t *testing.T) {
	fixedTime := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		method   string
		path     string
		headers  map[string]string
		body     string
		setup    func(f *fakeOrchestrator)
		want     int
		wantBody string
	}{
		{
			name: "register", method: http.MethodPost, path: "/api/v1/register",
			headers: bearer(), body: goldenRegBody, want: http.StatusOK,
			wantBody: `{"id":1,"name":"proud-heron-7"}`,
		},
		{
			name: "heartbeat", method: http.MethodPost, path: "/api/v1/heartbeat",
			headers: bearer(),
			body:    `{"name":"proud-heron-7","metrics":{"cpu_percent":12.5,"mem_used_bytes":5368709120,"mem_total_bytes":17179869184,"uptime_secs":302400},"tasks":[],"containers":[]}`,
			setup: func(f *fakeOrchestrator) {
				f.hookHeartbeat = func(hb HeartbeatReq) (*HeartbeatResp, error) {
					return &HeartbeatResp{CancelledTaskIDs: []string{"t-cancel-1"}, ServerTime: fixedTime}, nil
				}
			},
			want:     http.StatusOK,
			wantBody: `{"cancelled_task_ids":["t-cancel-1"],"server_time":"2026-08-05T10:00:00Z"}`,
		},
		{
			name: "poll without task", method: http.MethodPost, path: "/api/v1/poll",
			headers: bearer(), body: `{"name":"proud-heron-7","arch":"x86_64"}`,
			want:     http.StatusOK,
			wantBody: `{"task":null,"claim_token":"","cancelled_task_ids":null}`,
		},
		{
			name: "poll with task", method: http.MethodPost, path: "/api/v1/poll",
			headers: bearer(), body: `{"name":"proud-heron-7","arch":"x86_64"}`,
			setup: func(f *fakeOrchestrator) {
				f.nextTask = fullTaskDetail()
				f.claimToken = "a1b2c3"
			},
			want:     http.StatusOK,
			wantBody: `{"task":` + goldenTaskDetailJSON + `,"claim_token":"a1b2c3","cancelled_task_ids":null}`,
		},
		{
			name: "get task", method: http.MethodGet, path: "/api/v1/tasks/" + testTaskID,
			headers: taskAuth(testClaimTok),
			setup: func(f *fakeOrchestrator) {
				f.tasks[testTaskID] = fullTaskDetail()
				f.claimToken = testClaimTok
			},
			want:     http.StatusOK,
			wantBody: goldenTaskDetailJSON,
		},
		{
			name: "append log", method: http.MethodPost,
			path: "/api/v1/tasks/" + testTaskID + "/log", headers: taskAuth(testClaimTok),
			body: goldenLogBody, want: http.StatusOK,
			wantBody: `{"offset":19,"cancelled":false}`,
		},
		{
			name: "log offset mismatch", method: http.MethodPost,
			path: "/api/v1/tasks/" + testTaskID + "/log", headers: taskAuth(testClaimTok),
			body:     `{"offset":3,"data":"zz"}`,
			want:     http.StatusConflict,
			wantBody: `{"error":{"code":"conflict","message":"dispatch: offset mismatch: server offset is 0"},"offset":0}`,
		},
		{
			name: "report result", method: http.MethodPost,
			path: "/api/v1/tasks/" + testTaskID + "/result", headers: taskAuth(testClaimTok),
			body:     `{"status":"succeeded","error":null,"artifacts":[{"file":"foo-1.2.3-1-x86_64.pkg.tar.zst","kind":"package","pkgname":"foo","version":"1.2.3-1","arch":"x86_64","size":123456,"sha256":"deadbeef"}],"resource_usage":[{"at":"2026-08-05T10:00:00Z","cpu_time_ns":0,"memory_bytes":0}],"commit":"abc123"}`,
			want:     http.StatusOK,
			wantBody: `{"accepted":true}`,
		},
		{
			name: "signing key", method: http.MethodPost,
			path: "/api/v1/tasks/" + testTaskID + "/signing-key", headers: taskAuth(testClaimTok),
			want:     http.StatusOK,
			wantBody: `{"key_id":"ABCD1234","armored_private_key":"-----BEGIN PGP PRIVATE KEY BLOCK-----\nabc\n-----END PGP PRIVATE KEY BLOCK-----","passphrase":"secret"}`,
		},
		{
			name: "signing key repeat", method: http.MethodPost,
			path: "/api/v1/tasks/" + testTaskID + "/signing-key", headers: taskAuth(testClaimTok),
			setup:    func(f *fakeOrchestrator) { f.keyIssued = true },
			want:     http.StatusConflict,
			wantBody: `{"error":{"code":"conflict","message":"sign: key already exported for task"}}`,
		},
		{
			name: "upload", method: http.MethodPut,
			path: "/api/v1/tasks/" + testTaskID + "/files/" + testFileName, headers: taskAuth(testClaimTok),
			body: "0123456789abcdef", want: http.StatusOK,
			wantBody: `{"name":"foo-1.2.3-1-x86_64.pkg.tar.zst","offset":16}`,
		},
		{
			name: "download", method: http.MethodGet,
			path: "/api/v1/tasks/" + testTaskID + "/files/" + testFileName, headers: taskAuth(testClaimTok),
			setup:    func(f *fakeOrchestrator) { f.files[testFileName] = []byte("0123456789abcdef") },
			want:     http.StatusOK,
			wantBody: "0123456789abcdef",
		},
		{
			name: "deregister", method: http.MethodPost,
			path: "/api/v1/workers/proud-heron-7/deregister", headers: bearer(),
			want:     http.StatusOK,
			wantBody: `{}`,
		},
		{
			name: "task not found", method: http.MethodGet,
			path: "/api/v1/tasks/missing-task", headers: taskAuth(testClaimTok),
			want:     http.StatusNotFound,
			wantBody: `{"error":{"code":"not_found","message":"dispatch: not found"}}`,
		},
		{
			name: "wrong claim token", method: http.MethodGet,
			path: "/api/v1/tasks/" + testTaskID, headers: taskAuth("bogus"),
			setup:    func(f *fakeOrchestrator) { f.claimToken = testClaimTok },
			want:     http.StatusForbidden,
			wantBody: `{"error":{"code":"forbidden","message":"dispatch: forbidden"}}`,
		},
		{
			name: "missing bearer", method: http.MethodPost,
			path: "/api/v1/register", body: goldenRegBody,
			want:     http.StatusUnauthorized,
			wantBody: `{"error":{"code":"unauthorized","message":"missing bearer token"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			if tc.setup != nil {
				tc.setup(f)
			}
			srv := newTestServer(t, f)
			status, body := rawRequest(t, srv, tc.method, tc.path, tc.headers, tc.body)
			if status != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", status, tc.want, body)
			}
			if body != tc.wantBody {
				t.Errorf("body mismatch\n got: %s\nwant: %s", body, tc.wantBody)
			}
		})
	}
}

// TestContractTaskDetailAllFields verifies the full TaskDetail round trip
// through the real client: every DESIGN §5.4 field survives, and the wire
// encoding equals the golden byte-for-byte.
func TestContractTaskDetailAllFields(t *testing.T) {
	f := newFake()
	f.tasks[testTaskID] = fullTaskDetail()
	f.claimToken = testClaimTok
	srv := newTestServer(t, f)
	client := NewClient(srv.URL, testToken)

	task, err := client.GetTask(context.Background(), testTaskID, testClaimTok)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if !reflect.DeepEqual(task, fullTaskDetail()) {
		t.Errorf("task mismatch\n got: %#v\nwant: %#v", task, fullTaskDetail())
	}
	got, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != goldenTaskDetailJSON {
		t.Errorf("wire encoding mismatch\n got: %s\nwant: %s", got, goldenTaskDetailJSON)
	}
}

// TestContractPollClaim verifies the poll → claim-token handoff (decision
// A10): the client receives the task plus the token to inject into the
// one-shot agent container.
func TestContractPollClaim(t *testing.T) {
	f := newFake()
	f.nextTask = fullTaskDetail()
	f.claimToken = testClaimTok
	srv := newTestServer(t, f)
	client := NewClient(srv.URL, testToken)

	resp, err := client.Poll(context.Background(), PollReq{Name: "n1", Arch: "x86_64"})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if resp.Task == nil || !reflect.DeepEqual(resp.Task, fullTaskDetail()) {
		t.Errorf("poll task mismatch: %#v", resp.Task)
	}
	if resp.ClaimToken != testClaimTok {
		t.Errorf("claim token = %q, want %q", resp.ClaimToken, testClaimTok)
	}
}

// TestContractLogOffsetAndCancel drives the log offset semantics through
// the client (DETAIL §9.7 item 5): mismatched offsets surface as 409 with
// the current offset, and the durable cancellation flag is passed through.
func TestContractLogOffsetAndCancel(t *testing.T) {
	f := newFake()
	f.claimToken = testClaimTok
	srv := newTestServer(t, f)
	client := NewClient(srv.URL, testToken)

	ack, err := client.AppendLog(context.Background(), testTaskID, testClaimTok,
		LogSegment{Offset: 0, Data: "==> Making package\n"})
	if err != nil {
		t.Fatalf("append log: %v", err)
	}
	if ack.Offset != logDataLen || ack.Cancelled {
		t.Errorf("ack = %#v, want offset %d cancelled false", ack, logDataLen)
	}

	// Mismatched offset: 409 with the current offset.
	_, err = client.AppendLog(context.Background(), testTaskID, testClaimTok,
		LogSegment{Offset: 3, Data: "zz"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.Status != http.StatusConflict || apiErr.Offset != logDataLen {
		t.Errorf("offset error = %#v, want 409 offset %d", apiErr, logDataLen)
	}

	// Cancel flag passthrough (channel 2, D4).
	f.mu.Lock()
	f.cancelled = true
	f.mu.Unlock()
	ack, err = client.AppendLog(context.Background(), testTaskID, testClaimTok,
		LogSegment{Offset: logDataLen, Data: "done"})
	if err != nil {
		t.Fatalf("append log after cancel: %v", err)
	}
	if !ack.Cancelled {
		t.Error("cancelled flag not passed through")
	}
	if ack.Offset != logDataLen+4 {
		t.Errorf("ack offset = %d, want %d", ack.Offset, logDataLen+4)
	}
}

// TestContractResultReport verifies the result report carries artifacts,
// resource usage and the actual commit (D1), and that a late duplicate
// report conflicts (decision A3).
func TestContractResultReport(t *testing.T) {
	f := newFake()
	f.claimToken = testClaimTok
	srv := newTestServer(t, f)
	client := NewClient(srv.URL, testToken)

	at := time.Date(2026, 8, 5, 10, 10, 0, 0, time.UTC)
	res := ResultReq{
		Status: "succeeded",
		Artifacts: []repo.Artifact{
			{File: "foo-1.2.3-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "foo", Version: "1.2.3-1", Arch: "x86_64", Size: 123456, SHA256: "deadbeef"},
			{File: "foo-1.2.3-1-x86_64.pkg.tar.zst.sig", Kind: "signature", Size: 566, SHA256: "beef"},
			{File: ".SRCINFO", Kind: "srcinfo", Size: 321, SHA256: "cafe"},
		},
		ResourceUsage: []db.Sample{{At: at, CPUTimeNS: 1, MemoryBytes: 2}},
		Commit:        "abc123",
	}
	if err := client.ReportResult(context.Background(), testTaskID, testClaimTok, res); err != nil {
		t.Fatalf("report result: %v", err)
	}
	if !reflect.DeepEqual(f.lastResult, res) {
		t.Errorf("result not forwarded intact\n got: %#v\nwant: %#v", f.lastResult, res)
	}

	// Late duplicate report → 409 conflict.
	err := client.ReportResult(context.Background(), testTaskID, testClaimTok, res)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Errorf("duplicate result error = %v, want 409", err)
	}
}

// TestContractSigningKeyOneTime verifies the one-shot signing key (DESIGN
// §7.7): the material arrives exactly once, a repeat claim conflicts.
func TestContractSigningKeyOneTime(t *testing.T) {
	f := newFake()
	f.claimToken = testClaimTok
	srv := newTestServer(t, f)
	client := NewClient(srv.URL, testToken)

	km, err := client.GetSigningKey(context.Background(), testTaskID, testClaimTok)
	if err != nil {
		t.Fatalf("get signing key: %v", err)
	}
	if km.KeyID != "ABCD1234" || km.ArmoredPrivateKey == "" || km.Passphrase != "secret" {
		t.Errorf("key material = %#v", km)
	}

	_, err = client.GetSigningKey(context.Background(), testTaskID, testClaimTok)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Errorf("repeat signing key error = %v, want 409", err)
	}
}

// TestContractUploadResume drives the resumable upload through the client
// (DETAIL §9.7 item 4): segment 1, a conflict carrying the current offset,
// resume from that offset, overwrite rejected.
func TestContractUploadResume(t *testing.T) {
	f := newFake()
	f.claimToken = testClaimTok
	srv := newTestServer(t, f)
	client := NewClient(srv.URL, testToken)
	name := "foo-1.2.3-1-x86_64.pkg.tar.zst"

	meta, err := client.UploadFile(context.Background(), testTaskID, testClaimTok,
		name, io.NopCloser(strings.NewReader("part1")), 5, 0)
	if err != nil {
		t.Fatalf("segment 1: %v", err)
	}
	if meta.Name != name || meta.Offset != 5 {
		t.Errorf("segment 1 meta = %#v", meta)
	}

	_, err = client.UploadFile(context.Background(), testTaskID, testClaimTok,
		name, io.NopCloser(strings.NewReader("zz")), 2, 2)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Offset != 5 {
		t.Errorf("wrong offset error = %#v, want 409 offset 5", apiErr)
	}

	meta, err = client.UploadFile(context.Background(), testTaskID, testClaimTok,
		name, io.NopCloser(strings.NewReader("part2")), 5, 5)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if meta.Offset != 10 {
		t.Errorf("resume offset = %d, want 10", meta.Offset)
	}

	_, err = client.UploadFile(context.Background(), testTaskID, testClaimTok,
		name, io.NopCloser(strings.NewReader("overwrite")), 9, 0)
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Errorf("overwrite error = %v, want 409", err)
	}
}

// TestContractHeartbeatCancelSignal verifies the heartbeat carries the
// cancellation signals (channel 1, DESIGN §7.8 / D4) and the server time.
func TestContractHeartbeatCancelSignal(t *testing.T) {
	f := newFake()
	f.hookHeartbeat = func(hb HeartbeatReq) (*HeartbeatResp, error) {
		return &HeartbeatResp{
			CancelledTaskIDs: []string{"t-cancel-1", "t-cancel-2"},
			ServerTime:       f.serverTime,
		}, nil
	}
	srv := newTestServer(t, f)
	client := NewClient(srv.URL, testToken)

	resp, err := client.Heartbeat(context.Background(), HeartbeatReq{Name: "n1"})
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if !reflect.DeepEqual(resp.CancelledTaskIDs, []string{"t-cancel-1", "t-cancel-2"}) {
		t.Errorf("cancelled ids = %v", resp.CancelledTaskIDs)
	}
	if !resp.ServerTime.Equal(f.serverTime) {
		t.Errorf("server time = %v, want %v", resp.ServerTime, f.serverTime)
	}
}
