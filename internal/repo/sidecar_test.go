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

package repo

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// sampleSidecar mirrors the DESIGN §3.2 / PROPOSAL §11.2 example with every
// field populated, including a package, a detached signature and the
// .SRCINFO snapshot entry.
func sampleSidecar() *Sidecar {
	return &Sidecar{
		Pkgbase: "foo",
		Branch:  "foo",
		VCS:     "git",
		Artifacts: []Artifact{
			{File: "foo-1.2.3-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "foo", Version: "1.2.3-1", Arch: "x86_64", Size: 123456, SHA256: "aaaa"},
			{File: "foo-1.2.3-1-x86_64.pkg.tar.zst.sig", Kind: "signature", Size: 566, SHA256: "bbbb"},
			{File: ".SRCINFO", Kind: "srcinfo", Size: 321, SHA256: "cccc"},
		},
		Build: BuildInfo{
			Commit:      "abc123",
			UpstreamRef: "9f8e7d6",
			SrcinfoHash: "cccc",
			Time:        time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
			Worker:      "proud-heron-7",
		},
	}
}

// TestSidecarRoundTrip asserts that serialization and deserialization are
// mutually inverse: every field of a fully populated side file survives the
// go-toml v2 round trip (DETAIL §6.7 case 3, T6.1).
func TestSidecarRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		sc   *Sidecar
	}{
		{"full", sampleSidecar()},
		{"empty-build", &Sidecar{Pkgbase: "bare", Branch: "bare", Artifacts: []Artifact{{File: "bare-1-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "bare", Version: "1-1", Arch: "x86_64"}}}},
		{"no-artifacts", &Sidecar{Pkgbase: "n", Branch: "n", Artifacts: []Artifact{}}},
		{"nanosecond-time", &Sidecar{Pkgbase: "t", Branch: "t", Artifacts: []Artifact{}, Build: BuildInfo{Time: time.Date(2026, 8, 5, 1, 2, 3, 123456789, time.UTC)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := MarshalSidecar(tc.sc)
			if err != nil {
				t.Fatalf("MarshalSidecar: %v", err)
			}
			got, err := ParseSidecar(data)
			if err != nil {
				t.Fatalf("ParseSidecar: %v\n%s", err, data)
			}
			if !reflect.DeepEqual(got, tc.sc) {
				t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, tc.sc)
			}
		})
	}
}

// TestSidecarShape asserts the TOML structure matches DESIGN §3.2: lowercase
// keys, a [[artifacts]] table array, a [build] table and the build time
// serialized as an RFC3339 value.
func TestSidecarShape(t *testing.T) {
	data, err := MarshalSidecar(sampleSidecar())
	if err != nil {
		t.Fatalf("MarshalSidecar: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"pkgbase = ",
		"branch = ",
		"vcs = ",
		"[[artifacts]]",
		"[build]",
		"file = ",
		"pkgname = ",
		"version = ",
		"arch = ",
		"size = ",
		"sha256 = ",
		"commit = ",
		"upstream_ref = ",
		"srcinfo_hash = ",
		"worker = ",
		"2026-08-04T12:00:00Z",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("marshaled sidecar missing %q:\n%s", want, text)
		}
	}
}

// TestParseSidecarErrors asserts malformed and unknown-key input is refused
// (T6.1: damaged TOML -> error).
func TestParseSidecarErrors(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"not-toml", "pkgbase = "},
		{"garbage", "not toml at all ["},
		{"unknown-key", "pkgbase = \"x\"\nno_such_key = 1\n"},
		{"bad-build", "pkgbase = \"x\"\n[build]\ncommit = 42\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseSidecar([]byte(tc.data)); err == nil {
				t.Errorf("ParseSidecar(%q): want error, got nil", tc.data)
			}
		})
	}
}

// TestMarshalSidecarNil guards the nil receiver contract.
func TestMarshalSidecarNil(t *testing.T) {
	if _, err := MarshalSidecar(nil); err == nil {
		t.Error("MarshalSidecar(nil): want error, got nil")
	}
}
