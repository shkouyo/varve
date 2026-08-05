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

package dispatch

import (
	"encoding/json"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/repo"
)

// TestTaskDetailJSONGolden asserts the wire encoding of a fully populated
// TaskDetail matches DESIGN §5.4 field-for-field (snake_case keys, every
// field present).
func TestTaskDetailJSONGolden(t *testing.T) {
	d := TaskDetail{
		ID:      "550e8400-e29b-41d4-a716-446655440000",
		Package: TaskPackage{Pkgbase: "foo", Branch: "foo", VCSKind: "git", Arch: "x86_64"},
		Source: SourceInfo{
			Mode:    "clone",
			URL:     "git@git.example.org:pkgbuilds.git",
			Branch:  "foo",
			Commit:  "abc123def456",
			Archive: "",
		},
		Hooks: HooksInfo{
			PreBuild:  []string{"scripts/pre.sh"},
			PostBuild: []string{"scripts/post.sh"},
			OnSuccess: []string{"scripts/ok.sh"},
			OnFailure: []string{"scripts/fail.sh"},
		},
		Collect: CollectInfo{Exclude: []string{"*-debug"}},
		Signing: SigningInfo{Required: true, Mode: "packages"},
		Build: BuildInfo{
			TimeoutSeconds: 1800,
			Deadline:       time.Date(2026, 8, 5, 10, 30, 0, 0, time.UTC),
		},
	}
	got, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"id":"550e8400-e29b-41d4-a716-446655440000",` +
		`"package":{"pkgbase":"foo","branch":"foo","vcs_kind":"git","arch":"x86_64"},` +
		`"source":{"mode":"clone","url":"git@git.example.org:pkgbuilds.git","branch":"foo","commit":"abc123def456","archive":""},` +
		`"pkgbuild_source":null,` +
		`"hooks":{"pre_build":["scripts/pre.sh"],"post_build":["scripts/post.sh"],"on_success":["scripts/ok.sh"],"on_failure":["scripts/fail.sh"]},` +
		`"collect":{"exclude":["*-debug"]},` +
		`"signing":{"required":true,"mode":"packages"},` +
		`"build":{"timeout_seconds":1800,"deadline":"2026-08-05T10:30:00Z"}}`
	if string(got) != want {
		t.Errorf("TaskDetail JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

// TestProtocolRoundTrip covers JSON round-trips of the worker protocol
// types against their snake_case field names (DETAIL §0.3 rule 8).
func TestProtocolRoundTrip(t *testing.T) {
	reg := RegisterReq{Name: "proud-heron-7", Role: "host", Mode: "host", Arch: "x86_64", Capacity: 2, Version: "0.1.0"}
	assertJSON(t, reg, `{"name":"proud-heron-7","role":"host","mode":"host","arch":"x86_64","capacity":2,"version":"0.1.0"}`)

	hb := HeartbeatReq{
		Name:    "proud-heron-7",
		Metrics: Metrics{CPUPercent: 12.5, MemUsedBytes: 5368709120, MemTotalBytes: 17179869184, UptimeSecs: 302400},
		Tasks: []TaskProgress{{
			TaskID:      "550e8400-e29b-41d4-a716-446655440000",
			Stage:       "makepkg",
			CPUTimeNS:   123456789,
			MemoryBytes: 104857600,
			At:          time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
		}},
	}
	assertJSON(t, hb, `{"name":"proud-heron-7",`+
		`"metrics":{"cpu_percent":12.5,"mem_used_bytes":5368709120,"mem_total_bytes":17179869184,"uptime_secs":302400},`+
		`"tasks":[{"task_id":"550e8400-e29b-41d4-a716-446655440000","stage":"makepkg","cpu_time_ns":123456789,"memory_bytes":104857600,"at":"2026-08-05T10:00:00Z"}],`+
		`"containers":null}`)

	seg := LogSegment{Offset: 4096, Data: "==> Making package…\n", Progress: &TaskProgress{TaskID: "t1", Stage: "prepare", At: time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)}}
	assertJSON(t, seg, `{"offset":4096,"data":"==\u003e Making package…\n","progress":{"task_id":"t1","stage":"prepare","cpu_time_ns":0,"memory_bytes":0,"at":"2026-08-05T09:00:00Z"}}`)

	res := ResultReq{Status: "succeeded", Commit: "deadbeef",
		Artifacts: []repo.Artifact{{File: "foo-1.2.3-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "foo", Version: "1.2.3-1", Arch: "x86_64", Size: 123456, SHA256: "abc"}}}
	assertJSON(t, res, `{"status":"succeeded","error":null,`+
		`"artifacts":[{"file":"foo-1.2.3-1-x86_64.pkg.tar.zst","kind":"package","pkgname":"foo","version":"1.2.3-1","arch":"x86_64","size":123456,"sha256":"abc"}],`+
		`"resource_usage":null,"commit":"deadbeef"}`)
}

func assertJSON(t *testing.T, v any, want string) {
	t.Helper()
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != want {
		t.Errorf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}
