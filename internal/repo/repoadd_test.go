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

package repo

import (
	"context"
	"strings"
	"testing"
)

// TestRepoCommandMatrix asserts the --sign switch and the GNUPGHOME
// environment injection across the three repo.sign modes (DETAIL §6.7
// case 1): only "packages+db" signs the database commands.
func TestRepoCommandMatrix(t *testing.T) {
	cases := []struct {
		name     string
		sign     string
		wantSign bool
	}{
		{"off", "off", false},
		{"packages", "packages", false},
		{"packages+db", "packages+db", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newIngestEnv(t, "local", execCfg{})
			e.cfg.Repo.Sign = tc.sign
			oldPkg := "old-1-1-x86_64.pkg.tar.zst"
			e.seedSidecar(&Sidecar{
				Pkgbase:   testPkgbase,
				Branch:    testPkgbase,
				Artifacts: []Artifact{{File: oldPkg, Kind: "package", Pkgname: "old"}},
			})
			e.seedRoot(oldPkg, "old-pkg")
			e.stage(testManifest())

			if err := e.upd.Ingest(context.Background(), e.task, e.build, "w", testManifest()); err != nil {
				t.Fatalf("Ingest: %v", err)
			}
			execs := e.execLines()
			if len(execs) != 2 {
				t.Fatalf("exec lines = %d, want repo-remove + repo-add: %v", len(execs), execs)
			}
			for _, line := range execs {
				hasSign := strings.Contains(line, " --sign ")
				if hasSign != tc.wantSign {
					t.Errorf("%s: --sign presence = %v, want %v (line %q)", tc.name, hasSign, tc.wantSign, line)
				}
			}
			if got := e.logHas("env GNUPGHOME=/data/gnupg"); got != tc.wantSign {
				t.Errorf("%s: GNUPGHOME env injected = %v, want %v", tc.name, got, tc.wantSign)
			}
			if !strings.Contains(execs[0], "repo-remove "+e.root) || !strings.Contains(execs[0], "old") {
				t.Errorf("first command = %q, want repo-remove of replaced pkgname in local root", execs[0])
			}
			if !strings.Contains(execs[1], "repo-add "+e.root) {
				t.Errorf("second command = %q, want repo-add in local root", execs[1])
			}
		})
	}
}

// TestRepoRemoveAllThenAdd asserts the remove-before-add sequence with two
// replaced pkgnames: every repo-remove runs first, then a single repo-add
// with all new package files (A19, DETAIL §6.7 case 2).
func TestRepoRemoveAllThenAdd(t *testing.T) {
	e := newIngestEnv(t, "local", execCfg{})
	e.seedSidecar(&Sidecar{
		Pkgbase: testPkgbase,
		Branch:  testPkgbase,
		Artifacts: []Artifact{
			{File: "a-1-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "a"},
			{File: "b-1-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "b"},
		},
	})
	e.seedRoot("a-1-1-x86_64.pkg.tar.zst", "a")
	e.seedRoot("b-1-1-x86_64.pkg.tar.zst", "b")
	e.stage(testManifest())

	if err := e.upd.Ingest(context.Background(), e.task, e.build, "w", testManifest()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	execs := e.execLines()
	if len(execs) != 3 {
		t.Fatalf("exec lines = %d, want remove+remove+add: %v", len(execs), execs)
	}
	for _, want := range []string{"repo-remove " + e.root + " " + testDBName + " a", "repo-remove " + e.root + " " + testDBName + " b"} {
		if !strings.Contains(execs[0]+" "+execs[1], want) {
			t.Errorf("missing %q in %q / %q", want, execs[0], execs[1])
		}
	}
	if !strings.Contains(execs[2], "repo-add") {
		t.Errorf("last command = %q, want repo-add", execs[2])
	}
	// repo-add must list every new package file.
	if !strings.Contains(execs[2], testPkgFile) {
		t.Errorf("repo-add args missing new package %s: %q", testPkgFile, execs[2])
	}
}

// TestRepoCommandNonZeroExit asserts a failed repo-add surfaces an error
// whose summary carries the tail of stderr (last 200 characters, DETAIL
// §6.4 step 4).
func TestRepoCommandNonZeroExit(t *testing.T) {
	head := "HEAD_MARKER_"
	tail := "TAIL_MARKER"
	e := newIngestEnv(t, "local", execCfg{
		exits:  map[string]int{"repo-add": 1},
		stderr: map[string]string{"repo-add": head + strings.Repeat("x", 300) + tail},
	})
	e.stage(testManifest())
	err := e.upd.Ingest(context.Background(), e.task, e.build, "w", testManifest())
	if err == nil {
		t.Fatal("Ingest with failing repo-add: want error, got nil")
	}
	if !strings.Contains(err.Error(), "repo-add") {
		t.Errorf("error must name the command, got %v", err)
	}
	if !strings.Contains(err.Error(), tail) {
		t.Errorf("error must carry the stderr tail, got %v", err)
	}
	if strings.Contains(err.Error(), head) {
		t.Errorf("error summary must be limited to the stderr tail, got %v", err)
	}
}

// TestRepoRemoveNonZeroExit asserts a failed repo-remove (replaced pkgname)
// aborts the ingest.
func TestRepoRemoveNonZeroExit(t *testing.T) {
	e := newIngestEnv(t, "local", execCfg{
		exits:  map[string]int{"repo-remove": 2},
		stderr: map[string]string{"repo-remove": "cannot remove package"},
	})
	e.seedSidecar(&Sidecar{
		Pkgbase:   testPkgbase,
		Branch:    testPkgbase,
		Artifacts: []Artifact{{File: "old-1-1-x86_64.pkg.tar.zst", Kind: "package", Pkgname: "old"}},
	})
	e.seedRoot("old-1-1-x86_64.pkg.tar.zst", "old")
	e.stage(testManifest())
	err := e.upd.Ingest(context.Background(), e.task, e.build, "w", testManifest())
	if err == nil {
		t.Fatal("Ingest with failing repo-remove: want error, got nil")
	}
	if !strings.Contains(err.Error(), "repo-remove") {
		t.Errorf("error must name the command, got %v", err)
	}
	// The add must not run after the failed remove.
	execs := e.execLines()
	if len(execs) != 1 {
		t.Errorf("exec lines = %d, want only the failed repo-remove: %v", len(execs), execs)
	}
}
