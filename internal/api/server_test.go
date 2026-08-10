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
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/config"
)

const registerBody = `{"name":"n1","role":"host","mode":"host","arch":"x86_64","capacity":1,"version":"0.1.0"}`

// TestAuthMatrixNodeLevel covers the node-level Bearer matrix:
// missing/wrong/correct tokens on register.
func TestAuthMatrixNodeLevel(t *testing.T) {
	f := newFake()
	srv := newTestServer(t, f)

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no bearer", nil, http.StatusUnauthorized},
		{"malformed scheme", map[string]string{"Authorization": "Basic abc"}, http.StatusUnauthorized},
		{"empty bearer", map[string]string{"Authorization": "Bearer "}, http.StatusUnauthorized},
		{"wrong bearer", map[string]string{"Authorization": "Bearer wrong-token"}, http.StatusUnauthorized},
		{"correct bearer", bearer(), http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := rawRequest(t, srv, http.MethodPost, "/api/v1/register", tc.headers, registerBody)
			if status != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", status, tc.want, body)
			}
			if tc.want == http.StatusOK {
				return
			}
			var resp errorResponse
			if err := json.Unmarshal([]byte(body), &resp); err != nil {
				t.Fatalf("error body is not JSON: %v (%s)", err, body)
			}
			if resp.Error.Code != codeUnauthorized {
				t.Errorf("code = %q, want %q", resp.Error.Code, codeUnauthorized)
			}
		})
	}
}

// TestBearerAuthEmptyToken asserts a server constructed with an empty api
// token rejects every node-level request with 401 and never reaches the
// orchestrator. Production config validation already requires a token;
// this is the api layer's own defense in depth.
func TestBearerAuthEmptyToken(t *testing.T) {
	f := newFake()
	cfg := &config.ControllerConfig{} // empty API token
	srv := httptest.NewServer(NewServer(cfg, f).Handler())
	t.Cleanup(srv.Close)

	for _, tok := range []string{"", "some-token"} {
		status, body := rawRequest(t, srv, http.MethodPost, "/api/v1/register",
			map[string]string{"Authorization": "Bearer " + tok}, registerBody)
		if status != http.StatusUnauthorized {
			t.Errorf("Bearer %q: status = %d, want 401 (body %s)", tok, status, body)
		}
	}
	if f.calls["register"] != 0 {
		t.Errorf("register reached the orchestrator %d times with an empty token config", f.calls["register"])
	}
}

// TestAuthMatrixTaskLevel covers the claim-token matrix on a task-level
// endpoint (GET /tasks/{id}): missing token is rejected by the middleware,
// a wrong token is rejected by the orchestrator (fake) as forbidden, the
// correct token succeeds.
func TestAuthMatrixTaskLevel(t *testing.T) {
	f := newFake()
	f.claimToken = testClaimTok
	f.tasks[testTaskID] = fullTaskDetail()
	srv := newTestServer(t, f)

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no token", nil, http.StatusForbidden},
		{"empty token", taskAuth(""), http.StatusForbidden},
		{"wrong token", taskAuth("bogus"), http.StatusForbidden},
		{"correct token", taskAuth(testClaimTok), http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := rawRequest(t, srv, http.MethodGet, "/api/v1/tasks/"+testTaskID, tc.headers, "")
			if status != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", status, tc.want, body)
			}
			if tc.want == http.StatusOK {
				return
			}
			var resp errorResponse
			if err := json.Unmarshal([]byte(body), &resp); err != nil {
				t.Fatalf("error body is not JSON: %v (%s)", err, body)
			}
			if resp.Error.Code != codeForbidden {
				t.Errorf("code = %q, want %q", resp.Error.Code, codeForbidden)
			}
		})
	}
}

// TestAuthMatrixDualToken verifies the two token classes never interfere:
// a node-level endpoint ignores a stray task-token header, and a task-level
// endpoint ignores a stray Authorization header.
func TestAuthMatrixDualToken(t *testing.T) {
	f := newFake()
	f.claimToken = testClaimTok
	f.tasks[testTaskID] = fullTaskDetail()
	srv := newTestServer(t, f)

	// Node-level: correct Bearer + garbage task token still succeeds.
	headers := bearer()
	headers[taskTokenHeader] = "garbage"
	status, body := rawRequest(t, srv, http.MethodPost, "/api/v1/register", headers, registerBody)
	if status != http.StatusOK {
		t.Fatalf("register with both headers: status = %d (body %s)", status, body)
	}

	// Task-level: correct task token + garbage Bearer still succeeds.
	headers = taskAuth(testClaimTok)
	headers["Authorization"] = "Bearer garbage"
	status, body = rawRequest(t, srv, http.MethodGet, "/api/v1/tasks/"+testTaskID, headers, "")
	if status != http.StatusOK {
		t.Fatalf("get task with both headers: status = %d (body %s)", status, body)
	}
}

// TestUploadNameWhitelist drives the filename whitelist table: valid
// basenames upload, anything outside [A-Za-z0-9._+-] is rejected with
// 400, including path separators.
func TestUploadNameWhitelist(t *testing.T) {
	f := newFake()
	srv := newTestServer(t, f)

	valid := []string{
		"foo-1.2.3-1-x86_64.pkg.tar.zst",
		".SRCINFO",
		"a+b_c.d",
		"0-9_A.Z+",
		"A",
	}
	for _, name := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			status, body := rawRequest(t, srv, http.MethodPut,
				"/api/v1/tasks/"+testTaskID+"/files/"+url.PathEscape(name),
				taskAuth(testClaimTok), "payload")
			if status != http.StatusOK {
				t.Errorf("upload %q: status = %d (body %s)", name, status, body)
			}
		})
	}

	invalid := []string{
		"a/b",  // forward slash
		"a\\b", // backslash
		"a b",  // space
		"a,b",  // comma
		"a;b",  // semicolon
		"a:b",  // colon
		"é",    // non-ASCII
		"a?b",  // query meta
		"a#b",  // fragment meta
		"-q",   // leading dash: parsed as an option by repo tools
		"-foo.pkg.tar.zst",
	}
	// Note: "..", "." and "" never reach the handler: ServeMux
	// normalizes dot segments and trailing slashes at the router level, so
	// they are covered by TestBadUploadParams400 instead.
	for _, name := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			status, body := rawRequest(t, srv, http.MethodPut,
				"/api/v1/tasks/"+testTaskID+"/files/"+url.PathEscape(name),
				taskAuth(testClaimTok), "payload")
			if status != http.StatusBadRequest {
				t.Errorf("upload %q: status = %d, want 400 (body %s)", name, status, body)
			}
			var resp errorResponse
			if err := json.Unmarshal([]byte(body), &resp); err != nil {
				t.Fatalf("error body is not JSON: %v (%s)", err, body)
			}
			if resp.Error.Code != codeInvalidRequest {
				t.Errorf("code = %q, want %q", resp.Error.Code, codeInvalidRequest)
			}
		})
	}
}

// TestMethodMismatch405 verifies that a wrong HTTP method on a registered
// path yields 405 before any handler runs.
func TestMethodMismatch405(t *testing.T) {
	f := newFake()
	srv := newTestServer(t, f)

	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/register"},
		{http.MethodDelete, "/api/v1/tasks/" + testTaskID + "/log"},
		{http.MethodPost, "/api/v1/tasks/" + testTaskID + "/files/foo.bin"},
		{http.MethodPut, "/api/v1/workers/n1/deregister"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			status, _ := rawRequest(t, srv, tc.method, tc.path, bearer(), "")
			if status != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", status)
			}
		})
	}
}

// TestNotFoundUnknownPath returns 404 for an unregistered route.
func TestNotFoundUnknownPath(t *testing.T) {
	f := newFake()
	srv := newTestServer(t, f)
	status, _ := rawRequest(t, srv, http.MethodGet, "/api/v1/nope", bearer(), "")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// TestListCapsRejectOversizedPayloads asserts the HTTP layer rejects
// heartbeats and results whose lists exceed the caps with 400, while the
// boundary values pass through to the orchestrator.
func TestListCapsRejectOversizedPayloads(t *testing.T) {
	f := newFake()
	srv := newTestServer(t, f)

	hb := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"name":"n1","tasks":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"task_id":"task-0001"}`)
		}
		b.WriteString(`]}`)
		return b.String()
	}
	if status, _ := rawRequest(t, srv, http.MethodPost, "/api/v1/heartbeat", bearer(), hb(maxTasksPerHeartbeat)); status != http.StatusOK {
		t.Errorf("heartbeat at the task cap = %d, want 200", status)
	}
	if status, _ := rawRequest(t, srv, http.MethodPost, "/api/v1/heartbeat", bearer(), hb(maxTasksPerHeartbeat+1)); status != http.StatusBadRequest {
		t.Errorf("heartbeat over the task cap = %d, want 400", status)
	}

	res := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"status":"succeeded","resource_usage":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"at":"2026-01-01T00:00:00Z"}`)
		}
		b.WriteString(`]}`)
		return b.String()
	}
	path := "/api/v1/tasks/" + testTaskID + "/result"
	if status, _ := rawRequest(t, srv, http.MethodPost, path, taskAuth(testClaimTok), res(maxResourceSamplesPerResult)); status != http.StatusOK {
		t.Errorf("result at the sample cap = %d, want 200", status)
	}
	if status, _ := rawRequest(t, srv, http.MethodPost, path, taskAuth(testClaimTok), res(maxResourceSamplesPerResult+1)); status != http.StatusBadRequest {
		t.Errorf("result over the sample cap = %d, want 400", status)
	}
}

// TestRegisterRecordsRequest verifies the register payload reaches the
// orchestrator with every field intact.
func TestRegisterRecordsRequest(t *testing.T) {
	f := newFake()
	srv := newTestServer(t, f)
	status, body := rawRequest(t, srv, http.MethodPost, "/api/v1/register", bearer(), registerBody)
	if status != http.StatusOK {
		t.Fatalf("status = %d (body %s)", status, body)
	}
	if f.lastRegister.Name != "n1" || f.lastRegister.Role != "host" ||
		f.lastRegister.Mode != "host" || f.lastRegister.Arch != "x86_64" ||
		f.lastRegister.Capacity != 1 || f.lastRegister.Version != "0.1.0" {
		t.Errorf("register payload not forwarded intact: %#v", f.lastRegister)
	}
}

// TestDeregisterKeyedByName verifies the deregister endpoint passes the
// worker name from the path through to the orchestrator.
func TestDeregisterKeyedByName(t *testing.T) {
	f := newFake()
	srv := newTestServer(t, f)
	status, body := rawRequest(t, srv, http.MethodPost, "/api/v1/workers/proud-heron-7/deregister", bearer(), "")
	if status != http.StatusOK {
		t.Fatalf("status = %d (body %s)", status, body)
	}
	if f.lastDeregister != "proud-heron-7" {
		t.Errorf("deregister name = %q, want %q", f.lastDeregister, "proud-heron-7")
	}
	if !strings.Contains(body, "{}") {
		t.Errorf("deregister body = %q, want {}", body)
	}
}
