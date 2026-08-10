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
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/db"
)

// githubAPIBase is the GitHub REST API origin.
const githubAPIBase = "https://api.github.com"

// maxDispatchesPerPass caps the number of workflow runs triggered by one
// scheduler scan, so a queue of N tasks delays the pass by at most
// maxDispatchesPerPass × dispatchTimeout instead of N × dispatchTimeout.
// Undispatched tasks simply wait for the next scan; the claim-timeout
// budget is unaffected.
const maxDispatchesPerPass = 4

// dispatchTimeout bounds one workflow dispatch call so a hung GitHub API
// cannot stall the scheduler pass beyond a single attempt. Tests may
// shorten it.
var dispatchTimeout = 10 * time.Second

// githubAPIVersion is the API version pinned in the request header; the
// Actions endpoints require it.
const githubAPIVersion = "2022-11-28"

// dispatchEntry is the scheduler's record of one dispatched workflow run:
// the one-shot task token handed to the runner and the dispatch time.
// failed marks a dispatch attempt that the API rejected; the entry then
// only expires by age, so a broken endpoint is not hammered by the 30s
// scan.
type dispatchEntry struct {
	token        string
	dispatchedAt time.Time
	failed       bool
}

// workflowDispatcher triggers a workflow run through the GitHub Actions
// API. The orchestrator holds the concrete client built from
// worker.actions; tests substitute a recorder. taskID and token bind the
// run to its task: the workflow passes them to the container as the
// one-shot task id and claim token.
type workflowDispatcher interface {
	Dispatch(ctx context.Context, ref, taskID, token string) error
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

// Dispatch sends a workflow_dispatch request for ref with the task
// binding carried in the inputs. A non-2xx response is reported as an
// error carrying the status code; the GitHub API answers 204 No Content
// on success.
func (g *githubActions) Dispatch(ctx context.Context, ref, taskID, token string) error {
	endpoint := g.baseURL + "/repos/" + g.repo + "/actions/workflows/" + g.workflow + "/dispatches"
	body, err := json.Marshal(map[string]any{
		"ref": ref,
		"inputs": map[string]string{
			"task_id":    taskID,
			"task_token": token,
		},
	})
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
		http:     &http.Client{Timeout: 10 * time.Second},
	}
}

// autoscaleWorkers runs one per-task dispatch pass: queued tasks that are
// not bound to a run yet are dispatched as individual workflow runs, up
// to the max_concurrency gap left by in-flight runs. The scan is
// best-effort: every failure is logged and never aborts the scheduler.
//
// Run accounting is the in-memory dispatch map (dispatched -> claimed ->
// done), backed by the persisted dispatch bindings (tasks.dispatched_at
// and claim_token, migration 010): an entry is created when a run is
// dispatched, kept while the task is claimed or running, and pruned when
// the task becomes terminal. Each pass rebuilds the map from the database
// first, so a controller restart recovers in-flight bindings instead of
// double-dispatching them. A dispatched run must claim its task within
// the claim timeout or its entry and token are released so the task can
// be dispatched again. A rejected dispatch attempt is only retried after
// the same timeout, so a broken endpoint cannot be hammered by the 30s
// scan. Requeued tasks (stall recovery, retry) release their binding
// eagerly through releaseDispatch so the next attempt is dispatched
// immediately.
func (o *OrchestratorImpl) autoscaleWorkers(ctx context.Context) {
	ac := o.cfg.Worker.Actions
	if !ac.Enabled {
		return
	}
	if ac.Token == "" {
		log.Printf("dispatch: actions dispatch: skipped: token is empty")
		return
	}
	if o.actions == nil {
		log.Printf("dispatch: actions dispatch: skipped: dispatcher unavailable")
		return
	}

	active, err := o.store.ListActiveTasks(ctx)
	if err != nil {
		log.Printf("dispatch: actions dispatch: skipped: list tasks: %v", err)
		return
	}

	now := o.now().UTC()
	stateByID := make(map[string]string, len(active))
	var queued []db.Task
	for i := range active {
		stateByID[active[i].ID] = active[i].State
		if active[i].State == "queued" {
			queued = append(queued, active[i])
		}
	}

	// Rebuild the in-memory dispatch map from the persisted bindings so a
	// controller restart cannot double-dispatch runs that are still within
	// their claim window: a queued task with a binding dispatched before
	// the restart is treated exactly like a freshly dispatched one, and
	// its original token stays valid until the claim timeout elapses.
	o.dispatchMu.Lock()
	bindings, err := o.store.ListDispatchBindings(ctx)
	if err != nil {
		o.dispatchMu.Unlock()
		log.Printf("dispatch: actions dispatch: skipped: list bindings: %v", err)
		return
	}
	for _, b := range bindings {
		if _, ok := o.dispatchMap[b.TaskID]; !ok {
			o.dispatchMap[b.TaskID] = dispatchEntry{token: b.Token, dispatchedAt: b.DispatchedAt}
		}
	}
	o.dispatchMu.Unlock()

	// Prune finished runs, release expired unclaimed ones and count the
	// in-flight runs in one locked pass. The claim timeout is measured
	// from the dispatch time, so a run that spun up slowly but claimed
	// in time is never released while alive.
	o.dispatchMu.Lock()
	var expired []string
	inFlight := 0
	for id, e := range o.dispatchMap {
		state, ok := stateByID[id]
		if !ok {
			delete(o.dispatchMap, id) // task terminal: run done
			continue
		}
		if state == "queued" && now.Sub(e.dispatchedAt) > ac.ClaimTimeout {
			// The run never claimed its task: release the binding and
			// the token so the task can be dispatched again.
			delete(o.dispatchMap, id)
			expired = append(expired, id)
			continue
		}
		if !e.failed {
			inFlight++
		}
	}
	o.dispatchMu.Unlock()
	for _, id := range expired {
		o.clearToken(ctx, id)
		log.Printf("dispatch: actions: task %s not claimed in %s, released for re-dispatch", id, ac.ClaimTimeout)
	}

	gap := ac.MaxConcurrency - inFlight
	dispatched := 0
	for _, task := range queued {
		if gap <= 0 || dispatched >= maxDispatchesPerPass {
			return
		}
		o.dispatchMu.Lock()
		_, bound := o.dispatchMap[task.ID]
		o.dispatchMu.Unlock()
		if bound {
			continue // already dispatched, awaiting its claim
		}
		o.dispatchTask(ctx, task, ac)
		gap--
		dispatched++
	}
}

// dispatchTask binds one queued task to a new workflow run: a fresh
// one-shot token is persisted for the task and the workflow is triggered
// with the task id and token as dispatch inputs. The binding is recorded
// before the API call so a concurrent scan cannot dispatch the same task
// twice and a controller restart recovers the binding; a rejected attempt
// is marked failed and only retried after the claim timeout.
func (o *OrchestratorImpl) dispatchTask(ctx context.Context, task db.Task, ac config.WorkerActions) {
	token, err := randomToken()
	if err != nil {
		log.Printf("dispatch: actions dispatch %s: %v", task.ID, err)
		return
	}
	now := o.now().UTC()
	// Persist the binding first: a task that was concurrently claimed is
	// skipped (the claimant owns it and wrote its own token), and the
	// persisted record doubles as the cross-restart double-dispatch guard.
	if err := o.store.SetDispatchBinding(ctx, task.ID, token, now); err != nil {
		if !errors.Is(err, db.ErrConflict) {
			log.Printf("dispatch: actions dispatch %s: %v", task.ID, err)
		}
		return
	}
	o.cacheToken(task.ID, token)
	o.dispatchMu.Lock()
	o.dispatchMap[task.ID] = dispatchEntry{token: token, dispatchedAt: now}
	o.dispatchMu.Unlock()
	dctx, cancel := context.WithTimeout(ctx, dispatchTimeout)
	defer cancel()
	if err := o.actions.Dispatch(dctx, ac.Ref, task.ID, token); err != nil {
		o.dispatchMu.Lock()
		if e, ok := o.dispatchMap[task.ID]; ok {
			e.failed = true
			o.dispatchMap[task.ID] = e
		}
		o.dispatchMu.Unlock()
		log.Printf("dispatch: actions dispatch %s: %v", task.ID, err)
		return
	}
	log.Printf("dispatch: actions: task %s dispatched to %s workflow %s on %s", task.ID, ac.Repo, ac.Workflow, ac.Ref)
}

// releaseDispatch unbinds a task from its run (requeue paths): the
// binding and the one-shot token die so the task is dispatched again by
// the next scan with a fresh token. The binding is cleared even when the
// in-memory map has no entry (a binding dispatched before a controller
// restart), so a requeued task is never held by a stale binding. No-op
// for tasks that were never dispatched.
func (o *OrchestratorImpl) releaseDispatch(ctx context.Context, taskID string) {
	o.dispatchMu.Lock()
	delete(o.dispatchMap, taskID)
	o.dispatchMu.Unlock()
	o.clearToken(ctx, taskID)
}
