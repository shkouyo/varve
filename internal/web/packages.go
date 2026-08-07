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
	"strings"

	"git.0x0f.dev/varve/internal/db"
)

// perPage is the page size for every paginated list (packages, builds).
const perPage = 20

// maxScanPackages caps the row scan of the in-memory scope/arch filter
// pass (SQLite treats it as an unbounded limit).
const maxScanPackages = 1 << 30

// packagesData feeds packages.html: the searchable, paginated package
// list. Search matches pkgbase and/or pkgdesc depending on Scope, and
// can be narrowed to a single architecture by Arch; both filters ride
// on the data so the template can keep them in the pagination links.
type packagesData struct {
	base
	Query    string
	Scope    string
	Arch     string
	Page     int
	Total    int
	Pages    int
	Packages []db.Package
}

// parseScope validates a ?scope= search filter: name, desc or both (the
// default when absent). Anything else is rejected.
func parseScope(raw string) (string, bool) {
	switch raw {
	case "", "both":
		return "both", true
	case "name", "desc":
		return raw, true
	}
	return "", false
}

// parseArchFilter validates a ?arch= filter: all (the default when
// absent), x86_64 or any. Anything else is rejected.
func parseArchFilter(raw string) (string, bool) {
	switch raw {
	case "", "all":
		return "all", true
	case "x86_64", "any":
		return raw, true
	}
	return "", false
}

// archMatches reports whether the "|"-joined architecture set of a
// package contains the filter element: x86_64 matches a set listing
// x86_64, any matches a set listing any (which may also carry concrete
// arches).
func archMatches(set, filter string) bool {
	for _, elem := range strings.Split(set, "|") {
		if elem == filter {
			return true
		}
	}
	return false
}

// containsFold reports a case-insensitive substring match, mirroring
// the store's LIKE search for the in-memory scope pass.
func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// handlePackages renders GET /packages with ?q= search, ?scope= and
// ?arch= filters and ?page= pagination. The search term is length-capped
// and the page number is clamped to the valid range.
func (s *Server) handlePackages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query().Get("q")
	if len(q) > maxQueryLen {
		s.renderError(w, r, http.StatusBadRequest, "Search query is too long.")
		return
	}
	scope, ok := parseScope(r.URL.Query().Get("scope"))
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "Invalid search scope.")
		return
	}
	arch, ok := parseArchFilter(r.URL.Query().Get("arch"))
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "Invalid architecture filter.")
		return
	}
	page := parsePage(r.URL.Query().Get("page"))

	var pkgs []db.Package
	var total int
	var err error
	if scope == "both" && arch == "all" {
		// Fast path: the store search matches both fields already, so
		// SQL handles the filter and pagination; the first call learns
		// the total so an oversized page clamps before the row query.
		_, total, err = s.store.ListPackages(ctx, q, 1, perPage)
		if err != nil {
			s.renderError(w, r, http.StatusInternalServerError, "Failed to load the package list.")
			return
		}
		p := pages(total, perPage)
		if page > p {
			page = p
		}
		pkgs, _, err = s.store.ListPackages(ctx, q, page, perPage)
		if err != nil {
			s.renderError(w, r, http.StatusInternalServerError, "Failed to load the package list.")
			return
		}
	} else {
		// Filtered path: the store searches pkgbase OR pkgdesc without
		// scope or arch support, so the full match set is loaded and
		// narrowed here (the wave boundary keeps the store API intact;
		// the farm's package count makes the scan cheap).
		all, _, err := s.store.ListPackages(ctx, q, 1, maxScanPackages)
		if err != nil {
			s.renderError(w, r, http.StatusInternalServerError, "Failed to load the package list.")
			return
		}
		matched := make([]db.Package, 0, len(all))
		for _, pkg := range all {
			if scope == "name" && !containsFold(pkg.Pkgbase, q) {
				continue
			}
			if scope == "desc" && !containsFold(pkg.Pkgdesc, q) {
				continue
			}
			if arch != "all" && !archMatches(pkg.Arch, arch) {
				continue
			}
			matched = append(matched, pkg)
		}
		total = len(matched)
		p := pages(total, perPage)
		if page > p {
			page = p
		}
		lo := (page - 1) * perPage
		hi := lo + perPage
		if hi > total {
			hi = total
		}
		pkgs = matched[lo:hi]
	}

	data := packagesData{
		base:     s.page(r, "Packages", nil),
		Query:    q,
		Scope:    scope,
		Arch:     arch,
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
