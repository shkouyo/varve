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
	"sort"
	"time"

	"git.0x0f.dev/varve/internal/db"
)

// dashboardData feeds dashboard.html: build status counts, queue length,
// recent builds with their executing node name, and the worker online
// overview. Admin marks an authenticated request so the template renders
// the task queue and admin actions; Tasks carries the queue then.
type dashboardData struct {
	base
	Counts   []statusCount
	QueueLen int
	Recent   []recentBuildView
	Workers  []workerView
	Admin    bool
	Tasks    []taskView
}

// statusCount is one entry of the build status breakdown, kept in a
// stable display order.
type statusCount struct {
	Status string
	Count  int
}

// statusCounts orders the by-status map for display, always rendering
// the six known build statuses — zero counts included, so every card
// stays visible ("0" is shown, never omitted) — followed by any
// unknown statuses the map carries.
func statusCounts(by map[string]int) []statusCount {
	order := []string{"queued", "assigned", "running", "succeeded", "failed", "cancelled"}
	seen := make(map[string]bool, len(by))
	out := make([]statusCount, 0, len(by)+len(order))
	for _, s := range order {
		out = append(out, statusCount{Status: s, Count: by[s]})
		seen[s] = true
	}
	for s, n := range by {
		if !seen[s] {
			out = append(out, statusCount{Status: s, Count: n})
		}
	}
	return out
}

// recentBuildView is one recent build row with the executing node name
// (builds.worker_name in plain text, with the workers table as a
// fallback for rows recorded before the column was populated).
type recentBuildView struct {
	ID         string
	Pkgbase    string
	Status     string
	WorkerName string
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// workerNameOf resolves the display name of the machine behind a build:
// the plain-text worker_name column wins, the workers table is the
// fallback for rows that predate it.
func workerNameOf(b db.Build, byID map[int64]string) string {
	if b.WorkerName != "" {
		return b.WorkerName
	}
	return byID[b.WorkerID]
}

// workerView is one worker row of the online overview. Performance
// indicators are rendered as text; the worker table does not carry cgroup
// samples, so it shows role, mode, arch, capacity and heartbeat age
// instead.
type workerView struct {
	Name          string
	Status        string
	Online        bool
	Role          string
	Mode          string
	Arch          string
	Capacity      int
	Version       string
	LastHeartbeat *time.Time
}

// handleDashboard renders GET /.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data, err := s.dashboardData(r)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Failed to load overview statistics.")
		return
	}
	data.Nav = "dashboard"
	s.render(w, "dashboard.html", &data)
}

// dashboardData assembles the dashboard page data. The public view shows
// status counts, the queue length, recent builds and the worker overview;
// an authenticated request additionally marks the page as admin (the
// template then renders the task queue and admin actions) and carries the
// flash of the last admin action. The recent-build count comes from
// web.recent_builds, never a hardcoded slice.
func (s *Server) dashboardData(r *http.Request) (dashboardData, error) {
	ctx := r.Context()
	stats, err := s.orch.Stats(ctx)
	if err != nil {
		return dashboardData{}, err
	}
	workers, err := s.store.ListWorkers(ctx)
	if err != nil {
		return dashboardData{}, err
	}
	recent := stats.RecentBuilds
	if n := s.cfg.Web.RecentBuilds; n > 0 && len(recent) > n {
		recent = recent[:n]
	}
	data := dashboardData{
		base:     s.page(r, "Overview", nil),
		Counts:   statusCounts(stats.ByStatus),
		QueueLen: stats.QueueLen,
		Recent:   s.recentBuildViews(ctx, recent, workers),
		Workers:  workerViews(workers),
	}
	if s.authorized(r) {
		data.Admin = true
		data.Flash = flashFromQuery(r)
		tasks, err := s.store.ListActiveTasks(ctx)
		if err != nil {
			return dashboardData{}, err
		}
		data.Tasks = s.taskViews(ctx, tasks, workers)
	}
	return data, nil
}

// recentBuildViews resolves recent build rows into the template view: the
// executing node name (builds.worker_id joins workers.name) and the
// pkgbase of the package row. Timestamps pass through raw; the template
// renders them via absTime with a placeholder for missing values.
func (s *Server) recentBuildViews(ctx context.Context, builds []db.Build, workers []db.Worker) []recentBuildView {
	workerNames := make(map[int64]string, len(workers))
	for _, w := range workers {
		workerNames[w.ID] = w.Name
	}
	pkgNames := make(map[int64]string)
	out := make([]recentBuildView, 0, len(builds))
	for _, b := range builds {
		view := recentBuildView{
			ID:         b.ID,
			Status:     b.Status,
			WorkerName: workerNameOf(b, workerNames),
			StartedAt:  b.StartedAt,
			FinishedAt: b.FinishedAt,
		}
		if name, ok := pkgNames[b.PackageID]; ok {
			view.Pkgbase = name
		} else if p, err := s.store.GetPackageByID(ctx, b.PackageID); err == nil {
			pkgNames[b.PackageID] = p.Pkgbase
			view.Pkgbase = p.Pkgbase
		}
		out = append(out, view)
	}
	return out
}

// workerViews converts worker rows into the template view, marking a node
// online when its recorded status is online. The heartbeat passes through
// raw; the template renders it via absTime with a placeholder for missing
// values.
func workerViews(workers []db.Worker) []workerView {
	out := make([]workerView, 0, len(workers))
	for _, w := range workers {
		out = append(out, workerView{
			Name:          w.Name,
			Status:        w.Status,
			Online:        w.Status == "online",
			Role:          w.Role,
			Mode:          w.Mode,
			Arch:          w.Arch,
			Capacity:      w.Capacity,
			Version:       w.Version,
			LastHeartbeat: w.LastHeartbeat,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
