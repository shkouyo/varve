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
	"errors"
	"net/http"

	"git.0x0f.dev/varve/internal/db"
)

// packageData feeds package.html: current version, metadata from SQLite
// plus the latest build artifacts, the paged build history and the
// optional download button. The .SRCINFO metadata (url, licenses,
// conflicts, provides) rides on the package row for the template. Admin
// marks an authenticated request so the template renders the rebuild
// action.
type packageData struct {
	base
	Pkg       db.Package
	Builds    []db.Build
	Page      int // build history page
	Pages     int
	Total     int
	Download  *downloadLink // hero button; nil when downloads are disabled or the build split into several packages
	LatestArt []artifactView
	Admin     bool
}

// downloadLink is the hero download destination: DownloadBaseURI +
// "/" + the package artifact file name of the latest build.
type downloadLink struct {
	URL   string
	File  string
	Bytes int64
}

// artifactView is one artifact row of the latest build's card. URL is the
// download destination for package artifacts while downloads are enabled;
// signature and srcinfo side files are metadata and never get one.
type artifactView struct {
	db.Artifact
	URL string
}

// handlePackage renders GET /packages/{pkgbase}.
func (s *Server) handlePackage(w http.ResponseWriter, r *http.Request) {
	pkgbase := r.PathValue("pkgbase")
	if !validPkgbase(pkgbase) {
		s.renderError(w, r, http.StatusBadRequest, "Invalid package name.")
		return
	}
	page := parsePage(r.URL.Query().Get("page"))
	data, err := s.packageData(r, pkgbase, page)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "Package not found: "+pkgbase)
			return
		}
		s.renderError(w, r, http.StatusInternalServerError, "Failed to load the package.")
		return
	}
	data.Nav = "packages"
	s.render(w, "package.html", &data)
}

// packageData assembles the package page data: the package row (with its
// .SRCINFO metadata), the newest build history for the package (paged, so
// long histories are not truncated), the download link of the latest
// build and its artifact list. The requested page clamps to the valid
// range: malformed values become page 1, oversized values the last page.
func (s *Server) packageData(r *http.Request, pkgbase string, page int) (packageData, error) {
	ctx := r.Context()
	pkg, err := s.store.GetPackageByBase(ctx, pkgbase)
	if err != nil {
		return packageData{}, err
	}

	// Build history, newest first, restricted to this package. Count
	// first so an oversized page clamps before the row query runs.
	_, total, err := s.store.ListBuildsByPackage(ctx, pkg.ID, 1, perPage)
	if err != nil {
		return packageData{}, err
	}
	p := pages(total, perPage)
	if page > p {
		page = p
	}
	builds, _, err := s.store.ListBuildsByPackage(ctx, pkg.ID, page, perPage)
	if err != nil {
		return packageData{}, err
	}

	// Latest build: prefer the recorded last_build_id, fall back to the
	// newest history row.
	var latest *db.Build
	if pkg.LastBuildID != "" {
		if b, err := s.store.GetBuild(ctx, pkg.LastBuildID); err == nil {
			latest = b
		}
	}
	if latest == nil && len(builds) > 0 {
		latest = &builds[0]
	}

	return packageData{
		base:      s.page(r, pkg.Pkgbase, nil),
		Pkg:       *pkg,
		Builds:    builds,
		Page:      page,
		Pages:     p,
		Total:     total,
		Download:  downloadFor(s.cfg.Web.DownloadEnabled, s.cfg.Web.DownloadBaseURI, latest),
		LatestArt: artifactViews(s.cfg.Web.DownloadEnabled, s.cfg.Web.DownloadBaseURI, latest),
		Admin:     s.authorized(r),
	}, nil
}

// artifactViews maps the latest build's artifacts to their card rows,
// attaching a download URL to each package artifact when downloads are
// enabled. Side files (signatures, .SRCINFO) stay link-free; they are
// metadata about the packages, not packages themselves.
func artifactViews(enabled bool, baseURI string, latest *db.Build) []artifactView {
	if latest == nil {
		return nil
	}
	views := make([]artifactView, 0, len(latest.Artifacts))
	for _, a := range latest.Artifacts {
		v := artifactView{Artifact: a}
		if enabled && a.Kind == "package" {
			v.URL = baseURI + "/" + a.File
		}
		views = append(views, v)
	}
	return views
}

// downloadFor builds the hero download link for the latest build, or nil
// when downloads are disabled, the build has no package artifact, or the
// build split into several packages. A split build has no single obvious
// download, so the hero button hides and the artifact card links every
// package artifact individually.
func downloadFor(enabled bool, baseURI string, latest *db.Build) *downloadLink {
	if !enabled || latest == nil {
		return nil
	}
	var first *db.Artifact
	count := 0
	for i := range latest.Artifacts {
		if latest.Artifacts[i].Kind == "package" {
			count++
			if first == nil {
				a := latest.Artifacts[i]
				first = &a
			}
		}
	}
	if count != 1 {
		return nil
	}
	return &downloadLink{
		URL:   baseURI + "/" + first.File,
		File:  first.File,
		Bytes: first.Size,
	}
}
