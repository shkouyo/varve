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
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestEnsureMirrorCloneThenFetch pins the exact mirror maintenance command
// lines: "git clone --mirror" on first use and "git fetch origin
// +refs/heads/*:refs/heads/* --prune" afterwards (DETAIL §3.4 #1).
func TestEnsureMirrorCloneThenFetch(t *testing.T) {
	record := filepath.Join(t.TempDir(), "calls")
	script := "echo \"$*\" >> '" + record + "'\n"
	store, _ := openStore(t)
	d := newTestDetector(t, "git@git.example.org:pkgbuilds.git", store, &fakeSink{})
	d.execCommand = fakeExecScript(t, script)

	if err := d.ensureMirror(context.Background()); err != nil {
		t.Fatalf("ensureMirror (clone): %v", err)
	}
	// Simulate the successful clone having created the mirror directory.
	if err := os.MkdirAll(d.mirrorDir, 0o755); err != nil {
		t.Fatalf("mkdir mirror: %v", err)
	}
	if err := d.ensureMirror(context.Background()); err != nil {
		t.Fatalf("ensureMirror (fetch): %v", err)
	}

	got := readLines(t, record)
	want := []string{
		"git clone --mirror git@git.example.org:pkgbuilds.git " + d.mirrorDir,
		"git -C " + d.mirrorDir + " fetch origin +refs/heads/*:refs/heads/* --prune",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mirror commands = %v, want %v", got, want)
	}
}

// TestEnsureMirrorCloneFailure asserts a failed initial clone is an error
// (the Run loop logs it and retries on the next round).
func TestEnsureMirrorCloneFailure(t *testing.T) {
	store, _ := openStore(t)
	d := newTestDetector(t, "git@git.example.org:pkgbuilds.git", store, &fakeSink{})
	d.execCommand = fakeExecScript(t, `echo "boom" >&2; exit 1`)

	if err := d.ensureMirror(context.Background()); err == nil {
		t.Fatal("ensureMirror with failing clone succeeded, want error")
	}
}

// TestEnsureMirrorFetchFailure asserts a failed fetch is an error.
func TestEnsureMirrorFetchFailure(t *testing.T) {
	store, _ := openStore(t)
	d := newTestDetector(t, "git@git.example.org:pkgbuilds.git", store, &fakeSink{})
	if err := os.MkdirAll(d.mirrorDir, 0o755); err != nil {
		t.Fatalf("mkdir mirror: %v", err)
	}
	d.execCommand = fakeExecScript(t, `echo "boom" >&2; exit 1`)

	if err := d.ensureMirror(context.Background()); err == nil {
		t.Fatal("ensureMirror with failing fetch succeeded, want error")
	}
}

// TestMirrorTimeout asserts the 60s mirror command timeout fallback: a
// hung command is killed when the deadline passes (DETAIL §3.4 #1).
func TestMirrorTimeout(t *testing.T) {
	if mirrorTimeout != 60*time.Second {
		t.Errorf("mirrorTimeout = %v, want 60s", mirrorTimeout)
	}
	old := mirrorTimeout
	mirrorTimeout = 150 * time.Millisecond
	defer func() { mirrorTimeout = old }()

	store, _ := openStore(t)
	d := newTestDetector(t, "git@git.example.org:pkgbuilds.git", store, &fakeSink{})
	d.execCommand = fakeExecScript(t, "sleep 5")

	if err := d.ensureMirror(context.Background()); err == nil {
		t.Fatal("ensureMirror with hung clone succeeded, want timeout error")
	}
}
