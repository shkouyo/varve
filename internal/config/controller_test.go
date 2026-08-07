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
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// minimalConfig is a valid minimal controller configuration used by tests.
const minimalConfig = `
[api]
token = "t"

[source]
url = "git@x:y.git"

[web]
download_base_uri = "https://dl.example.org"

[[web.admins]]
user = "admin"
password = "p"

[logs]
dir = "/data/logs"
`

// fullExample is the complete controller configuration example, used for the
// field-by-field assertion test.
const fullExample = `
[server]
api_listen = ":31759"
web_listen = ":31760"
web_url = "https://varve.example.org"

[api]
token = "change-me"
token_file = ""

[database]
path = "/data/varve.db"

[storage]
backend = "local"

[storage.local]
root = "/data/repo"

[storage.s3]
endpoint = "https://s3.example.org"
bucket = "varve-repo"
region = "us-east-1"
access_key = ""
secret_key = ""
path_style = true

[repo]
name = "varve"
work_dir = "/data/work"
sign = "off"
keep_versions = 1

[gpg]
key_id = ""
key_file = ""
passphrase = ""

[source]
url = "git@git.example.org:pkgbuilds.git"
fetch_key = ""
poll_interval = "5m"
exclude_branches = ["main", "wip/*"]

[worker]
heartbeat_timeout = "90s"
stall_timeout = "10m"
build_timeout = "30m"
cpu_limit = 0
memory_limit = 0
retry_max = 3
failed_rebuild_cooldown = "1h"

[worker.actions]
enabled = true
token = "ghp-example"
repo = "owner/varve-runner"
workflow = "worker-actions.yml"
ref = "main"
cooldown = "5m"

[mail]
enabled = false
host = ""
port = 587
username = ""
password = ""
from = "varve@example.org"
tls = "starttls"

[web]
download_enabled = true
download_base_uri = "https://dl.example.org"
recent_builds = 20

[[web.admins]]
user = "admin"
password = "change-me"

[logs]
dir = "/data/logs"
retention = "90d"
max_builds = 1000
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "varve.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadControllerFullExample(t *testing.T) {
	cfg, err := LoadController(writeConfig(t, fullExample))
	if err != nil {
		t.Fatalf("LoadController: %v", err)
	}

	if cfg.Server.APIPort != ":31759" || cfg.Server.WebPort != ":31760" {
		t.Errorf("Server = %q/%q", cfg.Server.APIPort, cfg.Server.WebPort)
	}
	if cfg.Server.WebURL != "https://varve.example.org" {
		t.Errorf("Server.WebURL = %q", cfg.Server.WebURL)
	}
	if cfg.API.Token != "change-me" {
		t.Errorf("API.Token = %q", cfg.API.Token)
	}
	if cfg.API.TokenFile != "" {
		t.Errorf("API.TokenFile = %q", cfg.API.TokenFile)
	}
	if cfg.Database.Path != "/data/varve.db" {
		t.Errorf("Database.Path = %q", cfg.Database.Path)
	}
	if cfg.Storage.Backend != "local" {
		t.Errorf("Storage.Backend = %q", cfg.Storage.Backend)
	}
	if cfg.Storage.Local.Root != "/data/repo" {
		t.Errorf("Storage.Local.Root = %q", cfg.Storage.Local.Root)
	}
	if cfg.Storage.S3.Endpoint != "https://s3.example.org" ||
		cfg.Storage.S3.Bucket != "varve-repo" ||
		cfg.Storage.S3.Region != "us-east-1" ||
		cfg.Storage.S3.AccessKey != "" ||
		cfg.Storage.S3.SecretKey != "" ||
		!cfg.Storage.S3.PathStyle {
		t.Errorf("Storage.S3 = %+v", cfg.Storage.S3)
	}
	if cfg.Repo.Name != "varve" || cfg.Repo.WorkDir != "/data/work" ||
		cfg.Repo.Sign != "off" || cfg.Repo.KeepVersions != 1 {
		t.Errorf("Repo = %+v", cfg.Repo)
	}
	if cfg.GPG.KeyID != "" || cfg.GPG.KeyFile != "" || cfg.GPG.Passphrase != "" {
		t.Errorf("GPG = %+v", cfg.GPG)
	}
	if cfg.Source.URL != "git@git.example.org:pkgbuilds.git" || cfg.Source.FetchKey != "" {
		t.Errorf("Source = %+v", cfg.Source)
	}
	if cfg.Source.PollInterval != 5*time.Minute {
		t.Errorf("Source.PollInterval = %v", cfg.Source.PollInterval)
	}
	if len(cfg.Source.ExcludeBranches) != 2 ||
		cfg.Source.ExcludeBranches[0] != "main" ||
		cfg.Source.ExcludeBranches[1] != "wip/*" {
		t.Errorf("Source.ExcludeBranches = %v", cfg.Source.ExcludeBranches)
	}
	if cfg.Worker.HeartbeatTimeout != 90*time.Second ||
		cfg.Worker.StallTimeout != 10*time.Minute ||
		cfg.Worker.BuildTimeout != 30*time.Minute {
		t.Errorf("Worker timeouts = %v/%v/%v", cfg.Worker.HeartbeatTimeout, cfg.Worker.StallTimeout, cfg.Worker.BuildTimeout)
	}
	if cfg.Worker.CPULimit != 0 || cfg.Worker.MemoryLimit != "" {
		t.Errorf("Worker limits = %d/%q", cfg.Worker.CPULimit, cfg.Worker.MemoryLimit)
	}
	if cfg.Worker.RetryMax != 3 || cfg.Worker.FailedRebuildCooldown != time.Hour {
		t.Errorf("Worker retry policy = %d/%v, want 3/1h0m0s", cfg.Worker.RetryMax, cfg.Worker.FailedRebuildCooldown)
	}
	if !cfg.Worker.Actions.Enabled || cfg.Worker.Actions.Token != "ghp-example" ||
		cfg.Worker.Actions.Repo != "owner/varve-runner" ||
		cfg.Worker.Actions.Workflow != "worker-actions.yml" ||
		cfg.Worker.Actions.Ref != "main" ||
		cfg.Worker.Actions.Cooldown != 5*time.Minute {
		t.Errorf("Worker.Actions = %+v", cfg.Worker.Actions)
	}
	if cfg.Mail.Enabled || cfg.Mail.Host != "" || cfg.Mail.Port != 587 ||
		cfg.Mail.Username != "" || cfg.Mail.Password != "" ||
		cfg.Mail.From != "varve@example.org" || cfg.Mail.TLS != "starttls" {
		t.Errorf("Mail = %+v", cfg.Mail)
	}
	if !cfg.Web.DownloadEnabled || cfg.Web.DownloadBaseURI != "https://dl.example.org" ||
		cfg.Web.RecentBuilds != 20 || len(cfg.Web.Admins) != 1 ||
		cfg.Web.Admins[0].User != "admin" || cfg.Web.Admins[0].Password != "change-me" {
		t.Errorf("Web = %+v", cfg.Web)
	}
	if cfg.Logs.Dir != "/data/logs" || cfg.Logs.Retention != 90*24*time.Hour || cfg.Logs.MaxBuilds != 1000 {
		t.Errorf("Logs = %+v", cfg.Logs)
	}
}

func TestLoadControllerMemoryLimitString(t *testing.T) {
	content := minimalConfig + `
[worker]
memory_limit = "8GiB"
`
	cfg, err := LoadController(writeConfig(t, content))
	if err != nil {
		t.Fatalf("LoadController: %v", err)
	}
	if cfg.Worker.MemoryLimit != "8GiB" {
		t.Errorf("MemoryLimit = %q, want %q", cfg.Worker.MemoryLimit, "8GiB")
	}
}

func TestLoadControllerDefaults(t *testing.T) {
	// A minimal config: only the required fields. Every other section and
	// key must fall back to its documented default.
	cfg, err := LoadController(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("LoadController: %v", err)
	}

	if cfg.Server.APIPort != ":31759" || cfg.Server.WebPort != ":31760" || cfg.Server.WebURL != "" {
		t.Errorf("Server defaults = %+v", cfg.Server)
	}
	if cfg.Database.Path != "/data/varve.db" {
		t.Errorf("Database.Path default = %q", cfg.Database.Path)
	}
	if cfg.Storage.Backend != "local" || cfg.Storage.Local.Root != "/data/repo" || !cfg.Storage.S3.PathStyle {
		t.Errorf("Storage defaults = %+v", cfg.Storage)
	}
	if cfg.Repo.Name != "varve" || cfg.Repo.WorkDir != "/data/work" ||
		cfg.Repo.Sign != "off" || cfg.Repo.KeepVersions != 1 {
		t.Errorf("Repo defaults = %+v", cfg.Repo)
	}
	if cfg.Source.PollInterval != 5*time.Minute ||
		len(cfg.Source.ExcludeBranches) != 1 || cfg.Source.ExcludeBranches[0] != "main" {
		t.Errorf("Source defaults = %+v", cfg.Source)
	}
	if cfg.Worker.HeartbeatTimeout != 90*time.Second ||
		cfg.Worker.StallTimeout != 10*time.Minute ||
		cfg.Worker.BuildTimeout != 30*time.Minute ||
		cfg.Worker.CPULimit != 0 || cfg.Worker.MemoryLimit != "" ||
		cfg.Worker.RetryMax != 3 || cfg.Worker.FailedRebuildCooldown != time.Hour {
		t.Errorf("Worker defaults = %+v", cfg.Worker)
	}
	if cfg.Worker.Actions.Enabled || cfg.Worker.Actions.Token != "" ||
		cfg.Worker.Actions.Repo != "" ||
		cfg.Worker.Actions.Workflow != "worker-actions.yml" ||
		cfg.Worker.Actions.Ref != "main" ||
		cfg.Worker.Actions.Cooldown != 3*time.Minute {
		t.Errorf("Worker.Actions defaults = %+v", cfg.Worker.Actions)
	}
	if cfg.Mail.Enabled || cfg.Mail.Port != 587 || cfg.Mail.TLS != "starttls" ||
		cfg.Mail.From != "varve@example.org" {
		t.Errorf("Mail defaults = %+v", cfg.Mail)
	}
	if !cfg.Web.DownloadEnabled || cfg.Web.RecentBuilds != 20 ||
		len(cfg.Web.Admins) != 1 || cfg.Web.Admins[0].User != "admin" || cfg.Web.Admins[0].Password != "p" {
		t.Errorf("Web defaults = %+v", cfg.Web)
	}
	if cfg.Logs.Dir != "/data/logs" || cfg.Logs.Retention != 90*24*time.Hour || cfg.Logs.MaxBuilds != 1000 {
		t.Errorf("Logs defaults = %+v", cfg.Logs)
	}
}

func TestLoadControllerSectionOverrides(t *testing.T) {
	tests := []struct {
		name  string
		extra string // section appended to minimalConfig (must be a fresh section)
		check func(*testing.T, *ControllerConfig)
	}{
		{
			name: "server values honored",
			extra: `
[server]
api_listen = "127.0.0.1:1"
web_listen = "127.0.0.1:2"
`,
			check: func(t *testing.T, c *ControllerConfig) {
				t.Helper()
				if c.Server.APIPort != "127.0.0.1:1" || c.Server.WebPort != "127.0.0.1:2" {
					t.Errorf("Server = %+v", c.Server)
				}
			},
		},
		{
			name: "storage s3 backend honored",
			extra: `
[storage]
backend = "s3"

[storage.s3]
path_style = false
`,
			check: func(t *testing.T, c *ControllerConfig) {
				t.Helper()
				if c.Storage.Backend != "s3" || c.Storage.S3.PathStyle {
					t.Errorf("Storage = %+v", c.Storage)
				}
			},
		},
		{
			name: "worker memory and cpu limits honored",
			extra: `
[worker]
cpu_limit = 4
memory_limit = "8GiB"
`,
			check: func(t *testing.T, c *ControllerConfig) {
				t.Helper()
				if c.Worker.CPULimit != 4 || c.Worker.MemoryLimit != "8GiB" {
					t.Errorf("Worker = %+v", c.Worker)
				}
			},
		},
		{
			name: "mail enabled honored",
			extra: `
[mail]
enabled = true
host = "smtp.example.org"
port = 465
tls = "implicit"
`,
			check: func(t *testing.T, c *ControllerConfig) {
				t.Helper()
				if !c.Mail.Enabled || c.Mail.Host != "smtp.example.org" ||
					c.Mail.Port != 465 || c.Mail.TLS != "implicit" {
					t.Errorf("Mail = %+v", c.Mail)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadController(writeConfig(t, minimalConfig+tt.extra))
			if err != nil {
				t.Fatalf("LoadController: %v", err)
			}
			tt.check(t, cfg)
		})
	}
}

func TestLoadControllerSourceExplicitEmpty(t *testing.T) {
	// An explicitly empty exclude_branches must override the ["main"]
	// default; a disabled download must override the true default.
	content := `
[api]
token = "t"

[source]
url = "git@x:y.git"
exclude_branches = []

[web]
download_enabled = false

[[web.admins]]
user = "admin"
password = "p"

[logs]
dir = "/data/logs"
`
	cfg, err := LoadController(writeConfig(t, content))
	if err != nil {
		t.Fatalf("LoadController: %v", err)
	}
	if len(cfg.Source.ExcludeBranches) != 0 {
		t.Errorf("ExcludeBranches = %v, want empty", cfg.Source.ExcludeBranches)
	}
	if cfg.Web.DownloadEnabled {
		t.Error("DownloadEnabled = true, want false")
	}
}

func TestLoadControllerUnknownFields(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // substring that must appear in the error
	}{
		{
			name: "typo in api section",
			in: `
[api]
token = "t"
tokn = "x"

[source]
url = "git@x:y.git"

[web]

[[web.admins]]
user = "admin"
password = "p"

[logs]
dir = "/data/logs"
`,
			want: "api.tokn",
		},
		{
			name: "typo in worker section",
			in:   minimalConfig + "\n[worker]\nheartbeattimeout = \"1m\"\n",
			want: "worker.heartbeattimeout",
		},
		{
			name: "typo in storage section",
			in:   minimalConfig + "\n[storage]\nbakend = \"local\"\n",
			want: "storage.bakend",
		},
		{
			name: "unknown top-level section",
			in:   minimalConfig + "\n[bogus]\nx = 1\n",
			want: "bogus",
		},
		{
			name: "unknown key in top-level table",
			in:   minimalConfig + "\nunknown_key = 1\n",
			want: "unknown_key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadController(writeConfig(t, tt.in))
			if err == nil {
				t.Fatalf("expected error for %q", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "unknown field") {
				t.Errorf("error %q does not mention unknown field", err)
			}
		})
	}
}

func TestLoadControllerMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.toml")
	_, err := LoadController(path)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not contain path %q", err, path)
	}
}

func TestLoadControllerEmptyPath(t *testing.T) {
	if _, err := LoadController(""); err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestLoadControllerEnvOverrides(t *testing.T) {
	// base includes [api] with the TOML token; token_file is injected per
	// subtest into the same [api] table.
	base := func(tokenFile string) string {
		extra := ""
		if tokenFile != "" {
			extra = `token_file = "` + tokenFile + `"` + "\n"
		}
		return `
[api]
token = "toml-token"
` + extra + `
[storage.s3]
access_key = "toml-key"
secret_key = "toml-secret"

[source]
url = "git@x:y.git"
fetch_key = "toml-fetch"

[web]
download_base_uri = "https://dl.example.org"

[[web.admins]]
user = "admin"
password = "p"

[logs]
dir = "/data/logs"
`
	}

	t.Run("VARVE_API_TOKEN wins over token_file and TOML", func(t *testing.T) {
		t.Setenv("VARVE_API_TOKEN", "env-token")
		tokenFile := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(tokenFile, []byte("file-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadController(writeConfig(t, base(tokenFile)))
		if err != nil {
			t.Fatalf("LoadController: %v", err)
		}
		if cfg.API.Token != "env-token" {
			t.Errorf("Token = %q, want env-token", cfg.API.Token)
		}
	})

	t.Run("token_file wins over TOML when no env", func(t *testing.T) {
		t.Setenv("VARVE_API_TOKEN", "")
		tokenFile := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(tokenFile, []byte("file-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadController(writeConfig(t, base(tokenFile)))
		if err != nil {
			t.Fatalf("LoadController: %v", err)
		}
		if cfg.API.Token != "file-token" {
			t.Errorf("Token = %q, want file-token", cfg.API.Token)
		}
	})

	t.Run("TOML token kept when no env and no token_file", func(t *testing.T) {
		t.Setenv("VARVE_API_TOKEN", "")
		cfg, err := LoadController(writeConfig(t, base("")))
		if err != nil {
			t.Fatalf("LoadController: %v", err)
		}
		if cfg.API.Token != "toml-token" {
			t.Errorf("Token = %q, want toml-token", cfg.API.Token)
		}
	})

	t.Run("S3 keys and fetch key overridden", func(t *testing.T) {
		t.Setenv("VARVE_API_TOKEN", "")
		t.Setenv("VARVE_S3_ACCESS_KEY", "env-access")
		t.Setenv("VARVE_S3_SECRET_KEY", "env-secret")
		t.Setenv("VARVE_SOURCE_FETCH_KEY", "env-fetch")
		cfg, err := LoadController(writeConfig(t, base("")))
		if err != nil {
			t.Fatalf("LoadController: %v", err)
		}
		if cfg.Storage.S3.AccessKey != "env-access" || cfg.Storage.S3.SecretKey != "env-secret" {
			t.Errorf("S3 keys = %q/%q", cfg.Storage.S3.AccessKey, cfg.Storage.S3.SecretKey)
		}
		if cfg.Source.FetchKey != "env-fetch" {
			t.Errorf("FetchKey = %q", cfg.Source.FetchKey)
		}
	})

	t.Run("missing token_file errors", func(t *testing.T) {
		t.Setenv("VARVE_API_TOKEN", "")
		missing := filepath.Join(t.TempDir(), "missing-token")
		_, err := LoadController(writeConfig(t, base(missing)))
		if err == nil {
			t.Fatal("expected error for missing token_file")
		}
		if !strings.Contains(err.Error(), "token_file") || !strings.Contains(err.Error(), missing) {
			t.Errorf("error %q does not mention token_file %q", err, missing)
		}
	})
}

func TestLoadControllerPermissionWarning(t *testing.T) {
	capture := func(t *testing.T, path string) string {
		t.Helper()
		var buf bytes.Buffer
		old := warnW
		warnW = &buf
		t.Cleanup(func() { warnW = old })
		if _, err := LoadController(path); err != nil {
			t.Fatalf("LoadController: %v", err)
		}
		return buf.String()
	}

	t.Run("0644 warns", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "varve.toml")
		if err := os.WriteFile(path, []byte(minimalConfig), 0o644); err != nil {
			t.Fatal(err)
		}
		out := capture(t, path)
		if !strings.Contains(out, "0600") {
			t.Errorf("warning %q does not mention 0600", out)
		}
	})

	t.Run("0600 silent", func(t *testing.T) {
		path := writeConfig(t, minimalConfig)
		out := capture(t, path)
		if out != "" {
			t.Errorf("unexpected warning for 0600: %q", out)
		}
	})
}

func TestLoadControllerPasswordWipe(t *testing.T) {
	// The exported helper must zero the buffer.
	b := []byte("hunter2")
	WipeBytes(b)
	for i, c := range b {
		if c != 0 {
			t.Fatalf("WipeBytes: byte %d = %d, want 0", i, c)
		}
	}

	// The config values must survive loading (export happens before wipe).
	content := `
[api]
token = "t"

[source]
url = "git@x:y.git"

[gpg]
key_id = "K"
passphrase = "gpg-pass"

[mail]
host = "smtp.example.org"
password = "mail-pass"

[web]
download_base_uri = "https://dl.example.org"

[[web.admins]]
user = "admin"
password = "admin-pass"

[worker.actions]
enabled = true
token = "actions-pass"
repo = "owner/varve-runner"

[logs]
dir = "/data/logs"
`
	cfg, err := LoadController(writeConfig(t, content))
	if err != nil {
		t.Fatalf("LoadController: %v", err)
	}
	if cfg.GPG.Passphrase != "gpg-pass" || cfg.Mail.Password != "mail-pass" ||
		len(cfg.Web.Admins) != 1 || cfg.Web.Admins[0].User != "admin" || cfg.Web.Admins[0].Password != "admin-pass" {
		t.Errorf("passwords not preserved: %+v %+v %+v", cfg.GPG, cfg.Mail, cfg.Web)
	}
	if cfg.Worker.Actions.Token != "actions-pass" {
		t.Errorf("actions token not preserved: %q", cfg.Worker.Actions.Token)
	}
}

func TestLoadControllerBadDuration(t *testing.T) {
	content := `
[api]
token = "t"

[source]
url = "git@x:y.git"
poll_interval = "abc"

[web]
download_base_uri = "https://dl.example.org"

[[web.admins]]
user = "admin"
password = "p"

[logs]
dir = "/data/logs"
`
	_, err := LoadController(writeConfig(t, content))
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
	if !strings.Contains(err.Error(), "poll_interval") {
		t.Errorf("error %q does not mention poll_interval", err)
	}
}
