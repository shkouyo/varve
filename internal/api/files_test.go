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
	"crypto/sha256"
	"encoding/json"
	"math/rand"
	"net/http"
	"strings"
	"testing"
)

const testFileName = "foo-1.2.3-1-x86_64.pkg.tar.zst"

// TestUploadResume exercises the segmented upload contract (DESIGN §5.3,
// optimization O1): a 409 with the current offset lets the worker resume
// from there; overwriting an existing offset is rejected.
func TestUploadResume(t *testing.T) {
	f := newFake()
	srv := newTestServer(t, f)
	base := "/api/v1/tasks/" + testTaskID + "/files/" + testFileName

	// Segment 1: whole-file upload from 0.
	status, body := rawRequest(t, srv, http.MethodPut, base, taskAuth(testClaimTok), "part1")
	if status != http.StatusOK {
		t.Fatalf("segment 1: status = %d (body %s)", status, body)
	}
	var meta FileMeta
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		t.Fatalf("segment 1 body not FileMeta JSON: %v (%s)", err, body)
	}
	if meta.Name != testFileName || meta.Offset != 5 {
		t.Errorf("segment 1 meta = %#v, want name %q offset 5", meta, testFileName)
	}

	// Wrong offset: 409 carrying the current offset for resume.
	status, body = rawRequest(t, srv, http.MethodPut, base+"?offset=3", taskAuth(testClaimTok), "zz")
	if status != http.StatusConflict {
		t.Fatalf("wrong offset: status = %d (body %s)", status, body)
	}
	var eb errorBody
	if err := json.Unmarshal([]byte(body), &eb); err != nil {
		t.Fatalf("wrong offset body not JSON: %v (%s)", err, body)
	}
	if eb.Error.Code != codeConflict || eb.Offset == nil || *eb.Offset != 5 {
		t.Errorf("wrong offset: code = %q offset = %v, want conflict/5", eb.Error.Code, eb.Offset)
	}

	// Resume from the reported offset: segment 2.
	status, body = rawRequest(t, srv, http.MethodPut, base+"?offset=5", taskAuth(testClaimTok), "part2")
	if status != http.StatusOK {
		t.Fatalf("segment 2: status = %d (body %s)", status, body)
	}
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		t.Fatalf("segment 2 body not FileMeta JSON: %v (%s)", err, body)
	}
	if meta.Offset != 10 {
		t.Errorf("segment 2 offset = %d, want 10", meta.Offset)
	}

	// Overwrite attempt (offset 0 again) is rejected.
	status, body = rawRequest(t, srv, http.MethodPut, base, taskAuth(testClaimTok), "overwrite")
	if status != http.StatusConflict {
		t.Fatalf("overwrite: status = %d (body %s)", status, body)
	}
	if err := json.Unmarshal([]byte(body), &eb); err != nil {
		t.Fatalf("overwrite body not JSON: %v (%s)", err, body)
	}
	if eb.Offset == nil || *eb.Offset != 10 {
		t.Errorf("overwrite offset = %v, want 10", eb.Offset)
	}

	if got := string(f.files[testFileName]); got != "part1part2" {
		t.Errorf("staged content = %q, want %q", got, "part1part2")
	}
}

// TestDownloadStreams verifies the download endpoint returns the staged
// bytes and 404s for missing files.
func TestDownloadStreams(t *testing.T) {
	f := newFake()
	f.files[testFileName] = []byte("0123456789")
	srv := newTestServer(t, f)
	base := "/api/v1/tasks/" + testTaskID + "/files/"

	status, body := rawRequest(t, srv, http.MethodGet, base+testFileName, taskAuth(testClaimTok), "")
	if status != http.StatusOK {
		t.Fatalf("download: status = %d (body %q)", status, body)
	}
	if body != "0123456789" {
		t.Errorf("download body = %q, want %q", body, "0123456789")
	}

	status, body = rawRequest(t, srv, http.MethodGet, base+"missing.bin", taskAuth(testClaimTok), "")
	if status != http.StatusNotFound {
		t.Fatalf("missing: status = %d (body %s)", status, body)
	}
}

// TestLargeUploadStreams asserts the streaming property (DESIGN §4.3,
// DETAIL §9.7 item 6): a 10 MiB upload must never be loaded into memory as
// a whole — the fake records the largest read chunk it received — and the
// staged content must hash identically to the source.
func TestLargeUploadStreams(t *testing.T) {
	const size = 10 << 20 // 10 MiB

	rng := rand.New(rand.NewSource(42))
	data := make([]byte, size)
	if _, err := rng.Read(data); err != nil {
		t.Fatalf("generate payload: %v", err)
	}
	wantHash := sha256.Sum256(data)

	f := newFake()
	srv := newTestServer(t, f)
	base := "/api/v1/tasks/" + testTaskID + "/files/big.bin"

	status, body := rawRequest(t, srv, http.MethodPut, base, taskAuth(testClaimTok), string(data))
	if status != http.StatusOK {
		t.Fatalf("upload: status = %d (body %s)", status, body)
	}
	var meta FileMeta
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		t.Fatalf("body not FileMeta JSON: %v (%s)", err, body)
	}
	if meta.Offset != size {
		t.Errorf("offset = %d, want %d", meta.Offset, size)
	}

	f.mu.Lock()
	got := append([]byte(nil), f.files["big.bin"]...)
	peak := f.uploadPeak
	total := f.uploadTotal
	f.mu.Unlock()

	gotHash := sha256.Sum256(got)
	if gotHash != wantHash {
		t.Error("staged content hash mismatch")
	}
	if total != size {
		t.Errorf("total streamed = %d, want %d", total, size)
	}
	// The server must stream: the largest single read chunk stays well
	// below the whole file (a handler that buffered the body would show a
	// single read of the full size), and below a 4 MiB per-read bound.
	if peak >= size {
		t.Errorf("largest read chunk %d equals the whole file: body was buffered", peak)
	}
	if peak > 4<<20 {
		t.Errorf("largest read chunk %d exceeds the 4 MiB streaming bound", peak)
	}
}

// TestUploadDownloadRoundTrip drives a full upload → download round trip
// through the wire with multi-byte content.
func TestUploadDownloadRoundTrip(t *testing.T) {
	f := newFake()
	srv := newTestServer(t, f)
	base := "/api/v1/tasks/" + testTaskID + "/files/"

	payload := strings.Repeat("varve-构建-", 100)
	status, body := rawRequest(t, srv, http.MethodPut, base+".SRCINFO", taskAuth(testClaimTok), payload)
	if status != http.StatusOK {
		t.Fatalf("upload: status = %d (body %s)", status, body)
	}

	status, body = rawRequest(t, srv, http.MethodGet, base+".SRCINFO", taskAuth(testClaimTok), "")
	if status != http.StatusOK {
		t.Fatalf("download: status = %d (body %q)", status, body)
	}
	if body != payload {
		t.Errorf("round trip mismatch: got %d bytes, want %d", len(body), len(payload))
	}
}

// TestDownloadRequiresTaskToken guards the download endpoint with the
// task-level auth matrix.
func TestDownloadRequiresTaskToken(t *testing.T) {
	f := newFake()
	f.files[testFileName] = []byte("x")
	srv := newTestServer(t, f)

	status, _ := rawRequest(t, srv, http.MethodGet,
		"/api/v1/tasks/"+testTaskID+"/files/"+testFileName, nil, "")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", status)
	}
}
