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
	"path"
	"path/filepath"
	"strings"
	"time"
)

// validate checks a resolved ControllerConfig against the validation rules.
// The first failing rule is reported with the concrete field name. The
// staging settings are normalized in place: the local staging directory is
// resolved to a clean absolute path and the S3 staging and repository
// prefixes are validated as safe object-key prefixes.
func validate(c *ControllerConfig) error {
	c.Storage.Local.StagingDir = resolveLocalStagingDir(c.Storage.Local.Root, c.Storage.Local.StagingDir)
	if c.Storage.S3.StagingPrefix == "" {
		c.Storage.S3.StagingPrefix = "staging"
	}
	if err := validKeyPrefix(c.Storage.S3.StagingPrefix); err != nil {
		return fmt.Errorf("storage.s3.staging_prefix: %w", err)
	}
	if c.Storage.S3.RepoPrefix != "" {
		if err := validKeyPrefix(c.Storage.S3.RepoPrefix); err != nil {
			return fmt.Errorf("storage.s3.repo_prefix: %w", err)
		}
	}
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
	if len(c.Web.Admins) == 0 {
		return errors.New("web.admins: at least one entry is required")
	}
	seenUsers := make(map[string]bool, len(c.Web.Admins))
	for i, a := range c.Web.Admins {
		if a.User == "" {
			return fmt.Errorf("web.admins[%d].user: must not be empty", i)
		}
		if seenUsers[a.User] {
			return fmt.Errorf("web.admins: duplicate user %q", a.User)
		}
		seenUsers[a.User] = true
		if a.Password == "" {
			return fmt.Errorf("web.admins[%d].password: must not be empty", i)
		}
	}
	if c.Web.RecentBuilds < 1 {
		return fmt.Errorf("web.recent_builds: must be greater than zero, got %d", c.Web.RecentBuilds)
	}
	if c.Worker.RetryMax < 0 {
		return fmt.Errorf("worker.retry_max: must not be negative, got %d", c.Worker.RetryMax)
	}
	if err := validatePackager(c.Worker.Packager); err != nil {
		return err
	}
	if c.Repo.Sign != "off" && c.GPG.KeyID == "" && c.GPG.KeyFile == "" {
		return errors.New("gpg.key_id/gpg.key_file: at least one is required when repo.sign is not \"off\"")
	}
	if c.Source.URL == "" {
		return errors.New("source.url: must not be empty")
	}
	if c.Worker.Actions.Enabled {
		if c.Worker.Actions.Token == "" {
			return errors.New("worker.actions.token: must not be empty when worker.actions.enabled is true")
		}
		if c.Worker.Actions.Repo == "" {
			return errors.New("worker.actions.repo: must not be empty when worker.actions.enabled is true")
		}
	}
	if c.Worker.Actions.MaxConcurrency < 1 {
		return fmt.Errorf("worker.actions.max_concurrency: must be greater than zero, got %d", c.Worker.Actions.MaxConcurrency)
	}
	if c.Logs.Dir == "" {
		return errors.New("logs.dir: must not be empty")
	}
	if c.Logs.KeepSuccessful < 0 {
		return fmt.Errorf("logs.keep_successful: must not be negative, got %d", c.Logs.KeepSuccessful)
	}
	// AUR publishing is enabled by an SSH key; the endpoint fields are
	// required then (they have defaults, so only an explicit empty value
	// trips this). With an empty key_file the whole section is inert.
	if c.AUR.KeyFile != "" {
		if c.AUR.Server == "" {
			return errors.New("aur.server: must not be empty when aur.key_file is set")
		}
		if c.AUR.User == "" {
			return errors.New("aur.user: must not be empty when aur.key_file is set")
		}
	}
	for _, d := range []struct {
		name string
		val  time.Duration
	}{
		{"source.poll_interval", c.Source.PollInterval},
		{"worker.heartbeat_timeout", c.Worker.HeartbeatTimeout},
		{"worker.stall_timeout", c.Worker.StallTimeout},
		{"worker.build_timeout", c.Worker.BuildTimeout},
		{"worker.failed_rebuild_cooldown", c.Worker.FailedRebuildCooldown},
		{"worker.actions.claim_timeout", c.Worker.Actions.ClaimTimeout},
		{"logs.retention", c.Logs.Retention},
	} {
		if d.val <= 0 {
			return fmt.Errorf("%s: must be greater than zero, got %s", d.name, d.val)
		}
	}
	return nil
}

// resolveLocalStagingDir resolves the local staging upload directory: an
// empty value keeps the default <root>/staging, an absolute value is used
// as-is, and a relative value is joined onto the repository root. The
// result is cleaned and absolutized, so redundant "." segments, parent
// traversal and relative escapes are removed and the staging tree is
// confined to one deterministic location.
func resolveLocalStagingDir(root, dir string) string {
	if dir == "" {
		dir = filepath.Join(root, "staging")
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		if abs, err := filepath.Abs(dir); err == nil {
			return abs
		}
	}
	return dir
}

// validKeyPrefix checks that prefix is a usable object-key prefix: a
// normalized virtual path whose segments consist of whitelisted
// characters. Staged names are built as "<stagingPrefix>/<taskID>/<file>"
// and repository names as "<repoPrefix>/<name>", so either prefix must
// keep the composed name safe for both backends (no leading or trailing
// slash, no "." or ".." segments, no characters outside [A-Za-z0-9._+-]).
func validKeyPrefix(prefix string) error {
	if prefix == "" {
		return errors.New("must not be empty")
	}
	if strings.HasPrefix(prefix, "/") {
		return fmt.Errorf("%q must not start with a slash", prefix)
	}
	if strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("%q must not end with a slash", prefix)
	}
	// path.Clean above already rejects every prefix with an empty or "."
	// segment (Clean normalizes them away). A leading ".." survives
	// Clean though (".." and "../x" are clean paths), so the explicit
	// ".." rejection below is reachable and kept.
	if path.Clean(prefix) != prefix {
		return fmt.Errorf("%q must be a clean path", prefix)
	}
	for _, seg := range strings.Split(prefix, "/") {
		if seg == ".." {
			return fmt.Errorf("%q must not contain a %q segment", prefix, seg)
		}
		if !validPrefixSegment(seg) {
			return fmt.Errorf("%q: segment %q contains characters outside [A-Za-z0-9._+-]", prefix, seg)
		}
	}
	return nil
}

// validPrefixSegment reports whether a single prefix segment contains only
// whitelisted characters.
func validPrefixSegment(seg string) bool {
	for _, r := range seg {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '+', r == '-':
		default:
			return false
		}
	}
	return true
}

// validatePackager checks the worker.packager identity: empty is valid
// (no PACKAGER is injected), a non-empty value must be the pacman
// "Name <email>" shape so the value is safe to export into a build
// environment.
func validatePackager(packager string) error {
	if packager == "" {
		return nil
	}
	open := strings.IndexByte(packager, '<')
	close := strings.LastIndexByte(packager, '>')
	if open < 0 || close <= open {
		return fmt.Errorf("worker.packager: must be \"Name <email>\", got %q", packager)
	}
	if strings.TrimSpace(packager[:open]) == "" || strings.TrimSpace(packager[open+1:close]) == "" {
		return fmt.Errorf("worker.packager: must be \"Name <email>\", got %q", packager)
	}
	if strings.TrimSpace(packager[close+1:]) != "" {
		return fmt.Errorf("worker.packager: must be \"Name <email>\", got %q", packager)
	}
	return nil
}
