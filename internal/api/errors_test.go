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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/dispatch"
	"git.0x0f.dev/varve/internal/sign"
)

// errorBody is the decoded shape of an error response plus the optional
// resume offset.
type errorBody struct {
	Error  errorDetail `json:"error"`
	Offset *int64      `json:"offset"`
}

// TestSentinelErrorMapping drives the full sentinel → HTTP table through
// the register endpoint: each error returned by the orchestrator must
// surface as the mapped status with the {"error":{"code","message"}}
// body.
func TestSentinelErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"db not found", db.ErrNotFound, http.StatusNotFound, codeNotFound},
		{"dispatch not found", dispatch.ErrNotFound, http.StatusNotFound, codeNotFound},
		{"forbidden", dispatch.ErrForbidden, http.StatusForbidden, codeForbidden},
		{"conflict", dispatch.ErrConflict, http.StatusConflict, codeConflict},
		{"wrapped conflict", fmt.Errorf("ingest: %w", dispatch.ErrConflict), http.StatusConflict, codeConflict},
		{"sign already exported", sign.ErrAlreadyExported, http.StatusConflict, codeConflict},
		{"unknown error", errors.New("boom"), http.StatusInternalServerError, codeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.hookRegister = func(reg RegisterReq) (*RegisterResp, error) {
				return nil, tc.err
			}
			srv := newTestServer(t, f)

			status, body := rawRequest(t, srv, http.MethodPost, "/api/v1/register", bearer(), registerBody)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", status, tc.wantStatus, body)
			}
			var eb errorBody
			if err := json.Unmarshal([]byte(body), &eb); err != nil {
				t.Fatalf("body is not JSON: %v (%s)", err, body)
			}
			if eb.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", eb.Error.Code, tc.wantCode)
			}
			if eb.Error.Message == "" {
				t.Error("message must not be empty")
			}
			if tc.wantStatus == http.StatusInternalServerError {
				if strings.Contains(eb.Error.Message, "boom") {
					t.Error("internal errors must not leak the underlying error")
				}
			}
		})
	}
}

// TestOffsetConflictCarriesOffset verifies a 409 from
// dispatch.OffsetError carries the current server-side offset for
// resumable uploads.
func TestOffsetConflictCarriesOffset(t *testing.T) {
	f := newFake()
	f.hookUpload = func(taskID, token, name string, r io.Reader, size, offset int64) (*FileMeta, error) {
		return nil, &dispatch.OffsetError{Current: 42}
	}
	srv := newTestServer(t, f)

	status, body := rawRequest(t, srv, http.MethodPut,
		"/api/v1/tasks/"+testTaskID+"/files/foo.bin?offset=7", taskAuth(testClaimTok), "abc")
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", status, body)
	}
	var eb errorBody
	if err := json.Unmarshal([]byte(body), &eb); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	if eb.Error.Code != codeConflict {
		t.Errorf("code = %q, want %q", eb.Error.Code, codeConflict)
	}
	if eb.Offset == nil || *eb.Offset != 42 {
		t.Errorf("offset = %v, want 42", eb.Offset)
	}
}

// TestDecodeErrors400 drives the 400 invalid_request paths: malformed
// JSON, unknown fields (with the field name in the message), type
// mismatches and empty bodies.
func TestDecodeErrors400(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantField string
	}{
		{"malformed json", `{"name":`, ""},
		{"unknown field", `{"name":"n","bogus_field":1,"role":"host","mode":"host","arch":"x86_64","capacity":1,"version":"0.1.0"}`, "bogus_field"},
		{"type mismatch", `{"name":"n","capacity":"two","role":"host","mode":"host","arch":"x86_64","version":"0.1.0"}`, "capacity"},
		{"empty body", "", ""},
		{"array body", `[1,2,3]`, ""},
		{"trailing garbage", registerBody + ` xyz`, ""},
		{"multiple json values", registerBody + registerBody, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			srv := newTestServer(t, f)
			status, body := rawRequest(t, srv, http.MethodPost, "/api/v1/register", bearer(), tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", status, body)
			}
			var eb errorBody
			if err := json.Unmarshal([]byte(body), &eb); err != nil {
				t.Fatalf("body is not JSON: %v (%s)", err, body)
			}
			if eb.Error.Code != codeInvalidRequest {
				t.Errorf("code = %q, want %q", eb.Error.Code, codeInvalidRequest)
			}
			if tc.wantField != "" && !strings.Contains(eb.Error.Message, tc.wantField) {
				t.Errorf("message %q does not name field %q", eb.Error.Message, tc.wantField)
			}
		})
	}
}

// TestBadUploadParams400 verifies invalid offset parameters and invalid
// file names on the upload endpoint. Path traversal via ".." is defeated
// at the router level (ServeMux normalizes dot segments before any handler
// runs), so it surfaces as 404 without ever reaching the orchestrator.
func TestBadUploadParams400(t *testing.T) {
	f := newFake()
	srv := newTestServer(t, f)

	paths := []string{
		"/api/v1/tasks/" + testTaskID + "/files/foo.bin?offset=abc",
		"/api/v1/tasks/" + testTaskID + "/files/foo.bin?offset=-1",
		"/api/v1/tasks/" + testTaskID + "/files/foo.bin?offset=99999999999999999999999",
		"/api/v1/tasks/" + testTaskID + "/files/a%2Fb",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			status, body := rawRequest(t, srv, http.MethodPut, p, taskAuth(testClaimTok), "data")
			if status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", status, body)
			}
		})
	}

	// Dot segments are normalized by the router: rejected before the
	// handler, and the orchestrator is never called.
	status, body := rawRequest(t, srv, http.MethodPut,
		"/api/v1/tasks/"+testTaskID+"/files/../escape", taskAuth(testClaimTok), "data")
	if status != http.StatusNotFound {
		t.Errorf("traversal status = %d, want 404 (body %s)", status, body)
	}
	if f.calls["upload"] != 0 {
		t.Errorf("traversal reached the orchestrator: %d calls", f.calls["upload"])
	}
}
