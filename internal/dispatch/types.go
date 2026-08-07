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

package dispatch

import (
	"time"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/repo"
)

// This file defines the worker ↔ controller wire protocol types. JSON
// fields are snake_case to match the contract byte-for-byte. The API
// module re-exports these types by alias so the worker packages depend
// only on api.

// RegisterReq is the POST /register payload.
type RegisterReq struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	Mode     string `json:"mode"`
	Arch     string `json:"arch"`
	Capacity int    `json:"capacity"`
	Version  string `json:"version"`
}

// RegisterResp is the POST /register response.
type RegisterResp struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Metrics carries node-level system metrics.
type Metrics struct {
	CPUPercent    float64 `json:"cpu_percent"`
	MemUsedBytes  int64   `json:"mem_used_bytes"`
	MemTotalBytes int64   `json:"mem_total_bytes"`
	UptimeSecs    int64   `json:"uptime_secs"`
}

// TaskProgress is one running task's progress plus a resource sample; it
// doubles as the one-shot agent's sample channel. The disk fields ride
// along so the sample recorded for a task always carries the disk usage
// even when the build ends shortly after the last flush (the final sample
// collides with the last progress sample by timestamp and is dropped by
// the merge, so the fields must already be on the progress payload).
type TaskProgress struct {
	TaskID             string    `json:"task_id"`
	Stage              string    `json:"stage"`
	CPUTimeNS          int64     `json:"cpu_time_ns"`
	MemoryBytes        int64     `json:"memory_bytes"`
	DiskTotalBytes     int64     `json:"disk_total_bytes,omitempty"`
	DiskAvailableBytes int64     `json:"disk_available_bytes,omitempty"`
	DiskUsedBytes      int64     `json:"disk_used_bytes,omitempty"`
	At                 time.Time `json:"at"`
}

// ContainerState describes one container tracked by a host node. The
// controller stores nothing from it; it is informational for the web
// dashboard.
type ContainerState struct {
	TaskID   string `json:"task_id"`
	Status   string `json:"status"`
	ExitCode *int   `json:"exit_code"`
}

// HeartbeatReq is the POST /heartbeat payload.
type HeartbeatReq struct {
	Name       string           `json:"name"`
	Metrics    Metrics          `json:"metrics"`
	Tasks      []TaskProgress   `json:"tasks"`
	Containers []ContainerState `json:"containers"`
}

// HeartbeatResp carries the cancellation signal list and the server
// time.
type HeartbeatResp struct {
	CancelledTaskIDs []string  `json:"cancelled_task_ids"`
	ServerTime       time.Time `json:"server_time"`
}

// PollReq is the POST /poll payload.
type PollReq struct {
	Name string `json:"name"`
	Arch string `json:"arch"`
}

// PollResp carries the claimed task (null when the queue has nothing
// claimable), its claim token and the cancellation signals.
type PollResp struct {
	Task             *TaskDetail `json:"task"`
	ClaimToken       string      `json:"claim_token"`
	CancelledTaskIDs []string    `json:"cancelled_task_ids"`
}

// TaskPackage mirrors the "package" block of TaskDetail.
type TaskPackage struct {
	Pkgbase string `json:"pkgbase"`
	Branch  string `json:"branch"`
	VCSKind string `json:"vcs_kind"`
	Arch    string `json:"arch"`
}

// SourceInfo mirrors the "source" block of TaskDetail: mode is "clone"
// or "archive"; archive carries the staged snapshot path.
type SourceInfo struct {
	Mode    string `json:"mode"`
	URL     string `json:"url"`
	Branch  string `json:"branch"`
	Commit  string `json:"commit"`
	Archive string `json:"archive"`
}

// PkgbuildSource mirrors the optional external PKGBUILD source
// (detect.PkgbuildSource with snake_case JSON).
type PkgbuildSource struct {
	URL       string `json:"url"`
	Branch    string `json:"branch"`
	Directory string `json:"directory"`
}

// HooksInfo mirrors the "hooks" block of TaskDetail.
type HooksInfo struct {
	PreBuild  []string `json:"pre_build"`
	PostBuild []string `json:"post_build"`
	OnSuccess []string `json:"on_success"`
	OnFailure []string `json:"on_failure"`
}

// CollectInfo mirrors the "collect" block of TaskDetail.
type CollectInfo struct {
	Exclude []string `json:"exclude"`
}

// SigningInfo mirrors the "signing" block of TaskDetail.
type SigningInfo struct {
	Required bool   `json:"required"`
	Mode     string `json:"mode"`
}

// BuildInfo mirrors the "build" block of TaskDetail: the per-task timeout
// and the absolute deadline derived from assigned_at + build_timeout.
type BuildInfo struct {
	TimeoutSeconds int64     `json:"timeout_seconds"`
	Deadline       time.Time `json:"deadline"`
}

// TaskDetail is the full task description handed to a worker at claim
// time (every field).
type TaskDetail struct {
	ID             string          `json:"id"`
	Package        TaskPackage     `json:"package"`
	Source         SourceInfo      `json:"source"`
	PkgbuildSource *PkgbuildSource `json:"pkgbuild_source"`
	Hooks          HooksInfo       `json:"hooks"`
	Collect        CollectInfo     `json:"collect"`
	Signing        SigningInfo     `json:"signing"`
	Build          BuildInfo       `json:"build"`
	// Packager is the configured build identity, "Name <email>". The
	// agent injects it as PACKAGER into the build environment; empty
	// means no PACKAGER is set.
	Packager string `json:"packager"`
}

// LogSegment is one buffered log batch. Progress is optional and carries
// the one-shot agent's resource sample.
type LogSegment struct {
	Offset   int64         `json:"offset"`
	Data     string        `json:"data"`
	Progress *TaskProgress `json:"progress"`
}

// LogAck acknowledges a log segment and reports the new offset plus the
// durable cancellation flag.
type LogAck struct {
	Offset    int64 `json:"offset"`
	Cancelled bool  `json:"cancelled"`
}

// ResultError describes a failed build (stage + short summary). Stage
// uses the canonical enumeration, including the controller-side values
// verify/ingest/stalled/timeout/container.
type ResultError struct {
	Stage   string `json:"stage"`
	Summary string `json:"summary"`
}

// ResultReq is the POST result payload. Status is one of "succeeded" /
// "failed" / "cancelled". Commit is the actually checked-out commit;
// when empty the controller falls back to the dispatched source commit.
type ResultReq struct {
	Status        string          `json:"status"`
	Error         *ResultError    `json:"error"`
	Artifacts     []repo.Artifact `json:"artifacts"`
	ResourceUsage []db.Sample     `json:"resource_usage"`
	Commit        string          `json:"commit"`
}

// FileMeta is the upload acknowledgment (name + new offset).
type FileMeta struct {
	Name   string `json:"name"`
	Offset int64  `json:"offset"`
}

// Stats aggregates dashboard data. It is consumed by the web module, not
// serialized on the wire protocol.
type Stats struct {
	QueueLen     int
	ByStatus     map[string]int
	RecentBuilds []db.Build
	Workers      []db.Worker
}
