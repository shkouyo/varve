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
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/repo"
)

// TestValidTaskID covers the task id path-value charset and length bound.
func TestValidTaskID(t *testing.T) {
	valid := []string{
		"task-0001",
		"8a1c5b2d-6f2a-4c3b-9e7d-1f0a2b3c4d5e", // UUID v4 shape
		"TASK_1",
	}
	for _, s := range valid {
		if !validTaskID(s) {
			t.Errorf("validTaskID(%q) = false, want true", s)
		}
	}
	invalid := []string{
		"",
		"a/b",
		"a b",
		"a.b",
		"a\nb",
		"a\x7fb",
		"a-b\x00c",
		strings.Repeat("a", maxTaskIDLen+1),
	}
	for _, s := range invalid {
		if validTaskID(s) {
			t.Errorf("validTaskID(%q) = true, want false", s)
		}
	}
}

// TestValidToken covers the printable-ASCII worker-name bound.
func TestValidToken(t *testing.T) {
	if !validToken("proud-heron-7", maxWorkerNameLen) {
		t.Error("validToken(proud-heron-7) = false, want true")
	}
	for _, s := range []string{
		"",
		"has space",
		"tab\there",
		"ctl\x01",
		"unicode-\u00e9",
		strings.Repeat("x", maxWorkerNameLen+1),
	} {
		if validToken(s, maxWorkerNameLen) {
			t.Errorf("validToken(%q) = true, want false", s)
		}
	}
}

// TestValidateLogSegment asserts the log batch size cap.
func TestValidateLogSegment(t *testing.T) {
	if err := validateLogSegment(&LogSegment{Data: strings.Repeat("x", maxLogSegmentLen)}); err != nil {
		t.Errorf("segment at the cap: %v", err)
	}
	if err := validateLogSegment(&LogSegment{Data: strings.Repeat("x", maxLogSegmentLen+1)}); err == nil {
		t.Error("oversized segment accepted, want error")
	}
}

// TestValidateResultReq asserts the result payload bounds.
func TestValidateResultReq(t *testing.T) {
	if err := validateResultReq(&ResultReq{
		Status: "failed",
		Error:  &ResultError{Stage: "makepkg", Summary: strings.Repeat("s", maxSummaryLen)},
	}); err != nil {
		t.Errorf("result at the caps: %v", err)
	}
	if err := validateResultReq(&ResultReq{
		Status: "failed",
		Error:  &ResultError{Stage: "makepkg", Summary: strings.Repeat("s", maxSummaryLen+1)},
	}); err == nil {
		t.Error("oversized summary accepted, want error")
	}
	if err := validateResultReq(&ResultReq{
		Status: strings.Repeat("s", maxStatusLen+1),
	}); err == nil {
		t.Error("oversized status accepted, want error")
	}
	// Package artifacts feed repo-add/repo-remove positionally: the
	// pkgname and file name must survive the shared name rule and the
	// upload whitelist.
	for name, mutate := range map[string]func(*ResultReq){
		"dash pkgname": func(r *ResultReq) {
			r.Artifacts = []repo.Artifact{{File: "a-1-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "-q"}}
		},
		"nested pkgname": func(r *ResultReq) {
			r.Artifacts = []repo.Artifact{{File: "a-1-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "a/b"}}
		},
		"non-ascii pkgname": func(r *ResultReq) {
			r.Artifacts = []repo.Artifact{{File: "a-1-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "é"}}
		},
		"empty pkgname": func(r *ResultReq) {
			r.Artifacts = []repo.Artifact{{File: "a-1-1-x86_64.pkg.tar.zst", Kind: "package"}}
		},
		"dash file name": func(r *ResultReq) {
			r.Artifacts = []repo.Artifact{{File: "-a-1-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "a"}}
		},
	} {
		r := ResultReq{Status: "succeeded"}
		mutate(&r)
		if err := validateResultReq(&r); err == nil {
			t.Errorf("result with %s accepted, want error", name)
		}
	}
}

// TestValidateRegisterReq asserts the register payload bounds.
func TestValidateRegisterReq(t *testing.T) {
	ok := RegisterReq{Name: "proud-heron-7", Role: "agent", Mode: "pool", Arch: "x86_64", Version: "1.0"}
	if err := validateRegisterReq(&ok); err != nil {
		t.Errorf("valid register: %v", err)
	}
	for name, mutate := range map[string]func(*RegisterReq){
		"empty name":      func(r *RegisterReq) { r.Name = "" },
		"name with space": func(r *RegisterReq) { r.Name = "a b" },
		"long name":       func(r *RegisterReq) { r.Name = strings.Repeat("n", maxWorkerNameLen+1) },
		"long role":       func(r *RegisterReq) { r.Role = strings.Repeat("r", maxLabelLen+1) },
	} {
		r := ok
		mutate(&r)
		if err := validateRegisterReq(&r); err == nil {
			t.Errorf("register with %s accepted, want error", name)
		}
	}
}

// TestPathParamRejection asserts malformed path values are rejected with
// 400 before the orchestrator is consulted.
func TestPathParamRejection(t *testing.T) {
	f := newFake()
	srv := newTestServer(t, f)

	// A task id with a space (URL-encoded in the path) is invalid on the
	// GET task route and on the POST log route.
	if status, _ := rawRequest(t, srv, http.MethodGet, "/api/v1/tasks/task%20id", taskAuth(testClaimTok), ""); status != http.StatusBadRequest {
		t.Errorf("GET task with space = %d, want 400", status)
	}
	if status, _ := rawRequest(t, srv, http.MethodPost, "/api/v1/tasks/"+strings.Repeat("a", maxTaskIDLen+1)+"/log", taskAuth(testClaimTok), "{}"); status != http.StatusBadRequest {
		t.Errorf("POST log with long id = %d, want 400", status)
	}

	// Worker name with a space is invalid on deregister.
	status, _ := rawRequest(t, srv, http.MethodPost, "/api/v1/workers/bad%20name/deregister", bearer(), "")
	if status != http.StatusBadRequest {
		t.Errorf("deregister bad name = %d, want 400", status)
	}
}

// TestUploadSizeLimits asserts the upload size caps reject oversized
// requests up front. The bodies are declared via ContentLength only (the
// handler is served in-process, so the caps fire before any bytes are
// read and no large payload is sent).
func TestUploadSizeLimits(t *testing.T) {
	f := newFake()
	cfg := &config.ControllerConfig{API: config.APIConfig{Token: testToken}}
	h := NewServer(cfg, f).Handler()

	send := func(path string, length int64) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader("x"))
		req.ContentLength = length
		req.Header.Set(taskTokenHeader, testClaimTok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	base := "/api/v1/tasks/" + testTaskID + "/files/"

	// Segment larger than the per-request cap.
	if status := send(base+"big.bin", maxUploadSegment+1); status != http.StatusBadRequest {
		t.Errorf("oversized segment = %d, want 400", status)
	}

	// A resume offset that would push the staged file past the total cap.
	if status := send(base+"big.bin?offset="+strconv.FormatInt(maxUploadTotal, 10), 1); status != http.StatusBadRequest {
		t.Errorf("oversized total = %d, want 400", status)
	}

	// File name longer than the cap.
	if status := send(base+strings.Repeat("n", maxFileNameLen+1), 1); status != http.StatusBadRequest {
		t.Errorf("long file name = %d, want 400", status)
	}

	// A valid-size upload still reaches the orchestrator.
	req := httptest.NewRequest(http.MethodPut, base+"ok.bin", strings.NewReader("abc"))
	req.Header.Set(taskTokenHeader, testClaimTok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("ok upload = %d, want 200", rec.Code)
	}
}
