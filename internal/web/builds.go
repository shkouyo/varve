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
)

// buildsData feeds builds.html: the public, paginated build history with
// status, package, machine and timestamps, newest first. Empty flags the
// missing-list state so the template can render its empty message.
type buildsData struct {
	base
	Page   int
	Pages  int
	Total  int
	Builds []recentBuildView
	Empty  bool
}

// handleBuilds renders GET /builds with ?page= pagination. The requested
// page is clamped to the last one when it exceeds the range.
func (s *Server) handleBuilds(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page, ok := parsePage(r.URL.Query().Get("page"))
	if !ok {
		s.renderError(w, http.StatusBadRequest, "Invalid page number.")
		return
	}
	builds, total, err := s.store.ListBuilds(ctx, page, perPage, false)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, "Failed to load the build list.")
		return
	}
	workers, err := s.store.ListWorkers(ctx)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, "Failed to load the worker list.")
		return
	}
	p := pages(total, perPage)
	if page > p {
		page = p
		if builds, _, err = s.store.ListBuilds(ctx, page, perPage, false); err != nil {
			s.renderError(w, http.StatusInternalServerError, "Failed to load the build list.")
			return
		}
	}
	data := buildsData{
		base:   s.page("Builds", nil),
		Page:   page,
		Pages:  p,
		Total:  total,
		Builds: s.recentBuildViews(ctx, builds, workers),
		Empty:  total == 0,
	}
	data.Nav = "builds"
	s.render(w, "builds.html", data)
}
