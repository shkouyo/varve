// SPDX-License-Identifier: AGPL-3.0-or-later
//
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
			HeartbeatTimeout: 90 * time.Second,
			StallTimeout:     10 * time.Minute,
			BuildTimeout:     30 * time.Minute,
		},
		Mail: MailConfig{Port: 587, From: "varve@example.org", TLS: "starttls"},
		Web:  WebConfig{DownloadEnabled: true, DownloadBaseURI: "https://dl.example.org", AdminUser: "admin", AdminPassword: "p"},
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
			name:   "empty api.token",
			mutate: func(c *ControllerConfig) { c.API.Token = "" },
			want:   "api.token",
		},
		{
			name:   "empty web.admin_password",
			mutate: func(c *ControllerConfig) { c.Web.AdminPassword = "" },
			want:   "web.admin_password",
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
