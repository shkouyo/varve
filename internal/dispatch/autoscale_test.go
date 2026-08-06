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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/config"
)

// fakeActionsDispatcher records dispatch attempts; it is the test double
// for workflowDispatcher in the trigger-condition tests.
type fakeActionsDispatcher struct {
	mu    sync.Mutex
	calls int
	ref   string
	err   error
}

func (f *fakeActionsDispatcher) Dispatch(ctx context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.ref = ref
	return f.err
}

func (f *fakeActionsDispatcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// enabledActions returns a valid worker.actions configuration for tests.
func enabledActions() config.WorkerActions {
	return config.WorkerActions{
		Enabled:  true,
		Token:    "tok",
		Repo:     "shkouyo/varve-runner",
		Workflow: "worker-actions.yml",
		Ref:      "main",
		Cooldown: 3 * time.Minute,
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
			err := client.Dispatch(context.Background(), "main")

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
			if got.body != `{"ref":"main"}` {
				t.Errorf("body = %q", got.body)
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
	if err := client.Dispatch(context.Background(), "main"); err == nil {
		t.Fatal("Dispatch() = nil, want transport error")
	}
}

// autoscaleStep advances the injected clock and then runs one scan,
// expecting the given cumulative number of dispatch attempts.
type autoscaleStep struct {
	advance time.Duration
	want    int
}

// TestAutoscaleWorkers is the trigger-condition matrix: the workflow is
// dispatched exactly when queued tasks wait, no worker is online and the
// cooldown has elapsed.
func TestAutoscaleWorkers(t *testing.T) {
	cooldown := 3 * time.Minute
	tests := []struct {
		name    string
		actions func() config.WorkerActions
		setup   func(t *testing.T, env *testEnv)
		dispErr error // dispatcher failure injected for the retry case
		steps   []autoscaleStep
	}{
		{
			name: "disabled does not dispatch",
			actions: func() config.WorkerActions {
				return config.WorkerActions{}
			},
			setup: func(t *testing.T, env *testEnv) {
				env.enqueue(t, "foo", "foo")
			},
			steps: []autoscaleStep{{0, 0}},
		},
		{
			name:    "no queued tasks",
			actions: enabledActions,
			steps:   []autoscaleStep{{0, 0}},
		},
		{
			name:    "queued with an online worker",
			actions: enabledActions,
			setup: func(t *testing.T, env *testEnv) {
				env.enqueue(t, "foo", "foo")
				env.registerWorker(t, "w1", "host", "host", 1)
			},
			steps: []autoscaleStep{{0, 0}},
		},
		{
			name:    "queued with only a disabled worker",
			actions: enabledActions,
			setup: func(t *testing.T, env *testEnv) {
				env.enqueue(t, "foo", "foo")
				env.registerWorker(t, "w1", "agent", "pool", 1)
				if err := env.o.DisableWorker(context.Background(), "w1"); err != nil {
					t.Fatalf("DisableWorker: %v", err)
				}
			},
			steps: []autoscaleStep{{0, 1}},
		},
		{
			name:    "queued with only a stale-heartbeat worker",
			actions: enabledActions,
			setup: func(t *testing.T, env *testEnv) {
				env.enqueue(t, "foo", "foo")
				env.registerWorker(t, "w1", "agent", "pool", 1)
				env.advance(env.cfg.Worker.HeartbeatTimeout + time.Second)
			},
			steps: []autoscaleStep{{0, 1}},
		},
		{
			name:    "cooldown active after a previous dispatch",
			actions: enabledActions,
			setup: func(t *testing.T, env *testEnv) {
				env.enqueue(t, "foo", "foo")
				env.o.actionsMu.Lock()
				env.o.lastDispatchAt = env.now
				env.o.actionsMu.Unlock()
			},
			steps: []autoscaleStep{{0, 0}},
		},
		{
			name:    "dispatches after the cooldown, then waits again",
			actions: enabledActions,
			setup: func(t *testing.T, env *testEnv) {
				env.enqueue(t, "foo", "foo")
				env.o.actionsMu.Lock()
				env.o.lastDispatchAt = env.now.Add(-cooldown - time.Second)
				env.o.actionsMu.Unlock()
			},
			steps: []autoscaleStep{
				{0, 1},
				{time.Second, 1},
				{cooldown + time.Second, 2},
			},
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
			steps: []autoscaleStep{{0, 0}},
		},
		{
			name:    "failed dispatch respects the cooldown before retrying",
			actions: enabledActions,
			dispErr: context.DeadlineExceeded,
			setup: func(t *testing.T, env *testEnv) {
				env.enqueue(t, "foo", "foo")
			},
			steps: []autoscaleStep{
				{0, 1},
				{cooldown - time.Second, 1},
				{2 * time.Second, 2},
			},
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

// TestAutoscaleWorkersRef covers the ref handed to the dispatcher.
func TestAutoscaleWorkersRef(t *testing.T) {
	env := newTestEnv(t)
	ac := enabledActions()
	ac.Ref = "custom-branch"
	env.cfg.Worker.Actions = ac
	fake := &fakeActionsDispatcher{}
	env.o.actions = fake
	env.enqueue(t, "foo", "foo")
	env.o.autoscaleWorkers(context.Background())
	if fake.count() != 1 || fake.ref != "custom-branch" {
		t.Fatalf("dispatch calls = %d ref = %q, want 1 and custom-branch", fake.count(), fake.ref)
	}
}
