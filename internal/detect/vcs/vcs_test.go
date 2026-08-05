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

package vcs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// --- pure functions (DETAIL §3.7 #3, #4) ---

func TestDetectKind(t *testing.T) {
	tests := []struct {
		name       string
		pkgbase    string
		pkgnames   []string
		dotfileVCS string
		want       Kind
	}{
		{name: "suffix pkgbase git", pkgbase: "foo-git", dotfileVCS: "auto", want: Git},
		{name: "suffix pkgbase svn", pkgbase: "foo-svn", dotfileVCS: "auto", want: SVN},
		{name: "suffix pkgname git", pkgbase: "foo", pkgnames: []string{"bar-git"}, dotfileVCS: "auto", want: Git},
		{name: "suffix pkgname svn", pkgbase: "foo", pkgnames: []string{"bar-svn"}, dotfileVCS: "auto", want: SVN},
		{name: "pkgbase wins over pkgname", pkgbase: "foo-git", pkgnames: []string{"bar-svn"}, dotfileVCS: "auto", want: Git},
		{name: "no suffix none", pkgbase: "foo", pkgnames: []string{"bar"}, dotfileVCS: "auto", want: None},
		{name: "empty dotfile treated auto", pkgbase: "foo", dotfileVCS: "", want: None},
		{name: "unknown dotfile treated auto", pkgbase: "foo-git", dotfileVCS: "bogus", want: Git},
		{name: "dotfile git overrides suffix", pkgbase: "foo-svn", dotfileVCS: "git", want: Git},
		{name: "dotfile svn overrides suffix", pkgbase: "foo-git", dotfileVCS: "svn", want: SVN},
		{name: "dotfile none overrides suffix", pkgbase: "foo-git", dotfileVCS: "none", want: None},
		{name: "dotfile none no suffix", pkgbase: "foo", dotfileVCS: "none", want: None},
		{name: "dotfile git no suffix", pkgbase: "foo", dotfileVCS: "git", want: Git},
		{name: "dotfile svn no suffix", pkgbase: "foo", dotfileVCS: "svn", want: SVN},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectKind(tt.pkgbase, tt.pkgnames, tt.dotfileVCS); got != tt.want {
				t.Errorf("DetectKind(%q, %v, %q) = %v, want %v", tt.pkgbase, tt.pkgnames, tt.dotfileVCS, got, tt.want)
			}
		})
	}
}

func TestUpstreamURLs(t *testing.T) {
	tests := []struct {
		name   string
		source []string
		want   []string
	}{
		{
			name:   "git+ prefix",
			source: []string{"git+https://github.com/foo/bar.git"},
			want:   []string{"https://github.com/foo/bar.git"},
		},
		{
			name:   "svn+ prefix",
			source: []string{"svn+https://svn.example.org/repo"},
			want:   []string{"https://svn.example.org/repo"},
		},
		{
			name:   ".git suffix",
			source: []string{"https://github.com/foo/bar.git"},
			want:   []string{"https://github.com/foo/bar.git"},
		},
		{
			name:   "plain tarball not a vcs url",
			source: []string{"https://example.org/foo.tar.gz"},
			want:   nil,
		},
		{
			name:   "mixed extracts all hits in order",
			source: []string{"git+https://a.git", "https://b.tar.gz", "svn+https://c", "https://d.git"},
			want:   []string{"https://a.git", "https://c", "https://d.git"},
		},
		{
			name:   "no scheme no suffix",
			source: []string{"https://example.org/foo"},
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UpstreamURLs(tt.source); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("UpstreamURLs(%v) = %v, want %v", tt.source, got, tt.want)
			}
		})
	}
}

// --- GitHead against real local repositories (DETAIL §3.7 #5) ---

func TestGitHeadLocalRepo(t *testing.T) {
	dir := initGitRepo(t, "one")
	head1 := gitHeadOf(t, dir)

	got, err := GitHead(context.Background(), "file://"+dir)
	if err != nil {
		t.Fatalf("GitHead: %v", err)
	}
	if got != head1 {
		t.Errorf("GitHead = %q, want %q", got, head1)
	}

	// A new commit must change the reported HEAD.
	writeFile(t, filepath.Join(dir, "f"), "two")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "two")
	head2 := gitHeadOf(t, dir)

	got2, err := GitHead(context.Background(), "file://"+dir)
	if err != nil {
		t.Fatalf("GitHead after commit: %v", err)
	}
	if got2 != head2 {
		t.Errorf("GitHead after commit = %q, want %q", got2, head2)
	}
	if got2 == head1 {
		t.Error("GitHead did not change after a new commit")
	}
}

func TestGitHeadEmptyRepo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "empty.git")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runGit(t, dir, "init", "--bare")

	got, err := GitHead(context.Background(), "file://"+dir)
	if err != nil {
		t.Fatalf("GitHead on empty repo: %v", err)
	}
	if got != "" {
		t.Errorf("GitHead on empty repo = %q, want empty", got)
	}
}

// --- execCommand recording (DETAIL §3.7 #5, #6) ---

// TestCommandArgs pins the exact external command lines and asserts the
// SVNRevision call carries the --xml flag. The package execCommand var is
// replaced with a script-backed fake (DETAIL §0.3).
func TestCommandArgs(t *testing.T) {
	old := execCommand
	defer func() { execCommand = old }()

	record := filepath.Join(t.TempDir(), "calls")
	body := `
echo "$*" >> '` + record + `'
case "$1" in
  git)
    [ "$2" = "ls-remote" ] || { echo "bad git args: $*" >&2; exit 1; }
    echo "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa	HEAD"
    ;;
  svn)
    [ "$2" = "info" ] && [ "$3" = "--xml" ] || { echo "bad svn args: $*" >&2; exit 1; }
    cat <<'XML'
<?xml version="1.0" encoding="UTF-8"?>
<info>
<entry kind="dir" path="." revision="7">
<url>https://example.org/repo</url>
<repository><root>https://example.org/repo</root><uuid>u</uuid></repository>
<commit revision="7"><author>alice</author><date>2026-01-01T00:00:00.000000Z</date></commit>
</entry>
</info>
XML
    ;;
esac
`
	execCommand = fakeExecScript(t, body)

	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	head, err := GitHead(context.Background(), "https://example.org/repo.git")
	if err != nil {
		t.Fatalf("GitHead: %v", err)
	}
	if head != hash {
		t.Errorf("GitHead = %q, want %q", head, hash)
	}

	rev, err := SVNRevision(context.Background(), "https://example.org/repo")
	if err != nil {
		t.Fatalf("SVNRevision: %v", err)
	}
	if rev != "7" {
		t.Errorf("SVNRevision = %q, want 7", rev)
	}

	want := []string{
		"git ls-remote https://example.org/repo.git HEAD",
		"svn info --xml https://example.org/repo",
	}
	got := readLines(t, record)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recorded commands = %v, want %v", got, want)
	}
}

// TestSVNRevisionPathShim resolves svn through the testdata/bin PATH shim
// (DETAIL §3.7 #6): the shim only accepts "svn info --xml" invocations
// and emits a fixed last-changed-revision.
func TestSVNRevisionPathShim(t *testing.T) {
	shimDir, err := filepath.Abs(filepath.Join("..", "testdata", "bin"))
	if err != nil {
		t.Fatalf("abs shim dir: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	rev, err := SVNRevision(context.Background(), "https://example.org/repo")
	if err != nil {
		t.Fatalf("SVNRevision via shim: %v", err)
	}
	if rev != "41" {
		t.Errorf("SVNRevision via shim = %q, want 41", rev)
	}
}

// --- test helpers ---

// fakeExecScript builds a fake exec.Command constructor backed by a real
// shell script. The script receives the intended command name in $1 and
// its arguments in $2... and may emit canned output or record calls.
func fakeExecScript(t *testing.T, body string) func(ctx context.Context, name string, arg ...string) *exec.Cmd {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakeexec")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake exec script: %v", err)
	}
	return func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, path, append([]string{name}, arg...)...)
		return cmd
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitHeadOf(t *testing.T, dir string) string {
	t.Helper()
	return runGit(t, dir, "rev-parse", "HEAD")
}

func initGitRepo(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.org")
	writeFile(t, filepath.Join(dir, "f"), content)
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "one")
	return dir
}
