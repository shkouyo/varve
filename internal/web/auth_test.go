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

package web

import (
	"net/http"
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/dispatch"
)

// TestBasicAuthMatrix drives the credential matrix over /admin (DETAIL
// §10.7 point 2): no credentials and wrong credentials are rejected with
// 401 + challenge, correct credentials pass, and public routes stay open.
func TestBasicAuthMatrix(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{}},
		newTestDB(t), newFakeLogReader(""))

	cases := []struct {
		name    string
		user    string
		pass    string
		setAuth bool
		want    int
	}{
		{"no credentials", "", "", false, http.StatusUnauthorized},
		{"empty credentials", "", "", true, http.StatusUnauthorized},
		{"wrong user", "root", "s3cret", true, http.StatusUnauthorized},
		{"wrong password", "admin", "wrong", true, http.StatusUnauthorized},
		{"wrong both", "root", "wrong", true, http.StatusUnauthorized},
		{"correct", "admin", "s3cret", true, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := newRequest(t, http.MethodGet, "/admin")
			if tc.setAuth {
				req.SetBasicAuth(tc.user, tc.pass)
			}
			rec := serve(t, s, req)
			if rec.Code != tc.want {
				t.Fatalf("GET /admin = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusUnauthorized {
				challenge := rec.Header().Get("WWW-Authenticate")
				if !strings.HasPrefix(challenge, "Basic") {
					t.Errorf("WWW-Authenticate = %q, want a Basic challenge", challenge)
				}
			}
		})
	}

	// Public routes require no credentials.
	rec := get(t, s, http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / without auth = %d, want 200", rec.Code)
	}
}

// TestUnauthorizedRendersErrorPage asserts the 401 response carries the
// error page markup (DETAIL §10.4 point 4).
func TestUnauthorizedRendersErrorPage(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{stats: &dispatch.Stats{}},
		newTestDB(t), newFakeLogReader(""))
	rec := get(t, s, http.MethodGet, "/admin", nil)
	body := rec.Body.String()
	mustContain(t, body, "Unauthorized", "Authentication required", "main")
}
