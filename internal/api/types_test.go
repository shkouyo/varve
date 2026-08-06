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
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/dispatch"
	"git.0x0f.dev/varve/internal/repo"
	"git.0x0f.dev/varve/internal/sign"
)

// Compile-time: every worker-protocol type is re-exported by alias, so
// worker packages get the whole surface from api alone. The assignments
// are tautological by construction but pin the aliases: dropping one
// breaks the build.
var (
	_ RegisterReq    = dispatch.RegisterReq{}
	_ RegisterResp   = dispatch.RegisterResp{}
	_ HeartbeatReq   = dispatch.HeartbeatReq{}
	_ HeartbeatResp  = dispatch.HeartbeatResp{}
	_ PollReq        = dispatch.PollReq{}
	_ PollResp       = dispatch.PollResp{}
	_ TaskDetail     = dispatch.TaskDetail{}
	_ LogSegment     = dispatch.LogSegment{}
	_ LogAck         = dispatch.LogAck{}
	_ ResultReq      = dispatch.ResultReq{}
	_ ResultError    = dispatch.ResultError{}
	_ FileMeta       = dispatch.FileMeta{}
	_ Metrics        = dispatch.Metrics{}
	_ TaskProgress   = dispatch.TaskProgress{}
	_ ContainerState = dispatch.ContainerState{}
	_ KeyMaterial    = sign.KeyMaterial{}
)

// fullTaskDetail is the golden task with every field set.
func fullTaskDetail() *TaskDetail {
	return &TaskDetail{
		ID: "task-0001",
		Package: dispatch.TaskPackage{
			Pkgbase: "foo",
			Branch:  "foo",
			VCSKind: "git",
			Arch:    "x86_64",
		},
		Source: dispatch.SourceInfo{
			Mode:    "clone",
			URL:     "git@example.com:pkgbuilds.git",
			Branch:  "foo",
			Commit:  "abc123",
			Archive: "",
		},
		PkgbuildSource: nil,
		Hooks: dispatch.HooksInfo{
			PreBuild:  []string{"scripts/pre.sh"},
			PostBuild: []string{"scripts/post.sh"},
		},
		Collect: dispatch.CollectInfo{Exclude: []string{"*-debug"}},
		Signing: dispatch.SigningInfo{Required: true, Mode: "packages"},
		Build: dispatch.BuildInfo{
			TimeoutSeconds: 1800,
			Deadline:       time.Date(2026, 8, 5, 10, 30, 0, 0, time.UTC),
		},
	}
}

// TestTypesJSONGolden pins the snake_case wire encoding of the protocol
// types byte-for-byte: marshal each value and compare with the golden
// JSON, then unmarshal the golden back and compare with the original
// value.
func TestTypesJSONGolden(t *testing.T) {
	at := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		value  any
		golden string
	}{
		{
			name: "register",
			value: RegisterReq{
				Name: "proud-heron-7", Role: "host", Mode: "host",
				Arch: "x86_64", Capacity: 2, Version: "0.1.0",
			},
			golden: `{"name":"proud-heron-7","role":"host","mode":"host","arch":"x86_64","capacity":2,"version":"0.1.0"}`,
		},
		{
			name:   "register response",
			value:  RegisterResp{ID: 12, Name: "proud-heron-7"},
			golden: `{"id":12,"name":"proud-heron-7"}`,
		},
		{
			name: "heartbeat",
			value: HeartbeatReq{
				Name: "proud-heron-7",
				Metrics: Metrics{
					CPUPercent: 12.5, MemUsedBytes: 5368709120,
					MemTotalBytes: 17179869184, UptimeSecs: 302400,
				},
				Tasks: []TaskProgress{{
					TaskID: "t-1", Stage: "makepkg",
					CPUTimeNS: 123456789, MemoryBytes: 104857600, At: at,
				}},
				Containers: []ContainerState{{TaskID: "t-1", Status: "running"}},
			},
			golden: `{"name":"proud-heron-7","metrics":{"cpu_percent":12.5,"mem_used_bytes":5368709120,"mem_total_bytes":17179869184,"uptime_secs":302400},"tasks":[{"task_id":"t-1","stage":"makepkg","cpu_time_ns":123456789,"memory_bytes":104857600,"at":"2026-08-05T10:00:00Z"}],"containers":[{"task_id":"t-1","status":"running","exit_code":null}]}`,
		},
		{
			name:   "heartbeat response",
			value:  HeartbeatResp{CancelledTaskIDs: []string{"t-2"}, ServerTime: at},
			golden: `{"cancelled_task_ids":["t-2"],"server_time":"2026-08-05T10:00:00Z"}`,
		},
		{
			name:   "poll request",
			value:  PollReq{Name: "proud-heron-7", Arch: "x86_64"},
			golden: `{"name":"proud-heron-7","arch":"x86_64"}`,
		},
		{
			name: "poll response with task",
			value: PollResp{
				Task:       fullTaskDetail(),
				ClaimToken: "a1b2c3",
			},
			golden: `{"task":` + goldenTaskDetailJSON + `,"claim_token":"a1b2c3","cancelled_task_ids":null}`,
		},
		{
			name: "poll response without task",
			value: PollResp{
				Task:       nil,
				ClaimToken: "",
			},
			golden: `{"task":null,"claim_token":"","cancelled_task_ids":null}`,
		},
		{
			name:   "task detail full",
			value:  fullTaskDetail(),
			golden: goldenTaskDetailJSON,
		},
		{
			name: "log segment",
			value: LogSegment{
				Offset: 4096,
				Data:   "==> Making package\n",
				Progress: &TaskProgress{
					TaskID: "t-1", Stage: "makepkg", CPUTimeNS: 1, MemoryBytes: 2, At: at,
				},
			},
			golden: `{"offset":4096,"data":"==\u003e Making package\n","progress":{"task_id":"t-1","stage":"makepkg","cpu_time_ns":1,"memory_bytes":2,"at":"2026-08-05T10:00:00Z"}}`,
		},
		{
			name:   "log ack",
			value:  LogAck{Offset: 8192, Cancelled: true},
			golden: `{"offset":8192,"cancelled":true}`,
		},
		{
			name: "result",
			value: ResultReq{
				Status: "succeeded",
				Error:  nil,
				Artifacts: []repo.Artifact{{
					File: "foo-1.2.3-1-x86_64.pkg.tar.zst", Kind: "package",
					Pkgname: "foo", Version: "1.2.3-1", Arch: "x86_64",
					Size: 123456, SHA256: "deadbeef",
				}},
				ResourceUsage: []db.Sample{{At: at, CPUTimeNS: 1, MemoryBytes: 2}},
				Commit:        "abc123",
			},
			golden: `{"status":"succeeded","error":null,"artifacts":[{"file":"foo-1.2.3-1-x86_64.pkg.tar.zst","kind":"package","pkgname":"foo","version":"1.2.3-1","arch":"x86_64","size":123456,"sha256":"deadbeef"}],"resource_usage":[{"at":"2026-08-05T10:00:00Z","cpu_time_ns":1,"memory_bytes":2}],"commit":"abc123"}`,
		},
		{
			name:   "result error",
			value:  ResultError{Stage: "makepkg", Summary: "build failed"},
			golden: `{"stage":"makepkg","summary":"build failed"}`,
		},
		{
			name:   "file meta",
			value:  FileMeta{Name: "foo-1.2.3-1-x86_64.pkg.tar.zst", Offset: 123456},
			golden: `{"name":"foo-1.2.3-1-x86_64.pkg.tar.zst","offset":123456}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.golden {
				t.Errorf("marshal mismatch\n got: %s\nwant: %s", got, tc.golden)
			}
			// Round-trip: the golden decodes back into an equal value.
			back := reflect.New(reflect.TypeOf(tc.value)).Interface()
			if err := json.Unmarshal([]byte(tc.golden), back); err != nil {
				t.Fatalf("unmarshal golden: %v", err)
			}
			if !reflect.DeepEqual(reflect.ValueOf(back).Elem().Interface(), tc.value) {
				t.Errorf("round-trip mismatch: %#v", back)
			}
		})
	}
}

// TestKeyMaterialAlias pins the sign.KeyMaterial re-export: the alias is
// the identical type, so a sign.KeyMaterial is usable as an api type.
func TestKeyMaterialAlias(t *testing.T) {
	km := &KeyMaterial{
		KeyID:             "ABCD1234",
		ArmoredPrivateKey: "-----BEGIN PGP PRIVATE KEY BLOCK-----",
		Passphrase:        "secret",
	}
	var viaSign *sign.KeyMaterial = km
	if viaSign.KeyID != "ABCD1234" {
		t.Fatalf("alias mismatch: %#v", viaSign)
	}
}

// goldenTaskDetailJSON is the golden encoding of fullTaskDetail (every
// field, snake_case, RFC3339 UTC timestamps).
const goldenTaskDetailJSON = `{"id":"task-0001","package":{"pkgbase":"foo","branch":"foo","vcs_kind":"git","arch":"x86_64"},"source":{"mode":"clone","url":"git@example.com:pkgbuilds.git","branch":"foo","commit":"abc123","archive":""},"pkgbuild_source":null,"hooks":{"pre_build":["scripts/pre.sh"],"post_build":["scripts/post.sh"],"on_success":null,"on_failure":null},"collect":{"exclude":["*-debug"]},"signing":{"required":true,"mode":"packages"},"build":{"timeout_seconds":1800,"deadline":"2026-08-05T10:30:00Z"}}`
