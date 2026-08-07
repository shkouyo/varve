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
	"crypto/subtle"
	"net/http"
)

// requireAuth gates an admin handler behind HTTP Basic Auth.
// Unauthorized requests receive a 401 with the WWW-Authenticate challenge
// so browsers prompt for credentials. There is no cookie session, so the
// admin area is naturally CSRF-free.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="varve admin", charset="UTF-8"`)
			s.renderError(w, http.StatusUnauthorized,
				"Authentication required. /admin is protected by Basic Auth.")
			return
		}
		next(w, r)
	}
}

// authorized verifies the request credentials against every configured
// admin with constant-time comparisons, so a timing side channel cannot
// leak a configured username or password. All entries are evaluated so
// the runtime is independent of which entry matches.
func (s *Server) authorized(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	matched := 0
	for _, a := range s.cfg.Web.Admins {
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(a.User)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(a.Password)) == 1
		if userOK && passOK {
			matched = 1
		}
	}
	return matched == 1
}
