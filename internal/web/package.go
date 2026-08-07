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
	Download  *downloadLink // nil when downloads are disabled or there is no artifact
	LatestArt []db.Artifact
	Admin     bool
}

// downloadLink is the artifact download destination: DownloadBaseURI +
// "/" + the latest build's package artifact file name.
type downloadLink struct {
	URL   string
	File  string
	Bytes int64
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
	s.render(w, "package.html", data)
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
		LatestArt: latestArtifacts(latest),
		Admin:     s.authorized(r),
	}, nil
}

// latestArtifacts returns the artifact list of the latest build.
func latestArtifacts(latest *db.Build) []db.Artifact {
	if latest == nil {
		return nil
	}
	return latest.Artifacts
}

// downloadFor builds the download link for the latest build, or nil when
// downloads are disabled or the latest build has no package artifact.
// Only the primary package artifact is downloadable; signature and
// srcinfo side files are metadata, not packages.
func downloadFor(enabled bool, baseURI string, latest *db.Build) *downloadLink {
	if !enabled || latest == nil || len(latest.Artifacts) == 0 {
		return nil
	}
	var art *db.Artifact
	for i := range latest.Artifacts {
		if latest.Artifacts[i].Kind == "package" {
			art = &latest.Artifacts[i]
			break
		}
	}
	if art == nil {
		return nil
	}
	return &downloadLink{
		URL:   baseURI + "/" + art.File,
		File:  art.File,
		Bytes: art.Size,
	}
}
