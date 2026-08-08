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
// status, package, machine and timestamps, newest first. The template
// renders its empty message when Builds carries no rows. Admin marks an
// authenticated request so the template renders the per-row rebuild
// action.
type buildsData struct {
	base
	Page   int
	Pages  int
	Total  int
	Builds []recentBuildView
	Admin  bool
}

// handleBuilds renders GET /builds with ?page= pagination. Page numbers
// clamp to the valid range: malformed values become page 1, oversized
// values the last page.
func (s *Server) handleBuilds(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page := parsePage(r.URL.Query().Get("page"))
	// Count first so an oversized page clamps before the row query runs.
	_, total, err := s.store.ListBuilds(ctx, 1, perPage, false)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Failed to load the build list.")
		return
	}
	workers, err := s.store.ListWorkers(ctx)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Failed to load the worker list.")
		return
	}
	p := pages(total, perPage)
	if page > p {
		page = p
	}
	builds, _, err := s.store.ListBuilds(ctx, page, perPage, false)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Failed to load the build list.")
		return
	}
	data := buildsData{
		base:   s.page(r, "Builds", nil),
		Page:   page,
		Pages:  p,
		Total:  total,
		Builds: s.recentBuildViews(ctx, builds, workers),
		Admin:  s.authorized(r),
	}
	data.Nav = "builds"
	s.render(w, "builds.html", &data)
}
