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
	Enabled         bool
	CurrentVersion  string
	Pkgdesc         string
	URL             string   // upstream url from .SRCINFO
	Licenses        []string // .SRCINFO license entries
	Conflicts       []string // .SRCINFO conflict entries
	Provides        []string // .SRCINFO provides entries
	LastSrcinfoHash string
	LastUpstreamRef string
	LastFailedAt    *time.Time // when the package's build last failed (rebuild cooldown marker)
	LastBuildID     string     // 16-hex hash of the latest build row
	Maintainers     []string
}

// PackageUpdate carries the outcome of a successful build onto the
// package row: the version/description/hash records plus the verified
// .SRCINFO metadata (url, licenses, conflicts, provides).
type PackageUpdate struct {
	CurrentVersion string
	Pkgdesc        string
	SrcinfoHash    string
	UpstreamRef    string
	BuildID        string
	URL            string
	Licenses       []string
	Conflicts      []string
	Provides       []string
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
	SrcinfoHash   string
	Status        string
	WorkerID      int64
	WorkerName    string
	LogPath       string
	StartedAt     *time.Time
	FinishedAt    *time.Time
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

// Sample is one cgroup resource sample. It is stored inside the builds.resource_usage JSON column.
type Sample struct {
	At          time.Time `json:"at"`
	CPUTimeNS   int64     `json:"cpu_time_ns"`
	MemoryBytes int64     `json:"memory_bytes"`
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

// decodeStrings decodes a maintainers JSON array.
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
