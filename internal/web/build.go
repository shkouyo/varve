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
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/dispatch"
)

// buildData feeds build.html: the build summary, the merged live log
// (rendered as a line array with SSE increments appended by the
// template), and the cgroup resource samples rendered as text (no
// charts). Lines holds the truncated log tail split on newlines; the
// first line is the partial remainder when the log was cut. LogURL is
// the legacy log page (it redirects to the #log anchor) and SSEURL the
// resumable event stream, with ?after= resuming at the end of the
// rendered content. Wait marks a build that has no log yet but is still
// active; Note carries the closing message for a terminal build that
// never produced a log.
type buildData struct {
	base
	Build         db.Build
	Pkgbase       string
	WorkerName    string
	Log           string
	Lines         []string
	HasLog        bool
	Truncated     bool
	TruncatedNote string
	Wait          bool
	Note          string
	Samples       []resourceView
	SampleCount   int
	BuildID       string
	LogURL        string
	SSEURL        string
}

// resourceView is one cgroup sample rendered as text.
type resourceView struct {
	At  string
	CPU string
	Mem string
}

// handleBuild renders GET /builds/{id}. A malformed build id is a 400;
// a well-formed but unknown one a 404. The failure error summary is not
// exposed on this page (the live log carries the failure detail); the
// Error field is zeroed so stale summaries never render.
func (s *Server) handleBuild(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		s.renderError(w, http.StatusBadRequest, "Invalid build id.")
		return
	}
	data, err := s.buildData(r, id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		s.renderError(w, http.StatusNotFound, "Build not found: "+id)
	case err != nil:
		s.renderError(w, http.StatusInternalServerError, "Failed to load the build.")
	default:
		s.render(w, "build.html", data)
	}
}

// buildData assembles the build page data: the build row, the package
// and machine context, the merged log tail and the resource samples.
func (s *Server) buildData(r *http.Request, id string) (buildData, error) {
	ctx := r.Context()
	b, err := s.store.GetBuild(ctx, id)
	if err != nil {
		return buildData{}, err
	}

	data := buildData{
		base:        s.page("Build "+shortBuildID(id), nil),
		Build:       *b,
		BuildID:     id,
		LogURL:      "/builds/" + id + "/log",
		SSEURL:      "/builds/" + id + "/log/stream",
		SampleCount: len(b.ResourceUsage),
	}
	data.Build.Error = "" // failure detail lives in the log, not the summary

	// The log stream and the auto-refresh stay live only while the build
	// runs; a terminal build freezes the page.
	data.PageActive = !isTerminalStatus(b.Status)
	if !data.PageActive {
		data.RefreshSeconds = 0
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

	// Log history from the log store, truncated to the most recent
	// maxInlineLog bytes; the SSE stream resumes at the rendered end.
	if err := s.loadBuildLog(ctx, b, &data); err != nil {
		return buildData{}, err
	}

	// Cgroup resource samples rendered as text.
	for _, smp := range b.ResourceUsage {
		data.Samples = append(data.Samples, resourceView{
			At:  smp.At.Format("15:04:05"),
			CPU: formatCPU(smp.CPUTimeNS),
			Mem: formatBytes(smp.MemoryBytes),
		})
	}
	return data, nil
}

// loadBuildLog fills the merged log fields from the log store: the
// truncated line tail plus the flags describing the missing or
// truncated state.
func (s *Server) loadBuildLog(ctx context.Context, b *db.Build, data *buildData) error {
	id := b.ID
	logData, err := s.logs.ReadLog(ctx, id)
	switch {
	case err == nil && len(logData) > 0:
		start := 0
		if len(logData) > maxInlineLog {
			start = len(logData) - maxInlineLog
			data.Truncated = true
		}
		data.Log = string(logData[start:])
		data.Lines = strings.Split(data.Log, "\n")
		data.HasLog = true
		if data.Truncated {
			data.TruncatedNote = fmt.Sprintf("Log truncated, showing the last %d bytes.", len(data.Log))
		}
		data.SSEURL += "?after=" + strconv.FormatInt(int64(len(logData)), 10)
	case errors.Is(err, dispatch.ErrNotFound):
		// No log file: the queued/not-started state renders a waiting
		// message, a terminal build without a log a closing note.
		if isTerminalStatus(b.Status) {
			data.Note = "No log was recorded for this build."
		} else {
			data.Wait = true
		}
	case err != nil:
		return fmt.Errorf("web: read build log: %w", err)
	}
	return nil
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
