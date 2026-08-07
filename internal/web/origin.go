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
	"net/url"
	"strings"
)

// requireSameOrigin guards the admin POST handlers against cross-site
// request forgery: browsers attach Basic Auth credentials to every
// request for the site automatically, so without this check any
// cross-site form could trigger admin actions. The request must carry an
// Origin header (or a Referer as the fallback — some clients send only
// one) whose host matches the site host; anything else is rejected with
// 403. Non-browser clients (curl, scripts) have no Origin and must send
// one explicitly, e.g. -H 'Origin: http://<host>'.
func (s *Server) requireSameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			s.renderError(w, http.StatusForbidden, "Cross-site requests are rejected.")
			return
		}
		next(w, r)
	}
}

// sameOrigin reports whether the request comes from this site: the
// Origin header wins, the Referer is the fallback. The origin must be an
// absolute http(s) URL whose host, including the port, equals the
// request's Host. The scheme is not compared: the endpoint is usually
// behind a TLS-terminating proxy, and an attacker page cannot control
// the scheme of this site's URL, only its own host.
func sameOrigin(r *http.Request) bool {
	raw := r.Header.Get("Origin")
	if raw == "" {
		raw = r.Header.Get("Referer")
	}
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}
