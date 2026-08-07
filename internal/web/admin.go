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
	"net/http"
	"net/url"
	"regexp"

	"git.0x0f.dev/varve/internal/db"
)

// taskView is one queued/assigned/running task row.
type taskView struct {
	ID        string
	Pkgbase   string
	State     string
	Worker    string
	CreatedAt string
	CancelURL string
}

// adminBuildsData feeds admin_builds.html: the failed build list (the
// long-term retention area).
type adminBuildsData struct {
	base
	Builds []failedBuildView
	Page   int
	Pages  int
	Total  int
}

// failedBuildView is one failed build row with its package and the
// recorded error summary.
type failedBuildView struct {
	ID        string
	Pkgbase   string
	Error     string
	BuildURL  string
	LogURL    string
	StartedAt string
}

// handleAdmin redirects to the merged dashboard page; the admin content
// renders there when the request is authenticated. The flash query string
// of the last admin action survives the redirect.
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	target := "/"
	if r.URL.RawQuery != "" {
		target = "/?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleLogout ends a stateless Basic Auth session the only way one can:
// by answering 401 with a challenge so the browser drops its saved
// credentials. The error page carries a link back to the start page, so
// logout is never a dead end.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", `Basic realm="varve admin", charset="UTF-8"`)
	s.renderError(w, r, http.StatusUnauthorized, "Logged out.")
}

// taskViews resolves task rows into the template view (package name and
// worker name).
func (s *Server) taskViews(ctx context.Context, tasks []db.Task, workers []db.Worker) []taskView {
	workerNames := make(map[int64]string, len(workers))
	for _, w := range workers {
		workerNames[w.ID] = w.Name
	}
	pkgNames := make(map[int64]string)
	out := make([]taskView, 0, len(tasks))
	for _, t := range tasks {
		name, ok := pkgNames[t.PackageID]
		if !ok {
			if p, err := s.store.GetPackageByID(ctx, t.PackageID); err == nil {
				name = p.Pkgbase
				pkgNames[t.PackageID] = name
			}
		}
		out = append(out, taskView{
			ID:        t.ID,
			Pkgbase:   name,
			State:     t.State,
			Worker:    workerNames[t.WorkerID],
			CreatedAt: absTime(&t.CreatedAt),
			CancelURL: "/admin/tasks/" + t.ID + "/cancel",
		})
	}
	return out
}

// handleAdminRebuild posts a manual rebuild for a package. The action
// redirects straight back to the dashboard with the flash carried in the
// query string (one hop, no /admin round trip).
func (s *Server) handleAdminRebuild(w http.ResponseWriter, r *http.Request) {
	pkgbase := r.PathValue("pkgbase")
	if err := s.orch.RebuildPackage(r.Context(), pkgbase); err != nil {
		s.redirectFlash(w, r, "/", "error", "Rebuild failed: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "/", "ok", "Rebuild queued for "+pkgbase)
}

// handleAdminCancel cancels a queued or running task.
func (s *Server) handleAdminCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.orch.CancelTask(r.Context(), id); err != nil {
		s.redirectFlash(w, r, "/", "error", "Cancel failed: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "/", "ok", "Cancellation requested for task "+id)
}

// handleAdminDisable disables a worker (no new assignments).
func (s *Server) handleAdminDisable(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.orch.DisableWorker(r.Context(), name); err != nil {
		s.redirectFlash(w, r, "/", "error", "Disable failed: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "/", "ok", "Worker "+name+" disabled")
}

// handleAdminEnable re-enables a disabled worker (new assignments resume).
func (s *Server) handleAdminEnable(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.orch.EnableWorker(r.Context(), name); err != nil {
		s.redirectFlash(w, r, "/", "error", "Enable failed: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "/", "ok", "Worker "+name+" enabled")
}

// handleAdminRemove removes a worker record immediately; pressing the
// remove action is the explicit intent, so no confirmation round-trip is
// required.
func (s *Server) handleAdminRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.orch.RemoveWorker(r.Context(), name); err != nil {
		s.redirectFlash(w, r, "/", "error", "Remove failed: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "/", "ok", "Worker "+name+" removed")
}

// handleAdminBuilds renders GET /admin/builds?failed=1, the long-term
// failed build retention list. The failed filter must be "1" when
// present; page numbers clamp to the valid range.
func (s *Server) handleAdminBuilds(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if f := r.URL.Query().Get("failed"); f != "" && f != "1" {
		s.renderError(w, r, http.StatusBadRequest, "Invalid failed filter.")
		return
	}
	page := parsePage(r.URL.Query().Get("page"))
	// Count first so an oversized page clamps before the row query runs.
	_, total, err := s.store.ListBuilds(ctx, 1, perPage, true)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Failed to load the failed build list.")
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
	builds, _, err := s.store.ListBuilds(ctx, page, perPage, true)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Failed to load the failed build list.")
		return
	}

	data := adminBuildsData{
		base:  s.page(r, "Failed builds", flashFromQuery(r)),
		Page:  page,
		Pages: p,
		Total: total,
	}
	workerNames := make(map[int64]string, len(workers))
	for _, w := range workers {
		workerNames[w.ID] = w.Name
	}
	pkgNames := make(map[int64]string)
	for _, b := range builds {
		name, ok := pkgNames[b.PackageID]
		if !ok {
			if p, err := s.store.GetPackageByID(ctx, b.PackageID); err == nil {
				name = p.Pkgbase
				pkgNames[b.PackageID] = name
			}
		}
		id := b.ID
		data.Builds = append(data.Builds, failedBuildView{
			ID:        b.ID,
			Pkgbase:   name,
			Error:     b.Error,
			BuildURL:  "/builds/" + id,
			LogURL:    "/builds/" + id + "#log",
			StartedAt: absTime(b.StartedAt),
		})
	}
	s.render(w, "admin_builds.html", &data)
}

// pathToken matches an absolute POSIX path: a run of slash-separated
// segments preceded by a boundary (start or whitespace), so URLs in
// error text are left alone.
var pathToken = regexp.MustCompile(`(^|\s)(/[A-Za-z0-9._~-]+(?:/[A-Za-z0-9._~-]+)+)`)

// sanitizeFlash scrubs absolute file paths out of a flash message so
// internal disk layout never leaks into the UI; each path collapses to
// a placeholder.
func sanitizeFlash(msg string) string {
	return pathToken.ReplaceAllString(msg, "${1}/…")
}

// redirectFlash redirects back to the dashboard with a flash message
// carried in the query string (no cookies). Admin actions land directly
// on /, so there is no second redirect hop.
func (s *Server) redirectFlash(w http.ResponseWriter, r *http.Request, target, kind, msg string) {
	q := url.Values{}
	q.Set(kind, sanitizeFlash(msg))
	http.Redirect(w, r, target+"?"+q.Encode(), http.StatusSeeOther)
}

// flashFromQuery turns ?error= / ?ok= into a flash message, scrubbing
// internal paths even from hand-crafted URLs.
func flashFromQuery(r *http.Request) *flash {
	if m := r.URL.Query().Get("error"); m != "" {
		return &flash{Kind: "error", Message: sanitizeFlash(m)}
	}
	if m := r.URL.Query().Get("ok"); m != "" {
		return &flash{Kind: "ok", Message: sanitizeFlash(m)}
	}
	return nil
}
