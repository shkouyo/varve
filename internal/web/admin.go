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
	"strconv"
	"time"

	"git.0x0f.dev/varve/internal/db"
)

// adminData feeds admin.html: the public dashboard view plus the task
// queue (queued/running, with cancel buttons) and action buttons on
// recent builds and workers.
type adminData struct {
	base
	Counts   []statusCount
	QueueLen int
	Recent   []recentBuildView
	Workers  []workerView
	Tasks    []taskView
}

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
	ID        int64
	Pkgbase   string
	Error     string
	BuildURL  string
	LogURL    string
	StartedAt string
}

// handleAdmin renders GET /admin.
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	stats, err := s.orch.Stats(ctx)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, "Failed to load dashboard statistics.")
		return
	}
	workers, err := s.store.ListWorkers(ctx)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, "Failed to load the worker list.")
		return
	}
	tasks, err := s.store.ListActiveTasks(ctx)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, "Failed to load the task queue.")
		return
	}

	data := adminData{
		base:     s.page("Admin", flashFromQuery(r)),
		Counts:   statusCounts(stats.ByStatus),
		QueueLen: stats.QueueLen,
		Workers:  workerViews(workers, time.Now()),
		Recent:   s.recentBuildViews(ctx, stats.RecentBuilds, workers),
		Tasks:    s.taskViews(ctx, tasks, workers),
	}
	data.Nav = "admin"
	s.render(w, "admin.html", data)
}

// taskViews resolves task rows into the template view (package name and
// worker name).
func (s *Server) taskViews(ctx context.Context, tasks []db.Task, workers []db.Worker) []taskView {
	workerNames := make(map[int64]string, len(workers))
	for _, w := range workers {
		workerNames[w.ID] = w.Name
	}
	pkgNames := make(map[int64]string)
	now := time.Now()
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
			CreatedAt: formatWhen(&t.CreatedAt, now),
			CancelURL: "/admin/tasks/" + t.ID + "/cancel",
		})
	}
	return out
}

// handleAdminRebuild posts a manual rebuild for a package.
func (s *Server) handleAdminRebuild(w http.ResponseWriter, r *http.Request) {
	pkgbase := r.PathValue("pkgbase")
	if err := s.orch.RebuildPackage(r.Context(), pkgbase); err != nil {
		s.redirectFlash(w, r, "/admin", "error", "Rebuild failed: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "/admin", "ok", "Rebuild queued for "+pkgbase)
}

// handleAdminCancel cancels a queued or running task.
func (s *Server) handleAdminCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.orch.CancelTask(r.Context(), id); err != nil {
		s.redirectFlash(w, r, "/admin", "error", "Cancel failed: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "/admin", "ok", "Cancellation requested for task "+id)
}

// handleAdminDisable disables a worker (no new assignments).
func (s *Server) handleAdminDisable(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.orch.DisableWorker(r.Context(), name); err != nil {
		s.redirectFlash(w, r, "/admin", "error", "Disable failed: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "/admin", "ok", "Worker "+name+" disabled")
}

// handleAdminRemove removes a worker record.
func (s *Server) handleAdminRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.orch.RemoveWorker(r.Context(), name); err != nil {
		s.redirectFlash(w, r, "/admin", "error", "Remove failed: "+err.Error())
		return
	}
	s.redirectFlash(w, r, "/admin", "ok", "Worker "+name+" removed")
}

// handleAdminBuilds renders GET /admin/builds?failed=1, the long-term
// failed build retention list.
func (s *Server) handleAdminBuilds(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page := parsePage(r.URL.Query().Get("page"))
	builds, total, err := s.store.ListBuilds(ctx, page, perPage, true)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, "Failed to load the failed build list.")
		return
	}
	workers, err := s.store.ListWorkers(ctx)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, "Failed to load the worker list.")
		return
	}

	data := adminBuildsData{
		base:  s.page("Failed builds", flashFromQuery(r)),
		Page:  page,
		Pages: pages(total, perPage),
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
		id := strconv.FormatInt(b.ID, 10)
		data.Builds = append(data.Builds, failedBuildView{
			ID:        b.ID,
			Pkgbase:   name,
			Error:     b.Error,
			BuildURL:  "/builds/" + id,
			LogURL:    "/builds/" + id + "/log",
			StartedAt: formatWhen(b.StartedAt, time.Now()),
		})
	}
	s.render(w, "admin_builds.html", data)
}

// redirectFlash redirects back to the admin area with a flash message
// carried in the query string (no cookies).
func (s *Server) redirectFlash(w http.ResponseWriter, r *http.Request, target, kind, msg string) {
	q := url.Values{}
	q.Set(kind, msg)
	http.Redirect(w, r, target+"?"+q.Encode(), http.StatusSeeOther)
}

// flashFromQuery turns ?error= / ?ok= into a flash message.
func flashFromQuery(r *http.Request) *flash {
	if m := r.URL.Query().Get("error"); m != "" {
		return &flash{Kind: "error", Message: m}
	}
	if m := r.URL.Query().Get("ok"); m != "" {
		return &flash{Kind: "ok", Message: m}
	}
	return nil
}
