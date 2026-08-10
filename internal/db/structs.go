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

// Package db is the single persistence entry point of the varve
// controller: it owns the SQLite schema, the migration ledger and all
// table operations. Write access goes through one serialized connection;
// reads use a pooled read connection set (WAL).
package db

import (
	"encoding/json"
	"errors"
	"time"
)

// Sentinel errors returned by the store. Callers distinguish them with
// errors.Is and map them to HTTP semantics at the API boundary.
var (
	// ErrNotFound reports a missing row (package, build, worker or task).
	ErrNotFound = errors.New("db: not found")
	// ErrNoTask reports that no claimable task exists.
	ErrNoTask = errors.New("db: no task")
	// ErrConflict reports a uniqueness or state transition conflict.
	ErrConflict = errors.New("db: conflict")
)

// Maintainer is one dotfile maintainer entry: a display name plus the
// notification email address. The email feeds failure and AUR push
// notifications; the name is the display form on the package page. The
// packages.maintainers column stores a JSON object list of these entries;
// legacy string-list rows decode as email-only maintainers (see
// decodeMaintainers).
type Maintainer struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// MaintainerEmails returns the notification addresses of the maintainers,
// skipping entries without an email.
func MaintainerEmails(ms []Maintainer) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if m.Email != "" {
			out = append(out, m.Email)
		}
	}
	return out
}

// Package mirrors one packages row. Arch is the canonical architecture
// set: "any" for architecture-independent packages, otherwise every
// declared architecture joined with "|" (e.g. "aarch64|x86_64"). Claim
// matching treats any element as a match.
type Package struct {
	ID              int64
	Pkgbase         string
	Branch          string
	VCSKind         string
	Arch            string // "any" or "|"-joined architecture set
	CurrentVersion  string
	Pkgdesc         string
	URL             string   // upstream url from .SRCINFO
	Licenses        []string // .SRCINFO license entries
	Conflicts       []string // .SRCINFO conflict entries
	Provides        []string // .SRCINFO provides entries
	Pkgname         []string // .SRCINFO package names
	Source          []string // .SRCINFO source entries
	Pkgver          string   // .SRCINFO pkgver, split from current_version, epoch stripped
	Pkgrel          string   // .SRCINFO pkgrel, split from current_version
	Epoch           int      // .SRCINFO epoch prefix (0 when pkgver has none)
	LastCommit      string   // branch tip commit of the last successful build
	LastSrcinfoHash string
	LastUpstreamRef string
	PkgbuildRef     string     // external pkgbuild_source repo head of the last successful build
	LastFailedAt    *time.Time // when the package's build last failed (rebuild cooldown marker)
	LastBuildID     string     // 16-hex hash of the latest build row
	Maintainers     []Maintainer

	// AUR publishing records (branch dotfile [aur] section, refreshed at
	// enqueue; empty/zero = the branch is not published to AUR).
	AURName       string     // AUR package name
	AURSubmit     bool       // submit flag: push after every successful build
	LastAURPushAt *time.Time // when the last AUR push was attempted
	LastAURCommit string     // commit of the last attempted AUR push
	LastAURError  string     // last push error text; empty when the last push succeeded
}

// PackageUpdate carries the outcome of a successful build onto the
// package row: the version/description/hash records plus the verified
// .SRCINFO metadata (url, licenses, conflicts, provides, pkgname,
// source, pkgver, epoch, pkgrel) and the commit that was actually
// built.
type PackageUpdate struct {
	CurrentVersion string
	Pkgdesc        string
	SrcinfoHash    string
	UpstreamRef    string
	PkgbuildRef    string
	BuildID        string
	URL            string
	Licenses       []string
	Conflicts      []string
	Provides       []string
	Pkgname        []string
	Source         []string
	Pkgver         string
	Pkgrel         string
	Epoch          int
	Commit         string
}

// Build mirrors one builds row. ID is a 16-hex hash; WorkerName is the
// plain-text machine name that executed the build (worker_id is kept as a
// nullable provenance hint without a foreign key, so deleting a worker
// never orphans build history).
type Build struct {
	ID            string
	PackageID     int64
	Branch        string
	Commit        string
	UpstreamRef   string
	PkgbuildRef   string
	SrcinfoHash   string
	Status        string
	WorkerID      int64
	WorkerName    string
	LogPath       string
	StartedAt     *time.Time
	FinishedAt    *time.Time
	CreatedAt     *time.Time // when the build was enqueued (mirrors the task created_at)
	Error         string
	Artifacts     []Artifact
	ResourceUsage []Sample
}

// Worker mirrors one workers row.
type Worker struct {
	ID            int64
	Name          string
	Role          string
	Mode          string
	Arch          string
	Capacity      int
	Status        string
	LastHeartbeat *time.Time
	Version       string
}

// Task mirrors one tasks row. FailCount counts the failed attempts of the
// task; the retry policy (a later change) re-queues a failed task while
// FailCount stays below the configured retry budget.
type Task struct {
	ID              string
	PackageID       int64
	BuildID         string
	WorkerID        int64
	State           string
	AssignedAt      *time.Time
	CreatedAt       time.Time
	LastProgressAt  time.Time
	Attempts        int
	ClaimToken      string
	CancelRequested bool
	FailCount       int
}

// Sample is one resource sample. It is stored inside the builds.resource_usage JSON column; the disk fields are omitempty so records written before disk sampling decode with zero values.
type Sample struct {
	At                 time.Time `json:"at"`
	CPUTimeNS          int64     `json:"cpu_time_ns"`
	MemoryBytes        int64     `json:"memory_bytes"`
	DiskTotalBytes     int64     `json:"disk_total_bytes,omitempty"`
	DiskAvailableBytes int64     `json:"disk_available_bytes,omitempty"`
	DiskUsedBytes      int64     `json:"disk_used_bytes,omitempty"`
}

// Artifact describes one uploaded build artifact. It is stored inside the
// builds.artifacts JSON column.
type Artifact struct {
	File    string `json:"file"`
	Kind    string `json:"kind"`
	Pkgname string `json:"pkgname"`
	Version string `json:"version"`
	Arch    string `json:"arch"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

// timeLayout is the fixed-width RFC3339 layout used for every stored
// timestamp. A fixed-width fractional part keeps plain TEXT comparisons
// (ORDER BY, range filters) chronologically correct.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// formatTime renders t as UTC RFC3339 text with nanosecond precision.
func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

// formatNullableTime renders an optional timestamp for storage.
func formatNullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

// parseTime parses stored RFC3339 text back into a time.Time.
func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// maxSamples is the number of resource samples kept per build: samples
// are read every second to a few seconds, so long builds would otherwise
// grow the JSON column without bound. The cap keeps the most recent
// maxSamples entries, which is all the performance display needs.
const maxSamples = 200

// capSamples truncates a sample list to its most recent maxSamples
// entries, dropping the oldest ones when the list exceeds the cap.
func capSamples(s []Sample) []Sample {
	if len(s) <= maxSamples {
		return s
	}
	return s[len(s)-maxSamples:]
}

// decodeMaintainers decodes a packages.maintainers JSON value. The current
// shape is an object list ([{"name": .., "email": ..}]); rows written by
// older versions store a plain string list of email addresses, which maps
// to maintainers with an empty name. Any other value is an error so a
// corrupt column surfaces instead of panicking.
func decodeMaintainers(s string) ([]Maintainer, error) {
	var out []Maintainer
	if err := json.Unmarshal([]byte(s), &out); err == nil {
		return out, nil
	}
	var legacy []string
	if err := json.Unmarshal([]byte(s), &legacy); err != nil {
		return nil, err
	}
	conv := make([]Maintainer, 0, len(legacy))
	for _, email := range legacy {
		conv = append(conv, Maintainer{Email: email})
	}
	return conv, nil
}

// decodeStrings decodes a JSON string array.
func decodeStrings(s string) ([]string, error) {
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// decodeArtifacts decodes a builds.artifacts JSON array.
func decodeArtifacts(s string) ([]Artifact, error) {
	var out []Artifact
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// decodeSamples decodes a builds.resource_usage JSON array.
func decodeSamples(s string) ([]Sample, error) {
	var out []Sample
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// encodeJSON marshals a value for a JSON column.
func encodeJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
