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
	"path"
	"strings"
)

// defaultExcludeBranches is applied when the configuration leaves
// exclude_branches unset.
var defaultExcludeBranches = []string{"main"}

// listBranches enumerates the mirror's heads and drops the excluded ones
// (path.Match glob per pattern). Branches deleted upstream disappear after
// a pruned fetch and are simply not enumerated any more; their packages
// rows are left untouched for manual inspection.
func (d *Detector) listBranches(ctx context.Context) ([]string, error) {
	cmd := d.execCommand(ctx, "git", "-C", d.mirrorDir,
		"for-each-ref", "refs/heads", "--format=%(refname:short)")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("detect: enumerate branches: %w: %s", err, strings.TrimSpace(string(out)))
	}

	excludes := d.cfg.ExcludeBranches
	if len(excludes) == 0 {
		excludes = defaultExcludeBranches
	}
	var branches []string
	for _, line := range strings.Split(string(out), "\n") {
		branch := strings.TrimSpace(line)
		if branch == "" {
			continue
		}
		if matchAnyGlob(branch, excludes) {
			continue
		}
		branches = append(branches, branch)
	}
	return branches, nil
}

// matchAnyGlob reports whether branch matches any of the glob patterns
// (path.Match per pattern; malformed patterns match nothing).
func matchAnyGlob(branch string, patterns []string) bool {
	for _, p := range patterns {
		ok, err := path.Match(p, branch)
		if err == nil && ok {
			return true
		}
	}
	return false
}

// BranchSnapshot returns the commit the branch currently points at
// ("git rev-parse refs/heads/<branch>"). The caller uses it to stamp
// tasks with the detected commit. Not safe for concurrent use with
// PollOnce/Run.
func (d *Detector) BranchSnapshot(ctx context.Context, branch string) (string, error) {
	cmd := d.execCommand(ctx, "git", "-C", d.mirrorDir, "rev-parse", "refs/heads/"+branch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("detect: snapshot branch %q: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	commit := strings.TrimSpace(string(out))
	if commit == "" {
		return "", fmt.Errorf("detect: snapshot branch %q: empty commit", branch)
	}
	return commit, nil
}
