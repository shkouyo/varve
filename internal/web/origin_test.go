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
	"net/http"
	"net/http/httptest"
	"testing"

	"git.0x0f.dev/varve/internal/dispatch"
)

// TestSameOrigin is the origin-matching matrix: the request is accepted
// only when the Origin (or Referer fallback) host equals the request
// host.
func TestSameOrigin(t *testing.T) {
	tests := []struct {
		name       string
		origin     string
		referer    string
		host       string
		wantAccept bool
	}{
		{name: "same origin", origin: "http://example.com", host: "example.com", wantAccept: true},
		{name: "same origin https", origin: "https://example.com", host: "example.com", wantAccept: true},
		{name: "same origin with port", origin: "http://example.com:8080", host: "example.com:8080", wantAccept: true},
		{name: "host case is ignored", origin: "http://EXAMPLE.com", host: "example.com", wantAccept: true},
		{name: "cross site", origin: "https://evil.example", host: "example.com"},
		{name: "cross port", origin: "http://example.com:8080", host: "example.com"},
		{name: "null origin", origin: "null", host: "example.com"},
		{name: "non-http scheme", origin: "file:///tmp/x", host: "example.com"},
		{name: "garbage origin", origin: "not a url", host: "example.com"},
		{name: "referer fallback", referer: "http://example.com/admin/packages/x/rebuild", host: "example.com", wantAccept: true},
		{name: "cross-site referer", referer: "https://evil.example/admin", host: "example.com"},
		{name: "origin wins over referer", origin: "https://evil.example", referer: "http://example.com/admin", host: "example.com"},
		{name: "both missing", host: "example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newRequest(t, http.MethodPost, "/admin/packages/demo-pkg/rebuild")
			req.Host = tt.host
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.referer != "" {
				req.Header.Set("Referer", tt.referer)
			}
			if got := sameOrigin(req); got != tt.wantAccept {
				t.Errorf("sameOrigin() = %v, want %v", got, tt.wantAccept)
			}
		})
	}
}

// TestAdminRequiresSameOrigin asserts every admin POST is rejected with
// 403 unless the request carries a same-site Origin or Referer, and that
// the handler never runs for rejected requests.
func TestAdminRequiresSameOrigin(t *testing.T) {
	orch := &fakeOrchestrator{stats: &dispatch.Stats{}}
	s := newTestServer(t, testConfig(), orch, newTestDB(t), newFakeLogReader(""))

	post := func(origin, referer string) *httptest.ResponseRecorder {
		req := newRequest(t, http.MethodPost, "/admin/packages/demo-pkg/rebuild")
		req.SetBasicAuth("admin", "s3cret")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		return serve(t, s, req)
	}

	// Missing both headers: rejected (non-browser clients must send an
	// Origin or Referer explicitly).
	if rec := post("", ""); rec.Code != http.StatusForbidden {
		t.Errorf("POST without Origin/Referer = %d, want 403", rec.Code)
	}
	// Cross-site Origin and Referer: rejected.
	if rec := post("https://evil.example", ""); rec.Code != http.StatusForbidden {
		t.Errorf("POST with cross-site Origin = %d, want 403", rec.Code)
	}
	if rec := post("", "https://evil.example/admin"); rec.Code != http.StatusForbidden {
		t.Errorf("POST with cross-site Referer = %d, want 403", rec.Code)
	}
	if len(orch.rebuilds) != 0 {
		t.Errorf("rebuilds = %v, want none for rejected requests", orch.rebuilds)
	}

	// Same-site Origin and Referer: accepted.
	if rec := post("http://example.com", ""); rec.Code != http.StatusSeeOther {
		t.Errorf("POST with same-site Origin = %d, want 303", rec.Code)
	}
	if rec := post("", "http://example.com/admin/packages/demo-pkg/rebuild"); rec.Code != http.StatusSeeOther {
		t.Errorf("POST with same-site Referer = %d, want 303", rec.Code)
	}
	if len(orch.rebuilds) != 2 {
		t.Errorf("rebuilds = %v, want 2 accepted submissions", orch.rebuilds)
	}
}

// TestSameOriginLeavesGetsAlone asserts the gate is applied only to the
// admin POST routes: public GET pages and even cross-site GET requests
// are unaffected.
func TestSameOriginLeavesGetsAlone(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{}},
		newTestDB(t), newFakeLogReader(""))

	req := newRequest(t, http.MethodGet, "/")
	req.Header.Set("Origin", "https://evil.example")
	if rec := serve(t, s, req); rec.Code != http.StatusOK {
		t.Errorf("GET / with cross-site Origin = %d, want 200", rec.Code)
	}
}
