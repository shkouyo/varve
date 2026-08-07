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
// plus the latest build artifacts, the build history and the optional
// download button.
type packageData struct {
	base
	Pkg       db.Package
	Builds    []db.Build
	Download  *downloadLink // nil when downloads are disabled or there is no artifact
	LatestArt []db.Artifact
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
	ctx := r.Context()
	pkgbase := r.PathValue("pkgbase")

	pkg, err := s.store.GetPackageByBase(ctx, pkgbase)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			s.renderError(w, http.StatusNotFound, "Package not found: "+pkgbase)
			return
		}
		s.renderError(w, http.StatusInternalServerError, "Failed to load the package.")
		return
	}

	// Build history: the newest builds, restricted to this package (the
	// store has no package-scoped list; GetPackageByBase + ListBuilds is
	// the read path).
	all, _, err := s.store.ListBuilds(ctx, 1, perPage, false)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, "Failed to load the build history.")
		return
	}
	builds := make([]db.Build, 0, len(all))
	for _, b := range all {
		if b.PackageID == pkg.ID {
			builds = append(builds, b)
		}
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

	data := packageData{
		base:      s.page(pkg.Pkgbase, nil),
		Pkg:       *pkg,
		Builds:    builds,
		Download:  downloadFor(s.cfg.Web.DownloadEnabled, s.cfg.Web.DownloadBaseURI, latest),
		LatestArt: latestArtifacts(latest),
	}
	data.Nav = "packages"
	s.render(w, "package.html", data)
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
