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
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/db"
)

// fakeActionsDispatcher records dispatch attempts with their task
// bindings; it is the test double for workflowDispatcher.
type fakeActionsDispatcher struct {
	mu    sync.Mutex
	calls []dispatchCall
	err   error
}

type dispatchCall struct {
	ref   string
	task  string
	token string
}

func (f *fakeActionsDispatcher) Dispatch(ctx context.Context, ref, taskID, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, dispatchCall{ref: ref, task: taskID, token: token})
	return f.err
}

func (f *fakeActionsDispatcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeActionsDispatcher) last() dispatchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return dispatchCall{}
	}
	return f.calls[len(f.calls)-1]
}

// enabledActions returns a valid worker.actions configuration for tests.
func enabledActions() config.WorkerActions {
	return config.WorkerActions{
		Enabled:        true,
		Token:          "tok",
		Repo:           "shkouyo/varve-runner",
		Workflow:       "worker-actions.yml",
		Ref:            "main",
		MaxConcurrency: 3,
		ClaimTimeout:   5 * time.Minute,
	}
}

// TestNewActionsDispatcher covers the dispatcher factory: the feature is
// off until enabled with a token, a repo and a workflow.
func TestNewActionsDispatcher(t *testing.T) {
	if d := newActionsDispatcher(nil); d != nil {
		t.Fatalf("nil config produced a dispatcher: %v", d)
	}
	if d := newActionsDispatcher(&config.WorkerActions{}); d != nil {
		t.Fatalf("disabled config produced a dispatcher: %v", d)
	}
	ac := enabledActions()
	if d := newActionsDispatcher(&ac); d == nil {
		t.Fatal("enabled config produced no dispatcher")
	}
	missing := enabledActions()
	missing.Token = ""
	if d := newActionsDispatcher(&missing); d != nil {
		t.Fatalf("enabled config without token produced a dispatcher: %v", d)
	}
}

// TestGitHubActionsDispatch drives the real client against a local HTTP
// server and checks the request shape (method, path, headers, body) and
// the status handling.
func TestGitHubActionsDispatch(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		wantErr  bool
		wantCode string // substring of the error for non-2xx responses
	}{
		{name: "success 204", status: http.StatusNoContent},
		{name: "success 200", status: http.StatusOK},
		{name: "unauthorized 401", status: http.StatusUnauthorized, wantErr: true, wantCode: "401"},
		{name: "server error 500", status: http.StatusInternalServerError, wantErr: true, wantCode: "500"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got struct {
				method, path, auth, accept, version, contentType, body string
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got.method = r.Method
				got.path = r.URL.Path
				got.auth = r.Header.Get("Authorization")
				got.accept = r.Header.Get("Accept")
				got.version = r.Header.Get("X-GitHub-Api-Version")
				got.contentType = r.Header.Get("Content-Type")
				b, _ := io.ReadAll(r.Body)
				got.body = string(b)
				w.WriteHeader(tt.status)
			}))
			defer server.Close()

			client := &githubActions{
				baseURL:  server.URL,
				token:    "tok",
				repo:     "shkouyo/varve-runner",
				workflow: "worker-actions.yml",
				http:     server.Client(),
			}
			err := client.Dispatch(context.Background(), "main", "t1", "tok1")

			if got.method != http.MethodPost {
				t.Errorf("method = %q, want POST", got.method)
			}
			if got.path != "/repos/shkouyo/varve-runner/actions/workflows/worker-actions.yml/dispatches" {
				t.Errorf("path = %q", got.path)
			}
			if got.auth != "Bearer tok" {
				t.Errorf("Authorization = %q", got.auth)
			}
			if got.accept != "application/vnd.github+json" {
				t.Errorf("Accept = %q", got.accept)
			}
			if got.version != "2022-11-28" {
				t.Errorf("X-GitHub-Api-Version = %q", got.version)
			}
			if got.contentType != "application/json" {
				t.Errorf("Content-Type = %q", got.contentType)
			}
			want := `{"inputs":{"task_id":"t1","task_token":"tok1"},"ref":"main"}`
			if got.body != want {
				t.Errorf("body = %q, want %q", got.body, want)
			}

			if tt.wantErr {
				if err == nil {
					t.Fatal("Dispatch() = nil, want error")
				}
				if !strings.Contains(err.Error(), tt.wantCode) {
					t.Errorf("error %q does not mention status %s", err, tt.wantCode)
				}
			} else if err != nil {
				t.Fatalf("Dispatch() = %v, want nil", err)
			}
		})
	}
}

// TestGitHubActionsDispatchNetworkError covers the transport-failure
// branch: the request never reaches the server and the error propagates.
func TestGitHubActionsDispatchNetworkError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached a closed server")
	}))
	url := server.URL
	server.Close()

	client := &githubActions{
		baseURL:  url,
		token:    "tok",
		repo:     "owner/repo",
		workflow: "wf.yml",
		http:     server.Client(),
	}
	if err := client.Dispatch(context.Background(), "main", "t1", "tok1"); err == nil {
		t.Fatal("Dispatch() = nil, want transport error")
	}
}

// dispatchStep advances the injected clock and then runs one dispatch
// pass, expecting the given cumulative number of dispatch attempts.
type dispatchStep struct {
	advance time.Duration
	want    int
}

// TestAutoscaleWorkers is the trigger-condition matrix: a run is
// dispatched exactly when a queued task waits without a binding and the
// concurrency ceiling leaves room.
func TestAutoscaleWorkers(t *testing.T) {
	timeout := 5 * time.Minute
	tests := []struct {
		name    string
		actions func() config.WorkerActions
		setup   func(t *testing.T, env *testEnv)
		dispErr error // dispatcher failure injected for the retry case
		steps   []dispatchStep
	}{
		{
			name: "disabled does not dispatch",
			actions: func() config.WorkerActions {
				return config.WorkerActions{}
			},
			setup: func(t *testing.T, env *testEnv) {
				env.enqueue(t, "foo", "foo")
			},
			steps: []dispatchStep{{0, 0}},
		},
		{
			name:    "no queued tasks",
			actions: enabledActions,
			steps:   []dispatchStep{{0, 0}},
		},
		{
			name:    "queued with an online pool worker still dispatches",
			actions: enabledActions,
			setup: func(t *testing.T, env *testEnv) {
				env.enqueue(t, "foo", "foo")
				env.registerWorker(t, "w1", "host", "host", 1)
			},
			steps: []dispatchStep{{0, 1}},
		},
		{
			name: "missing token never dispatches",
			actions: func() config.WorkerActions {
				ac := enabledActions()
				ac.Token = ""
				return ac
			},
			setup: func(t *testing.T, env *testEnv) {
				env.enqueue(t, "foo", "foo")
			},
			steps: []dispatchStep{{0, 0}},
		},
		{
			name:    "one queued task dispatches once with its binding",
			actions: enabledActions,
			setup: func(t *testing.T, env *testEnv) {
				env.enqueue(t, "foo", "foo")
			},
			steps: []dispatchStep{
				{0, 1},
				{time.Second, 1},
			},
		},
		{
			name: "concurrency ceiling limits a dispatch pass",
			actions: func() config.WorkerActions {
				ac := enabledActions()
				ac.MaxConcurrency = 2
				return ac
			},
			setup: func(t *testing.T, env *testEnv) {
				env.enqueue(t, "a", "a")
				env.enqueue(t, "b", "b")
				env.enqueue(t, "c", "c")
			},
			steps: []dispatchStep{{0, 2}},
		},
		{
			name: "a finished run frees capacity",
			actions: func() config.WorkerActions {
				ac := enabledActions()
				ac.MaxConcurrency = 1
				return ac
			},
			setup: func(t *testing.T, env *testEnv) {
				env.enqueue(t, "a", "a")
				env.enqueue(t, "b", "b")
			},
			steps: []dispatchStep{{0, 1}},
		},
		{
			name:    "unclaimed run is released after the claim timeout",
			actions: enabledActions,
			setup: func(t *testing.T, env *testEnv) {
				env.enqueue(t, "foo", "foo")
			},
			steps: []dispatchStep{
				{0, 1},
				{timeout - time.Second, 1},
				{2 * time.Second, 2},
			},
		},
		{
			name:    "failed dispatch waits for the claim timeout before retrying",
			actions: enabledActions,
			dispErr: context.DeadlineExceeded,
			setup: func(t *testing.T, env *testEnv) {
				env.enqueue(t, "foo", "foo")
			},
			steps: []dispatchStep{
				{0, 1},
				{timeout - time.Second, 1},
				{2 * time.Second, 2},
			},
		},
		{
			name: "claimed run keeps its binding and its capacity",
			actions: func() config.WorkerActions {
				ac := enabledActions()
				ac.MaxConcurrency = 1
				return ac
			},
			setup: func(t *testing.T, env *testEnv) {
				env.enqueue(t, "a", "a")
				env.enqueue(t, "b", "b")
			},
			steps: []dispatchStep{{0, 1}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.cfg.Worker.Actions = tt.actions()
			fake := &fakeActionsDispatcher{err: tt.dispErr}
			env.o.actions = fake
			if tt.setup != nil {
				tt.setup(t, env)
			}
			for i, step := range tt.steps {
				env.advance(step.advance)
				env.o.autoscaleWorkers(context.Background())
				if got := fake.count(); got != step.want {
					t.Errorf("step %d: dispatch calls = %d, want %d", i, got, step.want)
				}
			}
		})
	}
}

// TestAutoscaleDispatchBinding asserts the dispatched run carries the
// task id and a fresh one-shot token, and the token claims the task
// through the one-shot GetTask path.
func TestAutoscaleDispatchBinding(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Worker.Actions = enabledActions()
	fake := &fakeActionsDispatcher{}
	env.o.actions = fake
	taskID := env.enqueue(t, "foo", "foo")

	env.o.autoscaleWorkers(context.Background())
	call := fake.last()
	if call.task != taskID {
		t.Fatalf("dispatched task = %q, want %q", call.task, taskID)
	}
	if call.ref != "main" {
		t.Errorf("dispatched ref = %q, want main", call.ref)
	}
	if call.token == "" {
		t.Fatal("dispatched token is empty")
	}
	// The pre-issued token claims the queued task (one-shot GetTask).
	detail, err := env.o.GetTask(context.Background(), taskID, call.token)
	if err != nil {
		t.Fatalf("GetTask with dispatch token: %v", err)
	}
	if detail.Package.Pkgbase != "foo" {
		t.Errorf("task detail pkgbase = %q, want foo", detail.Package.Pkgbase)
	}
	task, err := env.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "running" {
		t.Errorf("task state = %q, want running after claim", task.State)
	}
	// The binding survives the claim (the run is in flight) and the
	// same token stays valid for the task protocol.
	if _, err := env.o.GetTask(context.Background(), taskID, call.token); err != nil {
		t.Errorf("idempotent GetTask: %v", err)
	}
	if _, err := env.o.AppendLog(context.Background(), taskID, call.token, LogSegment{Offset: 0, Data: "x"}); err != nil {
		t.Errorf("AppendLog with dispatch token: %v", err)
	}
	// A different token cannot drive the task.
	if _, err := env.o.GetTask(context.Background(), taskID, "wrong"); !errors.Is(err, ErrForbidden) {
		t.Errorf("GetTask with wrong token = %v, want ErrForbidden", err)
	}
}

// TestAutoscaleClaimTimeoutRotatesToken covers the expiry path: after
// the claim timeout the old binding and token die, and the re-dispatch
// uses a fresh token.
func TestAutoscaleClaimTimeoutRotatesToken(t *testing.T) {
	env := newTestEnv(t)
	ac := enabledActions()
	ac.ClaimTimeout = 5 * time.Minute
	env.cfg.Worker.Actions = ac
	fake := &fakeActionsDispatcher{}
	env.o.actions = fake
	taskID := env.enqueue(t, "foo", "foo")

	env.o.autoscaleWorkers(context.Background())
	first := fake.last()
	env.advance(ac.ClaimTimeout + time.Second)
	env.o.autoscaleWorkers(context.Background())
	if got := fake.count(); got != 2 {
		t.Fatalf("dispatch calls = %d, want 2 after expiry", got)
	}
	second := fake.last()
	if second.task != taskID {
		t.Fatalf("re-dispatched task = %q, want %q", second.task, taskID)
	}
	if second.token == first.token {
		t.Errorf("re-dispatch reused the stale token")
	}
	if _, err := env.o.GetTask(context.Background(), taskID, first.token); !errors.Is(err, ErrForbidden) {
		t.Errorf("GetTask with expired token = %v, want ErrForbidden", err)
	}
	if _, err := env.o.GetTask(context.Background(), taskID, second.token); err != nil {
		t.Errorf("GetTask with fresh token = %v, want nil", err)
	}
}

// TestAutoscaleRequeueRedispatches covers the eager release on requeue: a
// claimed run whose task stalls is re-queued, the binding dies and the
// next scan dispatches the retry immediately with a fresh token (the
// claim timeout is still far away, so only the requeue could have
// released the binding).
func TestAutoscaleRequeueRedispatches(t *testing.T) {
	env := newTestEnv(t)
	ac := enabledActions()
	env.cfg.Worker.Actions = ac
	env.cfg.Worker.StallTimeout = time.Minute
	fake := &fakeActionsDispatcher{}
	env.o.actions = fake
	taskID := env.enqueue(t, "foo", "foo")

	env.o.autoscaleWorkers(context.Background())
	first := fake.last()
	if first.task != taskID {
		t.Fatalf("first dispatch task = %q, want %q", first.task, taskID)
	}
	if _, err := env.o.GetTask(context.Background(), taskID, first.token); err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	// Stall recovery re-queues the task and releases the binding.
	env.advance(2 * time.Minute)
	if err := env.o.scanStalled(context.Background()); err != nil {
		t.Fatalf("scanStalled: %v", err)
	}
	task, err := env.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "queued" {
		t.Fatalf("state = %q, want queued after stall requeue", task.State)
	}
	env.o.autoscaleWorkers(context.Background())
	if got := fake.count(); got != 2 {
		t.Fatalf("dispatch calls after requeue = %d, want 2", got)
	}
	second := fake.last()
	if second.token == first.token {
		t.Errorf("re-dispatch reused the stale token %q", first.token)
	}
	if _, err := env.o.GetTask(context.Background(), taskID, first.token); !errors.Is(err, ErrForbidden) {
		t.Errorf("GetTask with stale token = %v, want ErrForbidden", err)
	}
}

// TestAutoscaleConcurrencyGap covers the ceiling arithmetic: a claimed
// run keeps its slot until the task is terminal, then the next queued
// task is dispatched.
func TestAutoscaleConcurrencyGap(t *testing.T) {
	env := newTestEnv(t)
	ac := enabledActions()
	ac.MaxConcurrency = 1
	env.cfg.Worker.Actions = ac
	fake := &fakeActionsDispatcher{}
	env.o.actions = fake
	env.enqueue(t, "a", "a")
	env.enqueue(t, "b", "b")

	env.o.autoscaleWorkers(context.Background())
	if got := fake.count(); got != 1 {
		t.Fatalf("dispatch calls = %d, want 1 (ceiling 1)", got)
	}
	// Claim the run's task and finish it: the slot frees.
	call := fake.last()
	if _, err := env.o.GetTask(context.Background(), call.task, call.token); err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if err := env.store.WithTx(context.Background(), func(tx *db.Tx) error {
		return tx.FinalizeTask(context.Background(), call.task, "succeeded", "", env.now.UTC(), nil, nil)
	}); err != nil {
		t.Fatalf("finalize task: %v", err)
	}
	env.o.autoscaleWorkers(context.Background())
	if got := fake.count(); got != 2 {
		t.Fatalf("dispatch calls after finish = %d, want 2", got)
	}
	if last := fake.last(); last.task == call.task {
		t.Errorf("re-dispatched the finished task %q, want the next queued one", last.task)
	}
}
