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

// Package config parses and validates the controller TOML configuration and
// the worker environment-variable configuration (including the optional CWD
// .env file).
//
// Configurations are loaded once at process startup (LoadController /
// LoadWorker). The returned structures are treated as read-only and are
// shared concurrently by the other modules; callers must not modify them at
// runtime.
//
// Tests in this package swap package-level stubs (warnW) and must not run
// t.Parallel.
package config

import "time"

// ControllerConfig is the fully resolved controller configuration. Field
// defaults are noted in parentheses.
type ControllerConfig struct {
	Server   ServerConfig
	API      APIConfig
	Database DatabaseConfig
	Storage  StorageConfig
	Repo     RepoConfig
	GPG      GPGConfig
	Source   SourceConfig
	Worker   WorkerLimits
	Mail     MailConfig
	Web      WebConfig
	Logs     LogsConfig
	AUR      AURConfig
}

// ServerConfig configures the controller listen addresses and the public Web
// UI URL used in mail log links.
type ServerConfig struct {
	APIPort string // worker API listen address, ":31759"
	WebPort string // Web UI listen address, ":31760"
	WebURL  string // public Web UI URL (mail log links only)
}

// APIConfig configures the shared Bearer token used by workers.
type APIConfig struct {
	Token     string // shared Bearer token; overridable via VARVE_API_TOKEN
	TokenFile string // optional path to read the token from (container secrets)
}

// DatabaseConfig configures the SQLite database path.
type DatabaseConfig struct {
	Path string // "/data/varve.db"
}

// StorageConfig selects the artifact storage backend.
type StorageConfig struct {
	Backend string      // "local" | "s3"
	Local   LocalConfig // local backend settings
	S3      S3Config    // S3 backend settings
}

// LocalConfig configures the local repository root directory.
type LocalConfig struct {
	Root       string // "/data/repo"
	StagingDir string // staging upload dir; "" = <root>/staging, absolute = used as-is, relative = joined onto root (resolved at load)
}

// S3Config configures an S3-compatible artifact backend.
type S3Config struct {
	Endpoint      string // S3 endpoint URL
	Bucket        string // bucket name
	Region        string // S3 region
	AccessKey     string // access key; overridable via VARVE_S3_ACCESS_KEY
	SecretKey     string // secret key; overridable via VARVE_S3_SECRET_KEY
	PathStyle     bool   // use path-style addressing (default true)
	StagingPrefix string // object-key prefix of the staging upload area, "staging"
}

// RepoConfig configures the generated pacman repository.
type RepoConfig struct {
	Name         string // repository name, "varve"
	WorkDir      string // repo-add working directory (S3 backend only), "/data/work"
	Sign         string // "off" | "packages" | "packages+db"
	KeepVersions int    // versions kept per package (only 1 is supported)
}

// GPGConfig configures the repository signing key (controller side).
type GPGConfig struct {
	KeyID      string // signing key ID
	KeyFile    string // optional armored private key file
	Passphrase string // private key passphrase (TOML only, no env override)
}

// SourceConfig configures the upstream PKGBUILD repository polling.
type SourceConfig struct {
	URL             string        // source repository URL (required)
	FetchKey        string        // optional SSH key path or token; overridable via VARVE_SOURCE_FETCH_KEY
	PollInterval    time.Duration // polling interval, "5m"
	ExcludeBranches []string      // branches excluded from polling, ["main"]
}

// WorkerLimits configures worker-side timeouts and resource limits plus
// the task retry policy and the actions-based autostart policy
// ([worker.actions]).
type WorkerLimits struct {
	HeartbeatTimeout      time.Duration // seconds without heartbeat before a worker is offline, "90s"
	StallTimeout          time.Duration // task without progress considered stalled, "10m"
	BuildTimeout          time.Duration // per-task build timeout, "30m"
	CPULimit              int           // container CPU limit, 0 = unlimited
	MemoryLimit           string        // container memory limit (e.g. "8GiB"), "" = unlimited
	RetryMax              int           // failed tasks are retried up to this many times, 3
	FailedRebuildCooldown time.Duration // minimum wait before a failed package is rebuilt again, "1h"
	Packager              string        // build identity injected as PACKAGER into the build environment, "Name <email>"; empty = not injected
	Actions               WorkerActions // autostart the runner workflow when work waits
}

// WorkerActions configures the per-task start of build runners through
// the GitHub Actions workflow_dispatch API: every queued task is
// dispatched as its own workflow run (a one-shot runner), up to
// MaxConcurrency concurrent runs. The token is a password class field
// (TOML only, no environment override).
type WorkerActions struct {
	Enabled        bool          // dispatch one run per queued task, false
	Token          string        // GitHub PAT with actions:write permission on the runner repo
	Repo           string        // owner/repo slug of the runner repository (required when enabled)
	Workflow       string        // workflow file name, "worker-actions.yml"
	Ref            string        // git ref to dispatch on, "main"
	MaxConcurrency int           // maximum concurrent runner runs, 3
	ClaimTimeout   time.Duration // a dispatched run must claim its task within this window, "5m"
}

// MailConfig configures SMTP failure notifications.
type MailConfig struct {
	Enabled  bool   // send failure notifications, false
	Host     string // SMTP server address
	Username string // SMTP username
	Password string // SMTP password (TOML only, no env override)
	From     string // sender address, "varve@example.org"
	Port     int    // SMTP port, 587
	TLS      string // "none" | "starttls" | "implicit", "starttls"
}

// WebConfig configures the Web UI and artifact downloads. Admins is the
// list of Basic Auth accounts guarding the admin area; RecentBuilds
// limits how many recent builds the dashboard shows.
type WebConfig struct {
	DownloadEnabled bool
	DownloadBaseURI string // direct-link URI prefix (required when download_enabled)
	RecentBuilds    int    // dashboard recent-build count, 20
	Admins          []WebAdmin
}

// WebAdmin is one Basic Auth account of the admin area.
type WebAdmin struct {
	User     string
	Password string // TOML only, no env override
}

// LogsConfig configures build log retention.
type LogsConfig struct {
	Dir       string        // log directory, "/data/logs"
	Retention time.Duration // retention for successful build logs, "90d"
	MaxBuilds int           // maximum successful logs kept, 1000
}

// AURConfig configures the optional AUR publishing feature: after a
// successful build of a branch whose dotfile opts in ([aur].submit), the
// built branch commit is pushed to the AUR package repository. The feature
// is enabled only when KeyFile is set; the key file must be mounted under
// /data by the deployment.
type AURConfig struct {
	Server  string // AUR SSH server, "aur.archlinux.org"
	KeyFile string // SSH private key path; empty disables AUR publishing
	User    string // SSH user, "aur"
}

// WorkerConfig is the fully resolved worker configuration. It is built from
// the process environment, the optional CWD .env file, and built-in defaults
// (env > .env > default).
type WorkerConfig struct {
	ControllerURL    string        // controller API base URL (required)
	Token            string        // shared Bearer token (required for host/pool; empty for one-shot)
	Role             string        // "host" | "agent", default "host"
	WorkerName       string        // node name; empty = auto-generated (host module)
	WorkerArch       string        // architecture, default "x86_64"
	Concurrency      int           // host container concurrency, default 1
	Image            string        // build container image reference (required for host)
	ContainerRuntime string        // "docker" | "podman" | "" (empty = auto-detect)
	PullImage        bool          // pull the image before each run, default true
	OneShot          bool          // agent: handle a single task then exit (VARVE_ONE_SHOT=1)
	TaskID           string        // one-shot task ID (injected by the host)
	TaskToken        string        // one-shot claim token (injected by the host)
	PoolIdleTimeout  time.Duration // pool idle exit timeout, default 10m
	DataDir          string        // data directory, default "/var/lib/varve"
}
