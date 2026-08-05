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
	"errors"
	"fmt"
	"time"
)

// validate checks a resolved ControllerConfig against the rule table of
// DETAIL §1.4 (rule 3). The first failing rule is reported with the concrete
// field name.
func validate(c *ControllerConfig) error {
	switch c.Storage.Backend {
	case "local", "s3":
	default:
		return fmt.Errorf("storage.backend: must be one of %q, %q, got %q", "local", "s3", c.Storage.Backend)
	}
	switch c.Repo.Sign {
	case "off", "packages", "packages+db":
	default:
		return fmt.Errorf("repo.sign: must be one of %q, %q, %q, got %q", "off", "packages", "packages+db", c.Repo.Sign)
	}
	switch c.Mail.TLS {
	case "none", "starttls", "implicit":
	default:
		return fmt.Errorf("mail.tls: must be one of %q, %q, %q, got %q", "none", "starttls", "implicit", c.Mail.TLS)
	}
	if c.Web.DownloadEnabled && c.Web.DownloadBaseURI == "" {
		return errors.New("web.download_base_uri: required when web.download_enabled is true")
	}
	if c.Repo.KeepVersions != 1 {
		return fmt.Errorf("repo.keep_versions: only 1 is supported, got %d", c.Repo.KeepVersions)
	}
	if c.API.Token == "" {
		return errors.New("api.token: must not be empty (set it in the config file, via token_file, or VARVE_API_TOKEN)")
	}
	if c.Web.AdminPassword == "" {
		return errors.New("web.admin_password: must not be empty")
	}
	if c.Repo.Sign != "off" && c.GPG.KeyID == "" && c.GPG.KeyFile == "" {
		return errors.New("gpg.key_id/gpg.key_file: at least one is required when repo.sign is not \"off\"")
	}
	if c.Source.URL == "" {
		return errors.New("source.url: must not be empty")
	}
	if c.Logs.Dir == "" {
		return errors.New("logs.dir: must not be empty")
	}
	for _, d := range []struct {
		name string
		val  time.Duration
	}{
		{"source.poll_interval", c.Source.PollInterval},
		{"worker.heartbeat_timeout", c.Worker.HeartbeatTimeout},
		{"worker.stall_timeout", c.Worker.StallTimeout},
		{"worker.build_timeout", c.Worker.BuildTimeout},
		{"logs.retention", c.Logs.Retention},
	} {
		if d.val <= 0 {
			return fmt.Errorf("%s: must be greater than zero, got %s", d.name, d.val)
		}
	}
	return nil
}
