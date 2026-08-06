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

package detect

import (
	"context"
	"reflect"
	"testing"
)

// TestListBranches builds a real multi-branch repository, mirrors it and
// asserts enumeration plus exclude_branches glob filtering: "main" and
// "wip/*" are dropped, everything else is kept.
func TestListBranches(t *testing.T) {
	src := newMultiBranchRepo(t, []branchSpec{
		{name: "foo", files: map[string]string{"SRCINFO": srcinfoBody("foo", "1.0", "1")}},
		{name: "bar", files: map[string]string{"SRCINFO": srcinfoBody("bar", "1.0", "1")}},
		{name: "wip/a", files: map[string]string{"SRCINFO": srcinfoBody("wipa", "1.0", "1")}},
		{name: "wip/b", files: map[string]string{"SRCINFO": srcinfoBody("wipb", "1.0", "1")}},
		{name: "main", files: map[string]string{"SRCINFO": srcinfoBody("mainpkg", "1.0", "1")}},
	})
	store, _ := openStore(t)
	d := newTestDetector(t, "file://"+src, store, &fakeSink{})
	d.mirrorDir = cloneMirror(t, src)
	d.cfg.ExcludeBranches = []string{"main", "wip/*"}

	got, err := d.listBranches(context.Background())
	if err != nil {
		t.Fatalf("listBranches: %v", err)
	}
	want := []string{"bar", "foo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listBranches = %v, want %v", got, want)
	}
}

// TestListBranchesDefaultExclude asserts the ["main"] default when the
// configuration leaves exclude_branches unset.
func TestListBranchesDefaultExclude(t *testing.T) {
	src := newMultiBranchRepo(t, []branchSpec{
		{name: "foo", files: map[string]string{"SRCINFO": srcinfoBody("foo", "1.0", "1")}},
		{name: "main", files: map[string]string{"SRCINFO": srcinfoBody("mainpkg", "1.0", "1")}},
	})
	store, _ := openStore(t)
	d := newTestDetector(t, "file://"+src, store, &fakeSink{})
	d.mirrorDir = cloneMirror(t, src)

	got, err := d.listBranches(context.Background())
	if err != nil {
		t.Fatalf("listBranches: %v", err)
	}
	want := []string{"foo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listBranches = %v, want %v", got, want)
	}
}

// TestBranchSnapshot returns the branch HEAD commit from the mirror and
// errors for a branch that does not exist.
func TestBranchSnapshot(t *testing.T) {
	src := newSourceRepo(t, "foo", map[string]string{
		"SRCINFO": srcinfoBody("foo", "1.0", "1"),
	})
	store, _ := openStore(t)
	d := newTestDetector(t, "file://"+src, store, &fakeSink{})
	d.mirrorDir = cloneMirror(t, src)

	want := runGit(t, src, "rev-parse", "refs/heads/foo")
	got, err := d.BranchSnapshot(context.Background(), "foo")
	if err != nil {
		t.Fatalf("BranchSnapshot(foo): %v", err)
	}
	if got != want {
		t.Errorf("BranchSnapshot = %q, want %q", got, want)
	}

	if _, err := d.BranchSnapshot(context.Background(), "nope"); err == nil {
		t.Error("BranchSnapshot(nope) succeeded, want error")
	}
}
