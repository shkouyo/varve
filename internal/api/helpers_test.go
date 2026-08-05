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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/config"
)

// Shared test credentials (node-level Bearer + per-task claim token).
const (
	testToken    = "test-shared-token"
	testTaskID   = "task-0001"
	testClaimTok = "a1b2c3d4e5f6"
)

// newTestServer builds an httptest server over the real Handler() and the
// given fake orchestrator (DETAIL §9.7 pairing).
func newTestServer(t *testing.T, f *fakeOrchestrator) *httptest.Server {
	t.Helper()
	cfg := &config.ControllerConfig{API: config.APIConfig{Token: testToken}}
	srv := httptest.NewServer(NewServer(cfg, f).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// rawRequest issues a raw HTTP request against the test server and returns
// the status and body.
func rawRequest(t *testing.T, srv *httptest.Server, method, path string, headers map[string]string, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(data)
}

// bearer returns the Authorization header map for the shared token.
func bearer() map[string]string {
	return map[string]string{"Authorization": "Bearer " + testToken}
}

// taskAuth returns the X-Varve-Task-Token header map.
func taskAuth(token string) map[string]string {
	return map[string]string{taskTokenHeader: token}
}
