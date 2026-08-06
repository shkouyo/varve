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

package api

import (
	"git.0x0f.dev/varve/internal/dispatch"
	"git.0x0f.dev/varve/internal/sign"
)

// The worker protocol types are defined in dispatch and re-exported here
// by alias: worker packages depend only on api, while the JSON wire
// contract stays snake_case. KeyMaterial is likewise re-exported from
// sign so worker packages never import sign directly.

type (
	// RegisterReq is the POST /register payload.
	RegisterReq = dispatch.RegisterReq
	// RegisterResp is the POST /register response.
	RegisterResp = dispatch.RegisterResp
	// HeartbeatReq is the POST /heartbeat payload.
	HeartbeatReq = dispatch.HeartbeatReq
	// HeartbeatResp is the POST /heartbeat response (carries the
	// controller's cancellation signals).
	HeartbeatResp = dispatch.HeartbeatResp
	// PollReq is the POST /poll payload.
	PollReq = dispatch.PollReq
	// PollResp is the POST /poll response.
	PollResp = dispatch.PollResp
	// TaskDetail is the full task description handed to a worker
	// (every field of the wire contract).
	TaskDetail = dispatch.TaskDetail
	// LogSegment is one buffered log batch.
	LogSegment = dispatch.LogSegment
	// LogAck acknowledges a log segment (offset + durable cancel flag).
	LogAck = dispatch.LogAck
	// ResultReq is the POST result payload.
	ResultReq = dispatch.ResultReq
	// ResultError describes a failed build (stage + short summary).
	ResultError = dispatch.ResultError
	// FileMeta is the upload acknowledgment (name + new offset).
	FileMeta = dispatch.FileMeta
	// Metrics carries node-level system metrics.
	Metrics = dispatch.Metrics
	// TaskProgress is one running task's progress plus a resource sample.
	TaskProgress = dispatch.TaskProgress
	// ContainerState describes one container tracked by a host node.
	ContainerState = dispatch.ContainerState
	// KeyMaterial is the one-shot signing key material (sign package;
	// workers depend on api, not sign).
	KeyMaterial = sign.KeyMaterial
)
