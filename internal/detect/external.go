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
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"git.0x0f.dev/varve/internal/detect/vcs"
)

// ExecFn builds one external command. The detector and the dispatch module
// each inject their command constructor so tests can record or fake it.
type ExecFn func(ctx context.Context, name string, arg ...string) *exec.Cmd

// defaultPkgbuildBranch is the branch assumed when a pkgbuild_source
// dotfile omits the branch key.
const defaultPkgbuildBranch = "master"

// pkgbuildRepoTimeout bounds every external pkgbuild_source command so a
// hung network never wedges the poll loop.
const pkgbuildRepoTimeout = 60 * time.Second

// PkgbuildHead resolves the current head commit of a pkgbuild_source
// repository branch via "git ls-remote <url> refs/heads/<branch>". A
// missing branch or a failed lookup is an error so the caller can skip the
// branch instead of building from a stale snapshot.
func PkgbuildHead(ctx context.Context, execFn ExecFn, fetchKey string, src PkgbuildSource) (string, error) {
	branch := src.Branch
	if branch == "" {
		branch = defaultPkgbuildBranch
	}
	if err := vcs.ValidateRepoURL(src.URL); err != nil {
		return "", fmt.Errorf("detect: invalid pkgbuild source url %q: %w", src.URL, err)
	}
	cctx, cancel := context.WithTimeout(ctx, pkgbuildRepoTimeout)
	defer cancel()
	cmd := execFn(cctx, "git", "ls-remote", "--", src.URL, "refs/heads/"+branch)
	if env := pkgbuildEnv(fetchKey); env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("detect: ls-remote pkgbuild source %s: %w: %s", src.URL, err, strings.TrimSpace(string(out)))
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", fmt.Errorf("detect: pkgbuild source %s has no branch %q", src.URL, branch)
	}
	return strings.Fields(line)[0], nil
}

// PkgbuildFile reads one file from a pkgbuild_source repository: a shallow
// single-branch clone into a temp directory, then the file under the
// optional Directory prefix. The temp directory is removed before
// returning. fetchKey, when it names an existing file, authenticates the
// clone through GIT_SSH_COMMAND (the same identity the source mirror uses).
func PkgbuildFile(ctx context.Context, execFn ExecFn, fetchKey string, src PkgbuildSource, name string) ([]byte, error) {
	if err := vcs.ValidateRepoURL(src.URL); err != nil {
		return nil, fmt.Errorf("detect: invalid pkgbuild source url %q: %w", src.URL, err)
	}
	dir, err := os.MkdirTemp("", "varve-pkgbuild-*")
	if err != nil {
		return nil, fmt.Errorf("detect: pkgbuild source temp dir: %w", err)
	}
	defer os.RemoveAll(dir)
	branch := src.Branch
	if branch == "" {
		branch = defaultPkgbuildBranch
	}
	cctx, cancel := context.WithTimeout(ctx, pkgbuildRepoTimeout)
	defer cancel()
	cmd := execFn(cctx, "git", "clone", "--depth", "1", "--branch", branch, "--", src.URL, dir)
	if env := pkgbuildEnv(fetchKey); env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("detect: clone pkgbuild source %s: %w: %s", src.URL, err, strings.TrimSpace(string(out)))
	}
	rel, err := pkgbuildRel(src.Directory, name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return nil, fmt.Errorf("detect: read %s of pkgbuild source %s: %w", rel, src.URL, err)
	}
	return data, nil
}

// pkgbuildRel joins a pkgbuild_source directory and file into a clean
// checkout-relative path, rejecting any traversal that would escape the
// checkout.
func pkgbuildRel(directory, name string) (string, error) {
	dir := path.Clean(directory)
	if dir == "." {
		dir = ""
	}
	if dir == ".." || strings.HasPrefix(dir, "../") || path.IsAbs(dir) {
		return "", fmt.Errorf("detect: pkgbuild_source directory %q escapes the checkout", directory)
	}
	if dir == "" {
		return name, nil
	}
	return path.Join(dir, name), nil
}

// pkgbuildEnv returns the environment for external pkgbuild_source
// commands: when fetchKey names an existing file it pins the SSH identity
// through GIT_SSH_COMMAND. Empty or non-file values leave the command
// environment alone (https/file transports or externally managed
// credentials).
func pkgbuildEnv(fetchKey string) []string {
	if fetchKey == "" {
		return nil
	}
	if _, err := os.Stat(fetchKey); err != nil {
		return nil
	}
	sshCmd := "ssh -i " + strconv.Quote(fetchKey) +
		" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"
	return append(os.Environ(), "GIT_SSH_COMMAND="+sshCmd)
}

// pkgbuildHead resolves the head commit of the branch's external
// pkgbuild_source repository.
func (d *Detector) pkgbuildHead(ctx context.Context, src *PkgbuildSource) (string, error) {
	return PkgbuildHead(ctx, d.execCommand, d.cfg.FetchKey, *src)
}

// pkgbuildFile reads one file from the branch's external pkgbuild_source
// repository.
func (d *Detector) pkgbuildFile(ctx context.Context, src *PkgbuildSource, name string) ([]byte, error) {
	return PkgbuildFile(ctx, d.execCommand, d.cfg.FetchKey, *src, name)
}
