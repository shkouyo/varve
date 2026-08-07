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
	"strconv"

	"git.0x0f.dev/varve/internal/db"
)

// buildData feeds build.html: the command log history, the executing
// node name and the cgroup resource samples rendered as text (no
// charts).
type buildData struct {
	base
	Build       db.Build
	Pkgbase     string
	WorkerName  string
	Log         string
	HasLog      bool
	Samples     []resourceView
	SampleCount int
	BuildID     string
	LogURL      string
}

// resourceView is one cgroup sample rendered as text.
type resourceView struct {
	At  string
	CPU string
	Mem string
}

// handleBuild renders GET /builds/{id}.
func (s *Server) handleBuild(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		s.renderError(w, http.StatusNotFound, "Invalid build id.")
		return
	}

	b, err := s.store.GetBuild(ctx, id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			s.renderError(w, http.StatusNotFound, "Build not found: "+id)
			return
		}
		s.renderError(w, http.StatusInternalServerError, "Failed to load the build.")
		return
	}

	data := buildData{
		base:        s.page("Build #"+id, nil),
		Build:       *b,
		BuildID:     id,
		LogURL:      "/builds/" + id + "/log",
		SampleCount: len(b.ResourceUsage),
	}

	// Executing machine name (builds.worker_name in plain text, with the
	// workers table as a fallback for rows recorded before the column was
	// populated) and the package name for context.
	if p, err := s.store.GetPackageByID(ctx, b.PackageID); err == nil {
		data.Pkgbase = p.Pkgbase
	}
	data.WorkerName = b.WorkerName
	if data.WorkerName == "" && b.WorkerID > 0 {
		if w, err := s.store.GetWorkerByID(ctx, b.WorkerID); err == nil {
			data.WorkerName = w.Name
		}
	}
	// Log history from the log store (the live stream lives on the log
	// page).
	if logData, err := s.logs.ReadLog(ctx, id); err == nil && len(logData) > 0 {
		data.Log = string(logData)
		data.HasLog = true
	}

	// Cgroup resource samples rendered as text.
	for _, smp := range b.ResourceUsage {
		data.Samples = append(data.Samples, resourceView{
			At:  smp.At.Format("15:04:05"),
			CPU: formatCPU(smp.CPUTimeNS),
			Mem: formatBytes(smp.MemoryBytes),
		})
	}

	s.render(w, "build.html", data)
}

// formatCPU renders a cumulative CPU time in nanoseconds as seconds with
// one decimal ("12.3s").
func formatCPU(ns int64) string {
	return fmtFloat(float64(ns)/1e9) + "s"
}

// formatBytes renders a byte count in a human friendly unit ("512 MiB",
// "1.2 GiB").
func formatBytes(n int64) string {
	const (
		kb = 1 << 10
		mb = 1 << 20
		gb = 1 << 30
	)
	switch {
	case n >= gb:
		return fmtFloat(float64(n)/gb) + " GiB"
	case n >= mb:
		return fmtFloat(float64(n)/mb) + " MiB"
	case n >= kb:
		return fmtFloat(float64(n)/kb) + " KiB"
	default:
		return fmtFloat(float64(n)) + " B"
	}
}

// fmtFloat renders a float with one decimal, trimming a trailing ".0".
func fmtFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', 1, 64)
	if len(s) > 2 && s[len(s)-2:] == ".0" {
		s = s[:len(s)-2]
	}
	return s
}
