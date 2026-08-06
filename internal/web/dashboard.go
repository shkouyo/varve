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
// overview.
type dashboardData struct {
	base
	Counts   []statusCount
	QueueLen int
	Recent   []recentBuildView
	Workers  []workerView
}

// statusCount is one entry of the build status breakdown, kept in a
// stable display order.
type statusCount struct {
	Status string
	Count  int
}

// statusCounts orders the by-status map for display, leading with the
// active states.
func statusCounts(by map[string]int) []statusCount {
	order := []string{"queued", "assigned", "running", "succeeded", "failed", "cancelled"}
	seen := make(map[string]bool)
	out := make([]statusCount, 0, len(by))
	for _, s := range order {
		if n, ok := by[s]; ok {
			out = append(out, statusCount{Status: s, Count: n})
			seen[s] = true
		}
	}
	for s, n := range by {
		if !seen[s] {
			out = append(out, statusCount{Status: s, Count: n})
		}
	}
	return out
}

// recentBuildView is one recent build row with the executing node name
// resolved (builds.worker_id joins workers.name).
type recentBuildView struct {
	ID         int64
	Pkgbase    string
	Status     string
	WorkerName string
	StartedAt  string
	FinishedAt string
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
	LastHeartbeat string
}

// handleDashboard renders GET /.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
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

	data := dashboardData{
		base:     s.page("Dashboard", nil),
		Counts:   statusCounts(stats.ByStatus),
		QueueLen: stats.QueueLen,
		Workers:  workerViews(workers, time.Now()),
		Recent:   s.recentBuildViews(ctx, stats.RecentBuilds, workers),
	}
	data.Nav = "dashboard"

	s.render(w, "dashboard.html", data)
}

// recentBuildViews resolves recent build rows into the template view: the
// executing node name (builds.worker_id joins workers.name) and the
// pkgbase of the package row.
func (s *Server) recentBuildViews(ctx context.Context, builds []db.Build, workers []db.Worker) []recentBuildView {
	workerNames := make(map[int64]string, len(workers))
	for _, w := range workers {
		workerNames[w.ID] = w.Name
	}
	pkgNames := make(map[int64]string)
	now := time.Now()
	out := make([]recentBuildView, 0, len(builds))
	for _, b := range builds {
		view := recentBuildView{
			ID:         b.ID,
			Status:     b.Status,
			WorkerName: workerNames[b.WorkerID],
			StartedAt:  formatWhen(b.StartedAt, now),
			FinishedAt: formatWhen(b.FinishedAt, now),
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
// online when its recorded status is online.
func workerViews(workers []db.Worker, now time.Time) []workerView {
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
			LastHeartbeat: formatWhen(w.LastHeartbeat, now),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// formatWhen renders an optional timestamp as a relative age ("2m ago")
// or "never".
func formatWhen(t *time.Time, now time.Time) string {
	if t == nil {
		return "never"
	}
	d := now.Sub(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmtInt(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return fmtInt(int(d.Hours())) + "h ago"
	default:
		return fmtInt(int(d.Hours()/24)) + "d ago"
	}
}

// fmtInt is a tiny helper avoiding strconv imports in templates (the
// values are always non-negative).
func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
