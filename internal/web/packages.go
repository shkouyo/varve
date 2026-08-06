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

	"git.0x0f.dev/varve/internal/db"
)

// perPage is the page size for every paginated list (packages, builds).
const perPage = 20

// packagesData feeds packages.html: the searchable, paginated package
// list. Search matches pkgbase and pkgdesc via store.ListPackages.
type packagesData struct {
	base
	Query    string
	Page     int
	Total    int
	Pages    int
	Packages []db.Package
}

// handlePackages renders GET /packages with ?q= search and ?page=
// pagination.
func (s *Server) handlePackages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query().Get("q")
	page := parsePage(r.URL.Query().Get("page"))

	pkgs, total, err := s.store.ListPackages(ctx, q, page, perPage)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, "Failed to load the package list.")
		return
	}

	data := packagesData{
		base:     s.page("Packages", nil),
		Query:    q,
		Page:     page,
		Total:    total,
		Pages:    pages(total, perPage),
		Packages: pkgs,
	}
	data.Nav = "packages"
	s.render(w, "packages.html", data)
}

// pages computes the number of pages for a total item count.
func pages(total, size int) int {
	if total <= 0 {
		return 1
	}
	return (total-1)/size + 1
}
