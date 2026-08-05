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
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// secret holds a password in a mutable []byte buffer so that it can be
// scrubbed from memory after configuration validation (DETAIL §1.3).
type secret []byte

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *secret) UnmarshalText(text []byte) error {
	*s = append((*s)[:0], text...)
	return nil
}

// tomlDuration decodes TOML duration strings such as "90s", "5m" or "90d".
// time.Duration itself does not implement encoding.TextUnmarshaler, and Go's
// ParseDuration does not accept day units, so the decode layer uses this
// adapter (DETAIL §1.4).
type tomlDuration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *tomlDuration) UnmarshalText(text []byte) error {
	v, err := parseDuration(string(text))
	if err != nil {
		return err
	}
	*d = tomlDuration(v)
	return nil
}

// parseDuration parses a duration string, additionally accepting a trailing
// day unit such as "90d" (DESIGN §8.1).
func parseDuration(s string) (time.Duration, error) {
	if n := len(s); n > 1 && s[n-1] == 'd' {
		if d, err := strconv.ParseFloat(s[:n-1], 64); err == nil {
			return time.Duration(d * float64(24*time.Hour)), nil
		}
	}
	return time.ParseDuration(s)
}

// tomlMemory decodes the worker.memory_limit field, which may be written
// either as the integer 0 (no limit) or as a size string such as "8GiB"
// (DESIGN §8.1). The integer form is normalized to the empty string.
type tomlMemory string

// UnmarshalText implements encoding.TextUnmarshaler; go-toml feeds the raw
// text for both integer and string values.
func (m *tomlMemory) UnmarshalText(text []byte) error {
	if _, err := strconv.ParseInt(string(text), 10, 64); err == nil {
		if string(text) != "0" {
			return fmt.Errorf("expected 0 or a size string such as \"8GiB\"")
		}
		*m = ""
		return nil
	}
	*m = tomlMemory(text)
	return nil
}

// The raw* structs mirror the TOML schema (DESIGN §8.1) with decode-layer
// types. They are prefilled with defaults so that omitted sections and keys
// keep their documented values, then exported into ControllerConfig.
type rawConfig struct {
	Server   rawServer
	API      rawAPI
	Database rawDatabase
	Storage  rawStorage
	Repo     rawRepo
	GPG      rawGPG
	Source   rawSource
	Worker   rawWorker
	Mail     rawMail
	Web      rawWeb
	Logs     rawLogs
}

type rawServer struct {
	APIPort string `toml:"api_listen"`
	WebPort string `toml:"web_listen"`
	WebURL  string `toml:"web_url"`
}

type rawAPI struct {
	Token     string `toml:"token"`
	TokenFile string `toml:"token_file"`
}

type rawDatabase struct {
	Path string `toml:"path"`
}

type rawStorage struct {
	Backend string   `toml:"backend"`
	Local   rawLocal `toml:"local"`
	S3      rawS3    `toml:"s3"`
}

type rawLocal struct {
	Root string `toml:"root"`
}

type rawS3 struct {
	Endpoint  string `toml:"endpoint"`
	Bucket    string `toml:"bucket"`
	Region    string `toml:"region"`
	AccessKey string `toml:"access_key"`
	SecretKey string `toml:"secret_key"`
	PathStyle bool   `toml:"path_style"`
}

type rawRepo struct {
	Name         string `toml:"name"`
	WorkDir      string `toml:"work_dir"`
	Sign         string `toml:"sign"`
	KeepVersions int    `toml:"keep_versions"`
}

type rawGPG struct {
	KeyID      string `toml:"key_id"`
	KeyFile    string `toml:"key_file"`
	Passphrase secret `toml:"passphrase"`
}

type rawSource struct {
	URL             string       `toml:"url"`
	FetchKey        string       `toml:"fetch_key"`
	PollInterval    tomlDuration `toml:"poll_interval"`
	ExcludeBranches []string     `toml:"exclude_branches"`
}

type rawWorker struct {
	HeartbeatTimeout tomlDuration `toml:"heartbeat_timeout"`
	StallTimeout     tomlDuration `toml:"stall_timeout"`
	BuildTimeout     tomlDuration `toml:"build_timeout"`
	CPULimit         int          `toml:"cpu_limit"`
	MemoryLimit      tomlMemory   `toml:"memory_limit"`
}

type rawMail struct {
	Enabled  bool   `toml:"enabled"`
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	Username string `toml:"username"`
	Password secret `toml:"password"`
	From     string `toml:"from"`
	TLS      string `toml:"tls"`
}

type rawWeb struct {
	DownloadEnabled bool   `toml:"download_enabled"`
	DownloadBaseURI string `toml:"download_base_uri"`
	AdminUser       string `toml:"admin_user"`
	AdminPassword   secret `toml:"admin_password"`
}

type rawLogs struct {
	Dir       string       `toml:"dir"`
	Retention tomlDuration `toml:"retention"`
	MaxBuilds int          `toml:"max_builds"`
}

// defaultRawConfig returns the raw decode struct prefilled with the
// documented defaults (DESIGN §8.1, DETAIL §1.2).
func defaultRawConfig() rawConfig {
	return rawConfig{
		Server: rawServer{
			APIPort: ":31759",
			WebPort: ":31760",
		},
		Database: rawDatabase{Path: "/data/varve.db"},
		Storage: rawStorage{
			Backend: "local",
			Local:   rawLocal{Root: "/data/repo"},
			S3:      rawS3{PathStyle: true},
		},
		Repo: rawRepo{
			Name:         "varve",
			WorkDir:      "/data/work",
			Sign:         "off",
			KeepVersions: 1,
		},
		Source: rawSource{
			PollInterval:    tomlDuration(5 * time.Minute),
			ExcludeBranches: []string{"main"},
		},
		Worker: rawWorker{
			HeartbeatTimeout: tomlDuration(90 * time.Second),
			StallTimeout:     tomlDuration(10 * time.Minute),
			BuildTimeout:     tomlDuration(30 * time.Minute),
		},
		Mail: rawMail{
			Port: 587,
			From: "varve@example.org",
			TLS:  "starttls",
		},
		Web: rawWeb{
			DownloadEnabled: true,
			AdminUser:       "admin",
		},
		Logs: rawLogs{
			Dir:       "/data/logs",
			Retention: tomlDuration(90 * 24 * time.Hour),
			MaxBuilds: 1000,
		},
	}
}

// LoadController reads, parses and validates the controller configuration at
// path (DETAIL §1.4). It is called once at startup and is safe for concurrent
// use; the returned configuration must be treated as read-only.
//
// Flow: read file -> warn if permissions are not 0600 (non-fatal) -> strict
// TOML parse (unknown fields rejected) -> env overrides (VARVE_API_TOKEN >
// token_file > TOML api.token; S3 keys; source fetch key) -> validation ->
// scrub password buffers.
func LoadController(path string) (*ControllerConfig, error) {
	if path == "" {
		return nil, errors.New("config: empty config path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	if fi, err := os.Stat(path); err == nil {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			fmt.Fprintf(warnW, "varve: warning: config file %s has permissions %o, expected 0600\n", path, perm)
		}
	}

	raw := defaultRawConfig()
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		var sme *toml.StrictMissingError
		if errors.As(err, &sme) {
			names := make([]string, 0, len(sme.Errors))
			for _, e := range sme.Errors {
				names = append(names, strings.Join(e.Key(), "."))
			}
			return nil, fmt.Errorf("config: parse %s: unknown field(s): %s", path, strings.Join(names, ", "))
		}
		var de *toml.DecodeError
		if errors.As(err, &de) {
			if key := strings.Join(de.Key(), "."); key != "" {
				return nil, fmt.Errorf("config: parse %s: %s: %w", path, key, err)
			}
		}
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := applyEnvOverrides(&raw); err != nil {
		return nil, err
	}

	cfg := raw.export()
	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("config: invalid %s: %w", path, err)
	}
	raw.wipeSecrets()
	return cfg, nil
}

// applyEnvOverrides applies the documented environment overrides (DETAIL
// §1.4): a set-and-non-empty environment variable wins over everything else;
// token_file is only consulted when VARVE_API_TOKEN is absent. Password-class
// fields (admin_password, mail.password, gpg.passphrase) are never overridden
// from the environment.
func applyEnvOverrides(r *rawConfig) error {
	if v, ok := os.LookupEnv("VARVE_API_TOKEN"); ok && v != "" {
		r.API.Token = v
	} else if r.API.TokenFile != "" {
		data, err := os.ReadFile(r.API.TokenFile)
		if err != nil {
			return fmt.Errorf("config: read token_file %s: %w", r.API.TokenFile, err)
		}
		r.API.Token = strings.TrimSpace(string(data))
	}
	if v, ok := os.LookupEnv("VARVE_S3_ACCESS_KEY"); ok && v != "" {
		r.Storage.S3.AccessKey = v
	}
	if v, ok := os.LookupEnv("VARVE_S3_SECRET_KEY"); ok && v != "" {
		r.Storage.S3.SecretKey = v
	}
	if v, ok := os.LookupEnv("VARVE_SOURCE_FETCH_KEY"); ok && v != "" {
		r.Source.FetchKey = v
	}
	return nil
}

// export copies the decoded raw configuration into the exported
// ControllerConfig. Password buffers are copied to immutable strings here so
// that they can be wiped afterwards (DETAIL §1.3).
func (r *rawConfig) export() *ControllerConfig {
	return &ControllerConfig{
		Server: ServerConfig{
			APIPort: r.Server.APIPort,
			WebPort: r.Server.WebPort,
			WebURL:  r.Server.WebURL,
		},
		API: APIConfig{
			Token:     r.API.Token,
			TokenFile: r.API.TokenFile,
		},
		Database: DatabaseConfig{Path: r.Database.Path},
		Storage: StorageConfig{
			Backend: r.Storage.Backend,
			Local:   LocalConfig{Root: r.Storage.Local.Root},
			S3: S3Config{
				Endpoint:  r.Storage.S3.Endpoint,
				Bucket:    r.Storage.S3.Bucket,
				Region:    r.Storage.S3.Region,
				AccessKey: r.Storage.S3.AccessKey,
				SecretKey: r.Storage.S3.SecretKey,
				PathStyle: r.Storage.S3.PathStyle,
			},
		},
		Repo: RepoConfig{
			Name:         r.Repo.Name,
			WorkDir:      r.Repo.WorkDir,
			Sign:         r.Repo.Sign,
			KeepVersions: r.Repo.KeepVersions,
		},
		GPG: GPGConfig{
			KeyID:      r.GPG.KeyID,
			KeyFile:    r.GPG.KeyFile,
			Passphrase: string(r.GPG.Passphrase),
		},
		Source: SourceConfig{
			URL:             r.Source.URL,
			FetchKey:        r.Source.FetchKey,
			PollInterval:    time.Duration(r.Source.PollInterval),
			ExcludeBranches: r.Source.ExcludeBranches,
		},
		Worker: WorkerLimits{
			HeartbeatTimeout: time.Duration(r.Worker.HeartbeatTimeout),
			StallTimeout:     time.Duration(r.Worker.StallTimeout),
			BuildTimeout:     time.Duration(r.Worker.BuildTimeout),
			CPULimit:         r.Worker.CPULimit,
			MemoryLimit:      string(r.Worker.MemoryLimit),
		},
		Mail: MailConfig{
			Enabled:  r.Mail.Enabled,
			Host:     r.Mail.Host,
			Username: r.Mail.Username,
			Password: string(r.Mail.Password),
			From:     r.Mail.From,
			Port:     r.Mail.Port,
			TLS:      r.Mail.TLS,
		},
		Web: WebConfig{
			DownloadEnabled: r.Web.DownloadEnabled,
			DownloadBaseURI: r.Web.DownloadBaseURI,
			AdminUser:       r.Web.AdminUser,
			AdminPassword:   string(r.Web.AdminPassword),
		},
		Logs: LogsConfig{
			Dir:       r.Logs.Dir,
			Retention: time.Duration(r.Logs.Retention),
			MaxBuilds: r.Logs.MaxBuilds,
		},
	}
}

// wipeSecrets scrubs the password buffers held by the raw decode struct.
func (r *rawConfig) wipeSecrets() {
	WipeBytes(r.GPG.Passphrase)
	WipeBytes(r.Mail.Password)
	WipeBytes(r.Web.AdminPassword)
}

// WipeBytes zeroes the contents of b. It is exported so that password
// buffers used during configuration parsing can be scrubbed from memory
// (DETAIL §1.3); Go strings are immutable, so only the parse copies can be
// wiped.
func WipeBytes(b []byte) {
	clear(b)
}
