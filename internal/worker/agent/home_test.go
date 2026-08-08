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

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// envValue returns the value of key in env, or "" when absent.
func envValue(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if len(e) > len(prefix) && e[:len(prefix)] == prefix {
			return e[len(prefix):]
		}
	}
	return ""
}

// assertWritableHomeDir checks that dir exists as a private directory.
func assertWritableHomeDir(t *testing.T, dir string) {
	t.Helper()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("home dir %s: %v", dir, err)
	}
	if !fi.IsDir() {
		t.Fatalf("home dir %s is not a directory", dir)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("home dir %s mode = %o, want 700", dir, got)
	}
}

// TestChildEnvWritableHomeUnchanged asserts that an already-writable HOME
// is left untouched: childEnv returns nil and children inherit.
func TestChildEnvWritableHomeUnchanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := NewRunner(configForTest(t, true), &fakeClient{})
	if env := r.childEnv(); env != nil {
		t.Fatalf("childEnv = %v, want nil (inherit)", env)
	}
}

// TestChildEnvEmptyHome sets a writable replacement dir.
func TestChildEnvEmptyHome(t *testing.T) {
	t.Setenv("HOME", "")
	r := NewRunner(configForTest(t, true), &fakeClient{})
	env := r.childEnv()
	if env == nil {
		t.Fatal("childEnv = nil, want a replacement environment")
	}
	want := filepath.Join(r.workDir, "home")
	if got := envValue(env, "HOME"); got != want {
		t.Errorf("HOME = %q, want %q", got, want)
	}
	assertWritableHomeDir(t, want)
}

// TestChildEnvUnusableHome asserts that a HOME pointing at a non-directory
// is replaced. The probe rejects it regardless of the effective uid.
func TestChildEnvUnusableHome(t *testing.T) {
	notHome := filepath.Join(t.TempDir(), "home-file")
	if err := os.WriteFile(notHome, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", notHome)
	r := NewRunner(configForTest(t, true), &fakeClient{})
	env := r.childEnv()
	if env == nil {
		t.Fatal("childEnv = nil, want a replacement environment")
	}
	want := filepath.Join(r.workDir, "home")
	if got := envValue(env, "HOME"); got != want {
		t.Errorf("HOME = %q, want %q", got, want)
	}
	assertWritableHomeDir(t, want)
}

// TestChildEnvUnwritableHome asserts that a HOME the user cannot write
// into is replaced.
func TestChildEnvUnwritableHome(t *testing.T) {
	if os.Getenv("VARVE_TEST_SKIP_PERM") != "" {
		t.Skip("VARVE_TEST_SKIP_PERM set: permission-dependent behavior is not exercised")
	}
	if os.Geteuid() == 0 {
		t.Skip("the writability probe bypasses permissions for root")
	}
	locked := t.TempDir()
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", locked)
	r := NewRunner(configForTest(t, true), &fakeClient{})
	env := r.childEnv()
	if env == nil {
		t.Fatal("childEnv = nil, want a replacement environment")
	}
	if got := envValue(env, "HOME"); got != filepath.Join(r.workDir, "home") {
		t.Errorf("HOME = %q, want %q", got, filepath.Join(r.workDir, "home"))
	}
}

// TestChildEnvResolvedOnce asserts the resolution happens a single time
// per runner and the same environment is served to every command.
func TestChildEnvResolvedOnce(t *testing.T) {
	t.Setenv("HOME", "")
	r := NewRunner(configForTest(t, true), &fakeClient{})
	first := r.childEnv()
	second := r.childEnv()
	if first == nil || &first[0] != &second[0] {
		t.Fatalf("childEnv not cached: %v vs %v", first, second)
	}
}

// TestCommandEnvReplaced asserts the recorder sees a command whose HOME
// points at the created writable directory when the inherited HOME is
// unusable.
func TestCommandEnvReplaced(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "gh-home")) // missing -> unusable
	r := NewRunner(configForTest(t, true), &fakeClient{})
	fe := newFakeExec()
	r.execCommand = fe.command
	cmd := r.command(context.Background(), "git", "clone", "x")
	want := filepath.Join(r.workDir, "home")
	if got := envValue(cmd.Env, "HOME"); got != want {
		t.Errorf("cmd HOME = %q, want %q", got, want)
	}
	assertWritableHomeDir(t, want)
	if got := envValue(fe.callEnv("git"), "HOME"); got != want {
		t.Errorf("recorded cmd HOME = %q, want %q", got, want)
	}
}

// TestCommandEnvInherited asserts that a usable HOME leaves cmd.Env nil,
// so children inherit the agent's environment exactly as before.
func TestCommandEnvInherited(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := NewRunner(configForTest(t, true), &fakeClient{})
	fe := newFakeExec()
	r.execCommand = fe.command
	cmd := r.command(context.Background(), "git", "clone", "x")
	if cmd.Env != nil {
		t.Errorf("cmd.Env = %v, want nil (inherit)", cmd.Env)
	}
	if env := fe.callEnv("git"); env != nil {
		t.Errorf("recorded env = %v, want nil (inherit)", env)
	}
}

// TestFlowCommandsGetWritableHome runs a full one-shot task with an
// unusable HOME and asserts every build command carries the replaced
// HOME in its environment.
func TestFlowCommandsGetWritableHome(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1")}
	r := runOneShotRunner(t, f)
	exec := flowExec(t, r.workDir, "t-1", testSrcinfo,
		[]string{"foo-1.0-1-x86_64.pkg.tar.zst"}, nil)
	r.execCommand = exec.command
	r.logInterval = 20 * time.Millisecond
	t.Setenv("HOME", filepath.Join(t.TempDir(), "github-home")) // unusable
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := filepath.Join(r.workDir, "home")
	assertWritableHomeDir(t, want)
	for _, name := range []string{"git", "makepkg"} {
		if got := envValue(exec.callEnv(name), "HOME"); got != want {
			t.Errorf("%s env HOME = %q, want %q", name, got, want)
		}
	}
}

// TestFlowCommandsInheritHome runs the same flow with a usable HOME and
// asserts the commands inherit the environment unchanged.
func TestFlowCommandsInheritHome(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1")}
	r := runOneShotRunner(t, f)
	exec := flowExec(t, r.workDir, "t-1", testSrcinfo,
		[]string{"foo-1.0-1-x86_64.pkg.tar.zst"}, nil)
	r.execCommand = exec.command
	r.logInterval = 20 * time.Millisecond
	t.Setenv("HOME", t.TempDir())
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, name := range []string{"git", "makepkg"} {
		if env := exec.callEnv(name); env != nil {
			t.Errorf("%s env = %v, want nil (inherit)", name, env)
		}
	}
}

// TestWithEnv asserts replacement, append and nil-base behavior.
func TestWithEnv(t *testing.T) {
	base := []string{"A=1", "HOME=/old"}
	out := withEnv(base, "HOME", "/new")
	if got := envValue(out, "HOME"); got != "/new" {
		t.Errorf("replaced HOME = %q, want /new", got)
	}
	if got := envValue(base, "HOME"); got != "/old" {
		t.Errorf("withEnv mutated its input: %v", base)
	}
	if got := envValue(withEnv([]string{"A=1"}, "B", "2"), "B"); got != "2" {
		t.Errorf("withEnv did not append B")
	}
	if got := envValue(withEnv(nil, "X", "1"), "X"); got != "1" {
		t.Errorf("withEnv(nil) base: X = %q, want 1", got)
	}
}
