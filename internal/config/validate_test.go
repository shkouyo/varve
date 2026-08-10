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
	"strings"
	"testing"
	"time"
)

// validController returns a ControllerConfig that passes every validation
// rule; tests mutate a copy of it.
func validController() *ControllerConfig {
	return &ControllerConfig{
		Server:   ServerConfig{APIPort: ":31759", WebPort: ":31760"},
		API:      APIConfig{Token: "t"},
		Database: DatabaseConfig{Path: "/data/varve.db"},
		Storage: StorageConfig{
			Backend: "local",
			Local:   LocalConfig{Root: "/data/repo"},
			S3:      S3Config{PathStyle: true},
		},
		Repo: RepoConfig{Name: "varve", WorkDir: "/data/work", Sign: "off", KeepVersions: 1},
		Source: SourceConfig{
			URL:             "git@x:y.git",
			PollInterval:    5 * time.Minute,
			ExcludeBranches: []string{"main"},
		},
		Worker: WorkerLimits{
			HeartbeatTimeout:      90 * time.Second,
			StallTimeout:          10 * time.Minute,
			BuildTimeout:          30 * time.Minute,
			RetryMax:              3,
			FailedRebuildCooldown: time.Hour,
			Actions: WorkerActions{
				Repo:           "owner/varve-runner",
				Workflow:       "worker-actions.yml",
				Ref:            "main",
				MaxConcurrency: 3,
				ClaimTimeout:   5 * time.Minute,
			},
		},
		Mail: MailConfig{Port: 587, From: "varve@example.org", TLS: "starttls"},
		Web:  WebConfig{DownloadEnabled: true, DownloadBaseURI: "https://dl.example.org", RecentBuilds: 20, Admins: []WebAdmin{{User: "admin", Password: "p"}}},
		Logs: LogsConfig{Dir: "/data/logs", Retention: 90 * 24 * time.Hour, MaxBuilds: 1000},
	}
}

func TestValidateOK(t *testing.T) {
	if err := validate(validController()); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name string
		// mutate applies the invalid setting to a fresh valid config.
		mutate func(*ControllerConfig)
		want   string // field name that must appear in the error
	}{
		{
			name:   "invalid storage.backend",
			mutate: func(c *ControllerConfig) { c.Storage.Backend = "ftp" },
			want:   "storage.backend",
		},
		{
			name:   "invalid repo.sign",
			mutate: func(c *ControllerConfig) { c.Repo.Sign = "sig" },
			want:   "repo.sign",
		},
		{
			name:   "invalid mail.tls",
			mutate: func(c *ControllerConfig) { c.Mail.TLS = "ssl" },
			want:   "mail.tls",
		},
		{
			name:   "download enabled without base uri",
			mutate: func(c *ControllerConfig) { c.Web.DownloadBaseURI = "" },
			want:   "web.download_base_uri",
		},
		{
			name:   "keep_versions not 1",
			mutate: func(c *ControllerConfig) { c.Repo.KeepVersions = 2 },
			want:   "repo.keep_versions",
		},
		{
			name:   "invalid worker.packager",
			mutate: func(c *ControllerConfig) { c.Worker.Packager = "no angle brackets" },
			want:   "worker.packager",
		},
		{
			name:   "empty worker.packager name",
			mutate: func(c *ControllerConfig) { c.Worker.Packager = " <you@example.org>" },
			want:   "worker.packager",
		},
		{
			name:   "empty worker.packager email",
			mutate: func(c *ControllerConfig) { c.Worker.Packager = "Your Name <>" },
			want:   "worker.packager",
		},
		{
			name:   "worker.packager trailing junk",
			mutate: func(c *ControllerConfig) { c.Worker.Packager = "Your Name <you@example.org> extra" },
			want:   "worker.packager",
		},
		{
			name:   "no web.admins",
			mutate: func(c *ControllerConfig) { c.Web.Admins = nil },
			want:   "web.admins",
		},
		{
			name:   "empty admin user",
			mutate: func(c *ControllerConfig) { c.Web.Admins = []WebAdmin{{User: "", Password: "p"}} },
			want:   "web.admins",
		},
		{
			name: "duplicate admin user",
			mutate: func(c *ControllerConfig) {
				c.Web.Admins = []WebAdmin{{User: "admin", Password: "p"}, {User: "admin", Password: "q"}}
			},
			want: "web.admins",
		},
		{
			name:   "empty admin password",
			mutate: func(c *ControllerConfig) { c.Web.Admins = []WebAdmin{{User: "admin", Password: ""}} },
			want:   "web.admins",
		},
		{
			name:   "zero web.recent_builds",
			mutate: func(c *ControllerConfig) { c.Web.RecentBuilds = 0 },
			want:   "web.recent_builds",
		},
		{
			name:   "negative worker.retry_max",
			mutate: func(c *ControllerConfig) { c.Worker.RetryMax = -1 },
			want:   "worker.retry_max",
		},
		{
			name:   "zero failed_rebuild_cooldown",
			mutate: func(c *ControllerConfig) { c.Worker.FailedRebuildCooldown = 0 },
			want:   "worker.failed_rebuild_cooldown",
		},
		{
			name: "sign packages without gpg key",
			mutate: func(c *ControllerConfig) {
				c.Repo.Sign = "packages"
				c.GPG.KeyID = ""
				c.GPG.KeyFile = ""
			},
			want: "gpg.key_id",
		},
		{
			name:   "empty source.url",
			mutate: func(c *ControllerConfig) { c.Source.URL = "" },
			want:   "source.url",
		},
		{
			name:   "empty logs.dir",
			mutate: func(c *ControllerConfig) { c.Logs.Dir = "" },
			want:   "logs.dir",
		},
		{
			name:   "zero poll_interval",
			mutate: func(c *ControllerConfig) { c.Source.PollInterval = 0 },
			want:   "source.poll_interval",
		},
		{
			name:   "zero heartbeat_timeout",
			mutate: func(c *ControllerConfig) { c.Worker.HeartbeatTimeout = 0 },
			want:   "worker.heartbeat_timeout",
		},
		{
			name:   "zero stall_timeout",
			mutate: func(c *ControllerConfig) { c.Worker.StallTimeout = 0 },
			want:   "worker.stall_timeout",
		},
		{
			name:   "zero build_timeout",
			mutate: func(c *ControllerConfig) { c.Worker.BuildTimeout = 0 },
			want:   "worker.build_timeout",
		},
		{
			name:   "zero logs.retention",
			mutate: func(c *ControllerConfig) { c.Logs.Retention = 0 },
			want:   "logs.retention",
		},
		{
			name:   "negative poll_interval",
			mutate: func(c *ControllerConfig) { c.Source.PollInterval = -time.Minute },
			want:   "source.poll_interval",
		},
		{
			name:   "actions enabled without token",
			mutate: func(c *ControllerConfig) { c.Worker.Actions.Enabled = true },
			want:   "worker.actions.token",
		},
		{
			name: "actions enabled without repo",
			mutate: func(c *ControllerConfig) {
				c.Worker.Actions.Enabled = true
				c.Worker.Actions.Token = "t"
				c.Worker.Actions.Repo = ""
			},
			want: "worker.actions.repo",
		},
		{
			name:   "zero actions max concurrency",
			mutate: func(c *ControllerConfig) { c.Worker.Actions.MaxConcurrency = 0 },
			want:   "worker.actions.max_concurrency",
		},
		{
			name:   "negative actions max concurrency",
			mutate: func(c *ControllerConfig) { c.Worker.Actions.MaxConcurrency = -1 },
			want:   "worker.actions.max_concurrency",
		},
		{
			name:   "zero actions claim timeout",
			mutate: func(c *ControllerConfig) { c.Worker.Actions.ClaimTimeout = 0 },
			want:   "worker.actions.claim_timeout",
		},
		{
			name:   "negative actions claim timeout",
			mutate: func(c *ControllerConfig) { c.Worker.Actions.ClaimTimeout = -time.Minute },
			want:   "worker.actions.claim_timeout",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validController()
			tt.mutate(cfg)
			err := validate(cfg)
			if err == nil {
				t.Fatalf("validate() = nil, want error for %q", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain field %q", err, tt.want)
			}
		})
	}
}

// TestValidateStagingDirResolution asserts the local staging directory
// normalization: the default keeps <root>/staging, an absolute value is
// used as-is, a relative value is joined onto the root, and redundant or
// parent segments are cleaned away so the staging tree cannot traverse.
func TestValidateStagingDirResolution(t *testing.T) {
	cases := []struct {
		name, root, dir, want string
	}{
		{"default", "/data/repo", "", "/data/repo/staging"},
		{"absolute used as-is", "/data/repo", "/srv/varve/staging", "/srv/varve/staging"},
		{"absolute cleaned", "/data/repo", "/srv/../srv/varve/staging", "/srv/varve/staging"},
		{"relative joined onto root", "/data/repo", "tmp/staging", "/data/repo/tmp/staging"},
		{"relative traversal cleaned", "/data/repo", "a/../b", "/data/repo/b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validController()
			cfg.Storage.Local.Root = tc.root
			cfg.Storage.Local.StagingDir = tc.dir
			if err := validate(cfg); err != nil {
				t.Fatalf("validate(): %v", err)
			}
			if got := cfg.Storage.Local.StagingDir; got != tc.want {
				t.Errorf("StagingDir = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestValidateStagingPrefix asserts the S3 staging prefix contract: an
// empty value keeps the default "staging", valid prefixes (including
// multi-segment keys) pass, and unsafe prefixes are rejected with the
// field name.
func TestValidateStagingPrefix(t *testing.T) {
	valid := []string{"", "staging", "uploads", "uploads/tmp", "tmp-2026/staging.area"}
	for _, prefix := range valid {
		cfg := validController()
		cfg.Storage.S3.StagingPrefix = prefix
		if err := validate(cfg); err != nil {
			t.Errorf("validate() with prefix %q: %v", prefix, err)
			continue
		}
		want := prefix
		if want == "" {
			want = "staging"
		}
		if cfg.Storage.S3.StagingPrefix != want {
			t.Errorf("StagingPrefix = %q, want %q", cfg.Storage.S3.StagingPrefix, want)
		}
	}

	invalid := []string{"/uploads", "uploads/", "a//b", "a/./b", "a/../b", "up loads", "uploads@2026", ".."}
	for _, prefix := range invalid {
		cfg := validController()
		cfg.Storage.S3.StagingPrefix = prefix
		err := validate(cfg)
		if err == nil {
			t.Errorf("validate() with prefix %q: want error", prefix)
			continue
		}
		if !strings.Contains(err.Error(), "storage.s3.staging_prefix") {
			t.Errorf("error %q does not mention storage.s3.staging_prefix", err)
		}
	}
}

// TestValidateRepoPrefix asserts the S3 repository prefix contract: an
// empty value keeps the bucket root (valid), valid prefixes (including
// multi-segment keys) pass, and unsafe prefixes are rejected with the
// field name.
func TestValidateRepoPrefix(t *testing.T) {
	valid := []string{"", "repo", "artifacts", "artifacts/repo", "tmp-2026/repo.area"}
	for _, prefix := range valid {
		cfg := validController()
		cfg.Storage.S3.RepoPrefix = prefix
		if err := validate(cfg); err != nil {
			t.Errorf("validate() with repo prefix %q: %v", prefix, err)
			continue
		}
		if cfg.Storage.S3.RepoPrefix != prefix {
			t.Errorf("RepoPrefix = %q, want %q", cfg.Storage.S3.RepoPrefix, prefix)
		}
	}

	invalid := []string{"/repo", "repo/", "a//b", "a/./b", "a/../b", "rep o", "repo@2026", ".."}
	for _, prefix := range invalid {
		cfg := validController()
		cfg.Storage.S3.RepoPrefix = prefix
		err := validate(cfg)
		if err == nil {
			t.Errorf("validate() with repo prefix %q: want error", prefix)
			continue
		}
		if !strings.Contains(err.Error(), "storage.s3.repo_prefix") {
			t.Errorf("error %q does not mention storage.s3.repo_prefix", err)
		}
	}
}

func TestValidatePackagerOK(t *testing.T) {
	for _, packager := range []string{
		"", // unset: no PACKAGER injected
		"Your Name <you@example.org>",
		"Jane Packager <jane@example.org>",
	} {
		cfg := validController()
		cfg.Worker.Packager = packager
		if err := validate(cfg); err != nil {
			t.Errorf("validate() with packager %q: %v", packager, err)
		}
	}
}

func TestValidateSignWithKeyFile(t *testing.T) {
	// repo.sign != off passes when either key_id or key_file is set.
	for _, tc := range []struct {
		keyID, keyFile string
	}{
		{"K", ""},
		{"", "/path/key.asc"},
		{"K", "/path/key.asc"},
	} {
		cfg := validController()
		cfg.Repo.Sign = "packages+db"
		cfg.GPG.KeyID = tc.keyID
		cfg.GPG.KeyFile = tc.keyFile
		if err := validate(cfg); err != nil {
			t.Errorf("validate() with keyID=%q keyFile=%q: %v", tc.keyID, tc.keyFile, err)
		}
	}
}

// TestValidateActionsEnabledOK covers the happy path of the actions
// section: enabled with a token, a repo and a workflow passes, also with
// an explicit ref.
func TestValidateActionsEnabledOK(t *testing.T) {
	cfg := validController()
	cfg.Worker.Actions = WorkerActions{
		Enabled:        true,
		Token:          "t",
		Repo:           "owner/varve-runner",
		Workflow:       "worker-actions.yml",
		Ref:            "main",
		MaxConcurrency: 3,
		ClaimTimeout:   5 * time.Minute,
	}
	if err := validate(cfg); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
}
