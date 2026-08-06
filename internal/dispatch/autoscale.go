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

package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"git.0x0f.dev/varve/internal/config"
)

// githubAPIBase is the GitHub REST API origin.
const githubAPIBase = "https://api.github.com"

// githubAPIVersion is the API version pinned in the request header; the
// Actions endpoints require it.
const githubAPIVersion = "2022-11-28"

// workflowDispatcher triggers a workflow run through the GitHub Actions
// API. The orchestrator holds the concrete client built from
// worker.actions; tests substitute a recorder.
type workflowDispatcher interface {
	Dispatch(ctx context.Context, ref string) error
}

// githubActions dispatches a workflow run via the workflow_dispatch
// endpoint. The token must be a personal access token with the
// actions:write permission on the runner repository.
type githubActions struct {
	baseURL  string // API base URL; tests point it at a local server
	token    string
	repo     string // owner/repo slug of the runner repository
	workflow string // workflow file name inside the repository
	http     *http.Client
}

// Compile-time assertion: githubActions implements workflowDispatcher.
var _ workflowDispatcher = (*githubActions)(nil)

// Dispatch sends a workflow_dispatch request for ref. A non-2xx response
// is reported as an error carrying the status code; the GitHub API answers
// 204 No Content on success.
func (g *githubActions) Dispatch(ctx context.Context, ref string) error {
	endpoint := g.baseURL + "/repos/" + g.repo + "/actions/workflows/" + g.workflow + "/dispatches"
	body, err := json.Marshal(map[string]string{"ref": ref})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	resp, err := g.http.Do(req)
	if err != nil {
		return fmt.Errorf("github actions dispatch %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("github actions dispatch %s: status %d", endpoint, resp.StatusCode)
	}
	return nil
}

// newActionsDispatcher builds the workflow dispatcher from the configured
// worker.actions section, or nil when the feature is disabled or
// incomplete. The config validation already rejects the incomplete
// enabled state; the guard keeps programmatically built configs safe.
func newActionsDispatcher(ac *config.WorkerActions) workflowDispatcher {
	if ac == nil || !ac.Enabled || ac.Token == "" || ac.Repo == "" || ac.Workflow == "" {
		return nil
	}
	return &githubActions{
		baseURL:  githubAPIBase,
		token:    ac.Token,
		repo:     ac.Repo,
		workflow: ac.Workflow,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

// autoscaleWorkers runs one autoscaling pass: when worker.actions is
// enabled, at least one task is queued, no worker is online and the
// cooldown since the last dispatch attempt has elapsed, the configured
// runner workflow is triggered so a worker can drain the queue. The scan
// is best-effort: every failure is logged and never aborts the scheduler.
func (o *OrchestratorImpl) autoscaleWorkers(ctx context.Context) {
	ac := o.cfg.Worker.Actions
	if !ac.Enabled {
		return
	}
	if ac.Token == "" {
		log.Printf("dispatch: actions autoscale: skipped: token is empty")
		return
	}
	if o.actions == nil {
		log.Printf("dispatch: actions autoscale: skipped: dispatcher unavailable")
		return
	}

	// Any queued task means the queue is not being drained.
	active, err := o.store.ListActiveTasks(ctx)
	if err != nil {
		log.Printf("dispatch: actions autoscale: skipped: list tasks: %v", err)
		return
	}
	queued := false
	for i := range active {
		if active[i].State == "queued" {
			queued = true
			break
		}
	}
	if !queued {
		log.Printf("dispatch: actions autoscale: skipped: no queued tasks")
		return
	}

	online, err := o.onlineWorkerCount(ctx)
	if err != nil {
		log.Printf("dispatch: actions autoscale: skipped: list workers: %v", err)
		return
	}
	if online > 0 {
		log.Printf("dispatch: actions autoscale: skipped: %d worker(s) online", online)
		return
	}

	// The cooldown gates both successful and failed attempts so a broken
	// endpoint cannot be hammered by the 30s scan.
	o.actionsMu.Lock()
	elapsed := o.now().Sub(o.lastDispatchAt)
	if elapsed < ac.Cooldown {
		remaining := ac.Cooldown - elapsed
		o.actionsMu.Unlock()
		log.Printf("dispatch: actions autoscale: skipped: dispatch cooldown %s remaining", remaining.Round(time.Second))
		return
	}
	o.lastDispatchAt = o.now()
	o.actionsMu.Unlock()

	if err := o.actions.Dispatch(ctx, ac.Ref); err != nil {
		log.Printf("dispatch: actions autoscale: dispatch failed: %v", err)
		return
	}
	log.Printf("dispatch: actions autoscale: triggered %s workflow %s on %s", ac.Repo, ac.Workflow, ac.Ref)
}

// onlineWorkerCount counts workers that can pick up new work: their status
// is not disabled and their last heartbeat is younger than
// heartbeat_timeout.
func (o *OrchestratorImpl) onlineWorkerCount(ctx context.Context) (int, error) {
	workers, err := o.store.ListWorkers(ctx)
	if err != nil {
		return 0, err
	}
	now := o.now().UTC()
	n := 0
	for i := range workers {
		w := &workers[i]
		if w.Status == "disabled" {
			continue
		}
		if w.LastHeartbeat == nil || now.Sub(*w.LastHeartbeat) > o.cfg.Worker.HeartbeatTimeout {
			continue
		}
		n++
	}
	return n, nil
}
