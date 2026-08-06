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

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearWorkerEnv resets every VARVE_* variable the worker reads. Empty
// values are treated as unset by LoadWorker, so this also covers variables
// that may be inherited from the test environment.
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

// inTempDir chdirs the test into a fresh temp directory (for .env tests).
func inTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

func TestLoadWorkerDefaults(t *testing.T) {
	clearWorkerEnv(t)
	// The minimal environment that passes host validation: ControllerURL,
	// Token and Image are required; every other field must take its default.
	t.Setenv("VARVE_CONTROLLER_URL", "https://ctrl.example.org")
	t.Setenv("VARVE_TOKEN", "tok")
	t.Setenv("VARVE_WORKER_IMAGE", "archlinux/archlinux:multilib-devel")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatalf("LoadWorker: %v", err)
	}
	if cfg.ControllerURL != "https://ctrl.example.org" {
		t.Errorf("ControllerURL = %q", cfg.ControllerURL)
	}
	if cfg.Role != "host" {
		t.Errorf("Role = %q, want host", cfg.Role)
	}
	if cfg.WorkerArch != "x86_64" {
		t.Errorf("WorkerArch = %q, want x86_64", cfg.WorkerArch)
	}
	if cfg.Concurrency != 1 {
		t.Errorf("Concurrency = %d, want 1", cfg.Concurrency)
	}
	if cfg.PullImage != true {
		t.Errorf("PullImage = %v, want true", cfg.PullImage)
	}
	if cfg.PoolIdleTimeout != 10*time.Minute {
		t.Errorf("PoolIdleTimeout = %v, want 10m", cfg.PoolIdleTimeout)
	}
	if cfg.DataDir != "/var/lib/varve" {
		t.Errorf("DataDir = %q, want /var/lib/varve", cfg.DataDir)
	}
	if cfg.ContainerRuntime != "" {
		t.Errorf("ContainerRuntime = %q, want empty (auto-detect)", cfg.ContainerRuntime)
	}
	if cfg.WorkerName != "" {
		t.Errorf("WorkerName = %q, want empty (auto-generate)", cfg.WorkerName)
	}
}

func TestLoadWorkerFullHost(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("VARVE_CONTROLLER_URL", "https://ctrl.example.org")
	t.Setenv("VARVE_TOKEN", "tok")
	t.Setenv("VARVE_WORKER_IMAGE", "archlinux/archlinux:multilib-devel")
	t.Setenv("VARVE_WORKER_NAME", "node-1")
	t.Setenv("VARVE_WORKER_ARCH", "aarch64")
	t.Setenv("VARVE_WORKER_CONCURRENCY", "4")
	t.Setenv("VARVE_CONTAINER_RUNTIME", "podman")
	t.Setenv("VARVE_PULL_IMAGE", "false")
	t.Setenv("VARVE_POOL_IDLE_TIMEOUT", "5m")
	t.Setenv("VARVE_DATA_DIR", "/srv/varve")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatalf("LoadWorker: %v", err)
	}
	if cfg.Token != "tok" || cfg.WorkerName != "node-1" || cfg.WorkerArch != "aarch64" ||
		cfg.Concurrency != 4 || cfg.ContainerRuntime != "podman" || cfg.PullImage ||
		cfg.PoolIdleTimeout != 5*time.Minute || cfg.DataDir != "/srv/varve" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadWorkerDotenvPrecedence(t *testing.T) {
	clearWorkerEnv(t)
	dir := inTempDir(t)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(`
# worker env
VARVE_CONTROLLER_URL="https://dotenv.example.org"
VARVE_ROLE=agent
VARVE_TOKEN="dotenv-token"
VARVE_WORKER_IMAGE="archlinux/archlinux:multilib-devel"
VARVE_WORKER_CONCURRENCY=2
`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Exported env wins over .env.
	t.Setenv("VARVE_CONTROLLER_URL", "https://env.example.org")
	t.Setenv("VARVE_ROLE", "host")
	cfg, err := LoadWorker()
	if err != nil {
		t.Fatalf("LoadWorker: %v", err)
	}
	if cfg.ControllerURL != "https://env.example.org" {
		t.Errorf("ControllerURL = %q, want env value", cfg.ControllerURL)
	}
	if cfg.Role != "host" {
		t.Errorf("Role = %q, want env value", cfg.Role)
	}
	// .env supplies values that are not exported.
	if cfg.Token != "dotenv-token" {
		t.Errorf("Token = %q, want dotenv value", cfg.Token)
	}
	if cfg.Concurrency != 2 {
		t.Errorf("Concurrency = %d, want 2 from .env", cfg.Concurrency)
	}
}

func TestLoadWorkerDotenvOnly(t *testing.T) {
	clearWorkerEnv(t)
	dir := inTempDir(t)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(`VARVE_CONTROLLER_URL="https://dotenv.example.org"
VARVE_ROLE=agent
VARVE_TOKEN=agent-token
VARVE_ONE_SHOT=0
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatalf("LoadWorker: %v", err)
	}
	if cfg.ControllerURL != "https://dotenv.example.org" {
		t.Errorf("ControllerURL = %q", cfg.ControllerURL)
	}
	if cfg.Role != "agent" {
		t.Errorf("Role = %q, want agent", cfg.Role)
	}
	if cfg.Token != "agent-token" {
		t.Errorf("Token = %q", cfg.Token)
	}
	if cfg.OneShot {
		t.Error("OneShot = true, want false (VARVE_ONE_SHOT=0)")
	}
}

func TestLoadWorkerMissingDotenvOK(t *testing.T) {
	// No .env in CWD: LoadWorker must not fail.
	clearWorkerEnv(t)
	inTempDir(t)
	t.Setenv("VARVE_CONTROLLER_URL", "https://ctrl.example.org")
	t.Setenv("VARVE_TOKEN", "tok")
	t.Setenv("VARVE_WORKER_IMAGE", "img")
	if _, err := LoadWorker(); err != nil {
		t.Fatalf("LoadWorker without .env: %v", err)
	}
}

func TestLoadWorkerErrors(t *testing.T) {
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
			name: "pool agent missing Token",
			env: map[string]string{
				"VARVE_CONTROLLER_URL": "https://c",
				"VARVE_ROLE":           "agent",
			},
			want: "VARVE_TOKEN",
		},
		{
			name: "one-shot agent missing TaskID",
			env: map[string]string{
				"VARVE_CONTROLLER_URL": "https://c",
				"VARVE_ROLE":           "agent",
				"VARVE_ONE_SHOT":       "1",
				"VARVE_TASK_TOKEN":     "claim",
			},
			want: "VARVE_TASK_ID",
		},
		{
			name: "one-shot agent missing TaskToken",
			env: map[string]string{
				"VARVE_CONTROLLER_URL": "https://c",
				"VARVE_ROLE":           "agent",
				"VARVE_ONE_SHOT":       "1",
				"VARVE_TASK_ID":        "42",
			},
			want: "VARVE_TASK_TOKEN",
		},
		{
			name: "invalid role",
			env: map[string]string{
				"VARVE_CONTROLLER_URL": "https://c",
				"VARVE_ROLE":           "wat",
			},
			want: "VARVE_ROLE",
		},
		{
			name: "invalid concurrency",
			env: map[string]string{
				"VARVE_CONTROLLER_URL":     "https://c",
				"VARVE_TOKEN":              "tok",
				"VARVE_WORKER_IMAGE":       "img",
				"VARVE_WORKER_CONCURRENCY": "abc",
			},
			want: "VARVE_WORKER_CONCURRENCY",
		},
		{
			name: "invalid pull image",
			env: map[string]string{
				"VARVE_CONTROLLER_URL": "https://c",
				"VARVE_TOKEN":          "tok",
				"VARVE_WORKER_IMAGE":   "img",
				"VARVE_PULL_IMAGE":     "maybe",
			},
			want: "VARVE_PULL_IMAGE",
		},
		{
			name: "invalid pool idle timeout",
			env: map[string]string{
				"VARVE_CONTROLLER_URL":    "https://c",
				"VARVE_TOKEN":             "tok",
				"VARVE_WORKER_IMAGE":      "img",
				"VARVE_POOL_IDLE_TIMEOUT": "soon",
			},
			want: "VARVE_POOL_IDLE_TIMEOUT",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearWorkerEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			_, err := LoadWorker()
			if err == nil {
				t.Fatalf("LoadWorker() = nil, want error for %q", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadWorkerOneShot(t *testing.T) {
	// A one-shot agent does not need VARVE_TOKEN.
	clearWorkerEnv(t)
	t.Setenv("VARVE_CONTROLLER_URL", "https://c")
	t.Setenv("VARVE_ROLE", "agent")
	t.Setenv("VARVE_ONE_SHOT", "1")
	t.Setenv("VARVE_TASK_ID", "42")
	t.Setenv("VARVE_TASK_TOKEN", "claim")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatalf("LoadWorker: %v", err)
	}
	if !cfg.OneShot {
		t.Error("OneShot = false, want true")
	}
	if cfg.Token != "" {
		t.Errorf("Token = %q, want empty for one-shot", cfg.Token)
	}
	if cfg.TaskID != "42" || cfg.TaskToken != "claim" {
		t.Errorf("TaskID/TaskToken = %q/%q", cfg.TaskID, cfg.TaskToken)
	}
}

func TestLoadWorkerRoleDefaultHost(t *testing.T) {
	// VARVE_ROLE absent (only in .env with a typo'd name): defaults to host.
	clearWorkerEnv(t)
	dir := inTempDir(t)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(`VARVE_ROL=agent
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VARVE_CONTROLLER_URL", "https://c")
	t.Setenv("VARVE_TOKEN", "tok")
	t.Setenv("VARVE_WORKER_IMAGE", "img")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatalf("LoadWorker: %v", err)
	}
	if cfg.Role != "host" {
		t.Errorf("Role = %q, want host", cfg.Role)
	}
}

func TestLoadWorkerDataDir(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("VARVE_CONTROLLER_URL", "https://c")
	t.Setenv("VARVE_TOKEN", "tok")
	t.Setenv("VARVE_WORKER_IMAGE", "img")
	t.Setenv("VARVE_DATA_DIR", "/custom/varve")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatalf("LoadWorker: %v", err)
	}
	if cfg.DataDir != "/custom/varve" {
		t.Errorf("DataDir = %q", cfg.DataDir)
	}
}
