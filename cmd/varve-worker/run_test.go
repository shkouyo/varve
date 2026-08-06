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

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/api"
	"git.0x0f.dev/varve/internal/config"
)

// clearWorkerEnv resets every VARVE_* variable the worker reads. Empty
// values are treated as unset by LoadWorker, so this also covers
// variables inherited from the test environment (config package
// convention).
func clearWorkerEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"VARVE_CONTROLLER_URL", "VARVE_TOKEN", "VARVE_ROLE", "VARVE_WORKER_NAME",
		"VARVE_WORKER_ARCH", "VARVE_WORKER_CONCURRENCY", "VARVE_WORKER_IMAGE",
		"VARVE_CONTAINER_RUNTIME", "VARVE_PULL_IMAGE", "VARVE_ONE_SHOT",
		"VARVE_TASK_ID", "VARVE_TASK_TOKEN", "VARVE_POOL_IDLE_TIMEOUT", "VARVE_DATA_DIR",
	} {
		t.Setenv(k, "")
	}
}

// fakeRunner records the context it was run with and returns a preset
// error; it stands in for host.Runner and agent.Runner.
type fakeRunner struct {
	ctx context.Context
	err error
}

func (f *fakeRunner) Run(ctx context.Context) error {
	f.ctx = ctx
	return f.err
}

// constructorRecorder records every invocation of a runner constructor.
type constructorRecorder struct {
	cfg    *config.WorkerConfig
	client *api.Client
	ran    bool
}

// stubConstructors replaces the package-level runner constructors with
// recorders and restores the defaults when the test ends.
func stubConstructors(t *testing.T) (*constructorRecorder, *constructorRecorder) {
	t.Helper()
	host := &constructorRecorder{}
	agent := &constructorRecorder{}

	prevHost, prevAgent := newHostRunner, newAgentRunner
	newHostRunner = func(cfg *config.WorkerConfig, client *api.Client) (runner, error) {
		host.ran, host.cfg, host.client = true, cfg, client
		return &fakeRunner{}, nil
	}
	newAgentRunner = func(cfg *config.WorkerConfig, client *api.Client) runner {
		agent.ran, agent.cfg, agent.client = true, cfg, client
		return &fakeRunner{}
	}
	t.Cleanup(func() {
		newHostRunner, newAgentRunner = prevHost, prevAgent
	})
	return host, agent
}

// clientFields reads the unexported baseURL and token of an api.Client,
// verifying that run wired api.NewClient with the loaded configuration.
func clientFields(t *testing.T, c *api.Client) (baseURL, token string) {
	t.Helper()
	if c == nil {
		t.Fatal("nil client passed to runner constructor")
	}
	v := reflect.ValueOf(c).Elem()
	return v.FieldByName("baseURL").String(), v.FieldByName("token").String()
}

// TestRunDispatch covers the role dispatch matrix: every (role,
// one-shot) combination dispatches to the correct runner constructor
// with the loaded configuration and the matching client.
func TestRunDispatch(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		role string // expected constructor: "host" or "agent"
	}{
		{
			name: "default role host",
			env: map[string]string{
				"VARVE_CONTROLLER_URL": "https://ctrl.example.org",
				"VARVE_TOKEN":          "tok",
				"VARVE_WORKER_IMAGE":   "archlinux/archlinux:multilib-devel",
			},
			role: "host",
		},
		{
			name: "explicit host",
			env: map[string]string{
				"VARVE_CONTROLLER_URL": "https://ctrl.example.org",
				"VARVE_ROLE":           "host",
				"VARVE_TOKEN":          "tok",
				"VARVE_WORKER_IMAGE":   "img",
			},
			role: "host",
		},
		{
			name: "agent one-shot",
			env: map[string]string{
				"VARVE_CONTROLLER_URL": "https://ctrl.example.org",
				"VARVE_ROLE":           "agent",
				"VARVE_ONE_SHOT":       "1",
				"VARVE_TASK_ID":        "42",
				"VARVE_TASK_TOKEN":     "claim",
			},
			role: "agent",
		},
		{
			name: "agent pool",
			env: map[string]string{
				"VARVE_CONTROLLER_URL": "https://ctrl.example.org",
				"VARVE_ROLE":           "agent",
				"VARVE_TOKEN":          "pool-tok",
			},
			role: "agent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hostRec, agentRec := stubConstructors(t)
			clearWorkerEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			if err := run(nil); err != nil {
				t.Fatalf("run() = %v, want nil", err)
			}

			rec, other := agentRec, hostRec
			if tt.role == "host" {
				rec, other = hostRec, agentRec
			}
			if !rec.ran {
				t.Fatalf("%s runner constructor not called", tt.role)
			}
			if other.ran {
				t.Errorf("unexpected %s runner constructor call", tt.role)
			}
			if rec.cfg == nil {
				t.Fatal("runner constructor received nil cfg")
			}
			if got := rec.cfg.ControllerURL; got != "https://ctrl.example.org" {
				t.Errorf("cfg.ControllerURL = %q", got)
			}
			if baseURL, token := clientFields(t, rec.client); baseURL != "https://ctrl.example.org" {
				t.Errorf("client baseURL = %q, want the controller URL", baseURL)
			} else if token != rec.cfg.Token {
				t.Errorf("client token = %q, want cfg.Token %q", token, rec.cfg.Token)
			}
			if tt.role == "agent" && tt.env["VARVE_ONE_SHOT"] == "1" && !rec.cfg.OneShot {
				t.Error("cfg.OneShot = false, want true for one-shot agent")
			}
		})
	}
}

// TestRunValidationErrors covers required-field validation: a missing
// controller URL or a missing required field fails startup with an
// error, before any runner is constructed.
func TestRunValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string // substring that must appear in the error
	}{
		{
			name: "missing ControllerURL",
			env: map[string]string{
				"VARVE_TOKEN":        "tok",
				"VARVE_WORKER_IMAGE": "img",
			},
			want: "VARVE_CONTROLLER_URL",
		},
		{
			name: "host missing Token",
			env: map[string]string{
				"VARVE_CONTROLLER_URL": "https://c",
				"VARVE_WORKER_IMAGE":   "img",
			},
			want: "VARVE_TOKEN",
		},
		{
			name: "host missing Image",
			env: map[string]string{
				"VARVE_CONTROLLER_URL": "https://c",
				"VARVE_TOKEN":          "tok",
			},
			want: "VARVE_WORKER_IMAGE",
		},
		{
			name: "one-shot missing TaskID",
			env: map[string]string{
				"VARVE_CONTROLLER_URL": "https://c",
				"VARVE_ROLE":           "agent",
				"VARVE_ONE_SHOT":       "1",
				"VARVE_TASK_TOKEN":     "claim",
			},
			want: "VARVE_TASK_ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hostRec, agentRec := stubConstructors(t)
			clearWorkerEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			err := run(nil)
			if err == nil {
				t.Fatal("run() = nil, want validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
			if hostRec.ran || agentRec.ran {
				t.Error("runner constructor called despite configuration error")
			}
		})
	}
}

// TestRunOneShotWithoutToken asserts that a one-shot agent starts
// without VARVE_TOKEN, carrying only the task credentials, and that the
// shared token never reaches the client.
func TestRunOneShotWithoutToken(t *testing.T) {
	hostRec, agentRec := stubConstructors(t)
	clearWorkerEnv(t)
	t.Setenv("VARVE_CONTROLLER_URL", "https://c")
	t.Setenv("VARVE_ROLE", "agent")
	t.Setenv("VARVE_ONE_SHOT", "1")
	t.Setenv("VARVE_TASK_ID", "42")
	t.Setenv("VARVE_TASK_TOKEN", "claim")

	if err := run(nil); err != nil {
		t.Fatalf("run() = %v, want nil without VARVE_TOKEN", err)
	}
	if hostRec.ran {
		t.Error("host runner called for one-shot agent")
	}
	if !agentRec.ran {
		t.Fatal("agent runner not called for one-shot agent")
	}
	if agentRec.cfg.Token != "" {
		t.Errorf("cfg.Token = %q, want empty for one-shot", agentRec.cfg.Token)
	}
	if _, token := clientFields(t, agentRec.client); token != "" {
		t.Errorf("client token = %q, want empty for one-shot", token)
	}
	if agentRec.cfg.TaskID != "42" || agentRec.cfg.TaskToken != "claim" {
		t.Errorf("cfg.TaskID/TaskToken = %q/%q", agentRec.cfg.TaskID, agentRec.cfg.TaskToken)
	}
}

// TestRunDotenv covers .env loading: a .env file in the working
// directory supplies the configuration when the variables are not
// exported.
func TestRunDotenv(t *testing.T) {
	_, agentRec := stubConstructors(t)
	clearWorkerEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(`VARVE_CONTROLLER_URL="https://dotenv.example.org"
VARVE_ROLE=agent
VARVE_ONE_SHOT=1
VARVE_TASK_ID=7
VARVE_TASK_TOKEN=claim
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := run(nil); err != nil {
		t.Fatalf("run() = %v, want nil", err)
	}
	if !agentRec.ran {
		t.Fatal("agent runner not called")
	}
	cfg := agentRec.cfg
	if cfg.ControllerURL != "https://dotenv.example.org" {
		t.Errorf("cfg.ControllerURL = %q, want value from .env", cfg.ControllerURL)
	}
	if !cfg.OneShot {
		t.Error("cfg.OneShot = false, want true from .env")
	}
	if cfg.TaskID != "7" || cfg.TaskToken != "claim" {
		t.Errorf("cfg.TaskID/TaskToken = %q/%q, want values from .env", cfg.TaskID, cfg.TaskToken)
	}
	if baseURL, _ := clientFields(t, agentRec.client); baseURL != "https://dotenv.example.org" {
		t.Errorf("client baseURL = %q, want value from .env", baseURL)
	}
}

// TestRunErrorPropagation verifies that startup failures of the host
// runner and runtime errors of the runners surface through run.
func TestRunErrorPropagation(t *testing.T) {
	t.Run("host constructor error", func(t *testing.T) {
		hostRec, agentRec := stubConstructors(t)
		clearWorkerEnv(t)
		t.Setenv("VARVE_CONTROLLER_URL", "https://c")
		t.Setenv("VARVE_TOKEN", "tok")
		t.Setenv("VARVE_WORKER_IMAGE", "img")

		// Fail the injected constructor like host.NewRunner does when
		// no container runtime is available.
		prev := newHostRunner
		newHostRunner = func(cfg *config.WorkerConfig, client *api.Client) (runner, error) {
			hostRec.ran = true
			return nil, errors.New("host: no container runtime found")
		}
		t.Cleanup(func() { newHostRunner = prev })

		err := run(nil)
		if err == nil || !strings.Contains(err.Error(), "container runtime") {
			t.Fatalf("run() = %v, want the constructor error", err)
		}
		if agentRec.ran {
			t.Error("agent runner called after host startup failure")
		}
	})

	t.Run("runner run error", func(t *testing.T) {
		hostRec, agentRec := stubConstructors(t)
		clearWorkerEnv(t)
		t.Setenv("VARVE_CONTROLLER_URL", "https://c")
		t.Setenv("VARVE_ROLE", "agent")
		t.Setenv("VARVE_TOKEN", "tok")

		prev := newAgentRunner
		newAgentRunner = func(cfg *config.WorkerConfig, client *api.Client) runner {
			agentRec.ran = true
			return &fakeRunner{err: errors.New("agent: fatal")}
		}
		t.Cleanup(func() { newAgentRunner = prev })

		err := run(nil)
		if err == nil || !strings.Contains(err.Error(), "fatal") {
			t.Fatalf("run() = %v, want the runner error", err)
		}
		if hostRec.ran {
			t.Error("host runner called for agent role")
		}
	})
}
