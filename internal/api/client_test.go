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

package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/dispatch"
	"git.0x0f.dev/varve/internal/sign"
)

// newTestClient builds a real Client pointed at the test server.
func newTestClient(srvURL string) *Client {
	return NewClient(srvURL, testToken)
}

// TestClientHeartbeatRetry verifies the idempotent retry semantics
// (DETAIL §9.7 item 7): a transient 500 on heartbeat is retried with a
// fixed 1s interval and succeeds on the second attempt.
func TestClientHeartbeatRetry(t *testing.T) {
	orig := retryInterval
	retryInterval = time.Millisecond
	defer func() { retryInterval = orig }()

	f := newFake()
	f.hookHeartbeat = func(hb HeartbeatReq) (*HeartbeatResp, error) {
		if f.calls["heartbeat"] <= 1 {
			return nil, errors.New("transient failure")
		}
		return &HeartbeatResp{ServerTime: f.serverTime}, nil
	}
	srv := newTestServer(t, f)
	client := newTestClient(srv.URL)

	resp, err := client.Heartbeat(context.Background(), HeartbeatReq{Name: "n1"})
	if err != nil {
		t.Fatalf("heartbeat after retry: %v", err)
	}
	if f.calls["heartbeat"] != 2 {
		t.Errorf("heartbeat calls = %d, want 2", f.calls["heartbeat"])
	}
	if !resp.ServerTime.Equal(f.serverTime) {
		t.Errorf("server time = %v, want %v", resp.ServerTime, f.serverTime)
	}
}

// TestClientRegisterNoRetry verifies non-idempotent requests are never
// retried: a single 500 returns an APIError immediately.
func TestClientRegisterNoRetry(t *testing.T) {
	orig := retryInterval
	retryInterval = time.Millisecond
	defer func() { retryInterval = orig }()

	f := newFake()
	f.hookRegister = func(reg RegisterReq) (*RegisterResp, error) {
		return nil, errors.New("boom")
	}
	srv := newTestServer(t, f)
	client := newTestClient(srv.URL)

	_, err := client.Register(context.Background(), RegisterReq{Name: "n1"})
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", apiErr.Status)
	}
	if apiErr.Code != codeInternal {
		t.Errorf("code = %q, want %q", apiErr.Code, codeInternal)
	}
	if f.calls["register"] != 1 {
		t.Errorf("register calls = %d, want 1 (no retry)", f.calls["register"])
	}
}

// TestClientAppendLogRetry verifies log append retries like heartbeat.
func TestClientAppendLogRetry(t *testing.T) {
	orig := retryInterval
	retryInterval = time.Millisecond
	defer func() { retryInterval = orig }()

	f := newFake()
	f.claimToken = testClaimTok
	f.hookAppendLog = func(taskID, token string, seg LogSegment) (*LogAck, error) {
		if f.calls["appendlog"] <= 1 {
			return nil, errors.New("transient failure")
		}
		return &LogAck{Offset: int64(len(seg.Data)), Cancelled: false}, nil
	}
	srv := newTestServer(t, f)
	client := NewClient(srv.URL, testToken)

	ack, err := client.AppendLog(context.Background(), testTaskID, testClaimTok,
		LogSegment{Offset: 0, Data: "line1"})
	if err != nil {
		t.Fatalf("append log after retry: %v", err)
	}
	if f.calls["appendlog"] != 2 {
		t.Errorf("appendlog calls = %d, want 2", f.calls["appendlog"])
	}
	if ack.Offset != 5 {
		t.Errorf("ack offset = %d, want 5", ack.Offset)
	}
}

// TestClientUploadRetry verifies an upload is retried with a re-readable
// source (io.Seeker): the first attempt fails with 500, the source is
// rewound and the second attempt stores the full content.
func TestClientUploadRetry(t *testing.T) {
	orig := retryInterval
	retryInterval = time.Millisecond
	defer func() { retryInterval = orig }()

	f := newFake()
	f.claimToken = testClaimTok
	payload := []byte("0123456789abcdef")
	f.hookUpload = func(taskID, token, name string, r io.Reader, size, offset int64) (*FileMeta, error) {
		if f.calls["upload"] <= 1 {
			return nil, errors.New("transient failure")
		}
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		f.files[name] = data
		return &FileMeta{Name: name, Offset: int64(len(data))}, nil
	}
	srv := newTestServer(t, f)
	client := NewClient(srv.URL, testToken)

	meta, err := client.UploadFile(context.Background(), testTaskID, testClaimTok,
		"file.bin", bytes.NewReader(payload), int64(len(payload)), 0)
	if err != nil {
		t.Fatalf("upload after retry: %v", err)
	}
	if f.calls["upload"] != 2 {
		t.Errorf("upload calls = %d, want 2", f.calls["upload"])
	}
	if meta.Offset != int64(len(payload)) {
		t.Errorf("meta offset = %d, want %d", meta.Offset, len(payload))
	}
	if got := string(f.files["file.bin"]); got != string(payload) {
		t.Errorf("staged content = %q, want %q", got, payload)
	}
}

// TestClientUploadResumeOffset verifies the resume channel: a 409 conflict
// surfaces as *APIError carrying the current server offset (DESIGN §5.3).
func TestClientUploadResumeOffset(t *testing.T) {
	f := newFake()
	f.claimToken = testClaimTok
	srv := newTestServer(t, f)
	client := NewClient(srv.URL, testToken)

	// First segment lands at offset 5.
	if _, err := client.UploadFile(context.Background(), testTaskID, testClaimTok,
		"f.bin", strings.NewReader("part1"), 5, 0); err != nil {
		t.Fatalf("segment 1: %v", err)
	}
	// Resuming at a wrong offset yields 409 with the current offset.
	_, err := client.UploadFile(context.Background(), testTaskID, testClaimTok,
		"f.bin", strings.NewReader("zz"), 2, 2)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.Status != http.StatusConflict || apiErr.Code != codeConflict {
		t.Errorf("api error = %#v, want 409 conflict", apiErr)
	}
	if apiErr.Offset != 5 {
		t.Errorf("offset = %d, want 5", apiErr.Offset)
	}
}

// TestClientDownloadStreams verifies DownloadFile returns a streaming body
// and surfaces 404 as an APIError.
func TestClientDownloadStreams(t *testing.T) {
	f := newFake()
	f.claimToken = testClaimTok
	f.files[testFileName] = []byte("streamed-bytes")
	srv := newTestServer(t, f)
	client := NewClient(srv.URL, testToken)

	rc, err := client.DownloadFile(context.Background(), testTaskID, testClaimTok, testFileName)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(data) != "streamed-bytes" {
		t.Errorf("body = %q, want %q", data, "streamed-bytes")
	}

	_, err = client.DownloadFile(context.Background(), testTaskID, testClaimTok, "missing.bin")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Errorf("missing download error = %v, want 404 APIError", err)
	}
}

// TestClientGetSigningKey verifies the signing-key wire contract: the
// response is snake_case on the wire and the client returns a *KeyMaterial
// alias value.
func TestClientGetSigningKey(t *testing.T) {
	f := newFake()
	f.claimToken = testClaimTok
	srv := newTestServer(t, f)
	client := NewClient(srv.URL, testToken)

	km, err := client.GetSigningKey(context.Background(), testTaskID, testClaimTok)
	if err != nil {
		t.Fatalf("get signing key: %v", err)
	}
	var viaSign *sign.KeyMaterial = km
	if viaSign.KeyID != "ABCD1234" || viaSign.Passphrase != "secret" {
		t.Errorf("key material = %#v", viaSign)
	}
}

// TestClientContextCancellation verifies ctx propagation: a canceled
// context aborts the request without retrying.
func TestClientContextCancellation(t *testing.T) {
	f := newFake()
	srv := newTestServer(t, f)
	client := newTestClient(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Register(ctx, RegisterReq{Name: "n1"})
	if err == nil {
		t.Fatal("expected error with canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	// The canceled context aborts before the request is sent.
	if f.calls["register"] != 0 {
		t.Errorf("register calls = %d, want 0 (request never sent)", f.calls["register"])
	}
}

// TestClientDeregister verifies the deregister round trip.
func TestClientDeregister(t *testing.T) {
	f := newFake()
	srv := newTestServer(t, f)
	client := newTestClient(srv.URL)

	if err := client.Deregister(context.Background(), "proud-heron-7"); err != nil {
		t.Fatalf("deregister: %v", err)
	}
	if f.lastDeregister != "proud-heron-7" {
		t.Errorf("deregister name = %q", f.lastDeregister)
	}
}

// TestClientAPIErrorMessages verifies APIError carries the wire code and
// message verbatim.
func TestClientAPIErrorMessages(t *testing.T) {
	f := newFake()
	f.hookRegister = func(reg RegisterReq) (*RegisterResp, error) {
		return nil, dispatch.ErrConflict
	}
	srv := newTestServer(t, f)
	client := newTestClient(srv.URL)

	_, err := client.Register(context.Background(), RegisterReq{Name: "n1"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.Status != http.StatusConflict || apiErr.Code != codeConflict {
		t.Errorf("api error = %#v", apiErr)
	}
	if !strings.Contains(apiErr.Message, "conflict") {
		t.Errorf("message = %q, want it to mention conflict", apiErr.Message)
	}
}
