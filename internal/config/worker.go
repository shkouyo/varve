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
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// LoadWorker resolves the worker configuration from the process environment,
// the optional CWD .env file, and built-in defaults. The precedence is:
// exported environment variables > .env > defaults; .env never overrides an
// already-exported variable.
//
// It is called once at startup and is safe for concurrent use; the returned
// configuration must be treated as read-only.
func LoadWorker() (*WorkerConfig, error) {
	file := loadDotenvFile(".env")
	// get resolves key with exported-env-first precedence. A set-but-empty
	// environment variable is treated as unset so that defaults can apply.
	get := func(key string) string {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			return v
		}
		return file[key]
	}

	cfg := &WorkerConfig{
		ControllerURL:    get("VARVE_CONTROLLER_URL"),
		Token:            get("VARVE_TOKEN"),
		Role:             get("VARVE_ROLE"),
		WorkerName:       get("VARVE_WORKER_NAME"),
		WorkerArch:       get("VARVE_WORKER_ARCH"),
		Image:            get("VARVE_WORKER_IMAGE"),
		ContainerRuntime: get("VARVE_CONTAINER_RUNTIME"),
		TaskID:           get("VARVE_TASK_ID"),
		TaskToken:        get("VARVE_TASK_TOKEN"),
		DataDir:          get("VARVE_DATA_DIR"),
	}

	// Defaults. An empty ContainerRuntime means "auto-detect" and is
	// performed by the host module.
	if cfg.Role == "" {
		cfg.Role = "host"
	}
	if cfg.WorkerArch == "" {
		cfg.WorkerArch = "x86_64"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "/var/lib/varve"
	}

	if v := get("VARVE_WORKER_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("config: VARVE_WORKER_CONCURRENCY: invalid value %q: %w", v, err)
		}
		cfg.Concurrency = n
	} else {
		cfg.Concurrency = 1
	}
	if v := get("VARVE_PULL_IMAGE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("config: VARVE_PULL_IMAGE: invalid value %q: %w", v, err)
		}
		cfg.PullImage = b
	} else {
		cfg.PullImage = true
	}
	if v := get("VARVE_ONE_SHOT"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("config: VARVE_ONE_SHOT: invalid value %q: %w", v, err)
		}
		cfg.OneShot = b
	}
	if v := get("VARVE_POOL_IDLE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("config: VARVE_POOL_IDLE_TIMEOUT: invalid value %q: %w", v, err)
		}
		cfg.PoolIdleTimeout = d
	} else {
		cfg.PoolIdleTimeout = 10 * time.Minute
	}

	if err := validateWorker(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validateWorker checks a resolved WorkerConfig against the validation
// rules. ControllerURL is always required; a host worker requires Token and
// Image; a one-shot agent requires TaskID and TaskToken (it has no shared
// Token), while a pool agent requires the shared Token.
func validateWorker(c *WorkerConfig) error {
	if c.ControllerURL == "" {
		return errors.New("config: VARVE_CONTROLLER_URL is required")
	}
	switch c.Role {
	case "host", "agent":
	default:
		return fmt.Errorf("config: VARVE_ROLE: must be one of %q, %q, got %q", "host", "agent", c.Role)
	}
	if c.Role == "host" {
		if c.Token == "" {
			return errors.New("config: VARVE_TOKEN is required for role \"host\"")
		}
		if c.Image == "" {
			return errors.New("config: VARVE_WORKER_IMAGE is required for role \"host\"")
		}
		if c.Concurrency < 1 {
			return errors.New("config: VARVE_WORKER_CONCURRENCY must be at least 1 for role \"host\"")
		}
	}
	if c.Role == "agent" {
		if c.OneShot {
			if c.TaskID == "" {
				return errors.New("config: VARVE_TASK_ID is required for one-shot agent")
			}
			if c.TaskToken == "" {
				return errors.New("config: VARVE_TASK_TOKEN is required for one-shot agent")
			}
		} else if c.Token == "" {
			return errors.New("config: VARVE_TOKEN is required for pool agent")
		}
	}
	return nil
}
