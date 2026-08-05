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

package storage

import (
	"path"
	"strings"
)

// StagingPath returns the staging area path of a task artifact
// (DESIGN §4.1): "staging/<taskID>/<fileName>". The staging tree is created
// implicitly on first upload and must be cleaned up after result handling.
func StagingPath(taskID, fileName string) string {
	return "staging/" + taskID + "/" + fileName
}

// validName reports whether name is a safe, normalized virtual path usable
// by both backends (DETAIL §5.4):
//
//   - path.Clean(name) == name (no redundant separators, no "." segments,
//     no trailing slash),
//   - name does not start with "/" (no absolute paths),
//   - no segment is ".." (no parent traversal),
//   - every segment matches the basename whitelist [A-Za-z0-9._+-]
//     (identical to the upload whitelist, DESIGN §5.7).
//
// The whitelist intentionally excludes separators and shell metacharacters,
// so a validated name is safe to embed in a filesystem path (local) and in
// an object key (s3).
func validName(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") {
		return false
	}
	if path.Clean(name) != name {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		if !validBasename(seg) {
			return false
		}
	}
	return true
}

// validBasename reports whether a single path segment contains only
// whitelisted characters (DETAIL §5.4).
func validBasename(seg string) bool {
	for _, r := range seg {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '+', r == '-':
		default:
			return false
		}
	}
	return true
}

// globLiteralPrefix returns the longest literal (non-meta) prefix of a glob
// pattern: "foo*" -> "foo", "*.meta.toml" -> "". Backends use it to narrow
// server-side listings before applying path.Match client-side (DETAIL §5.4).
func globLiteralPrefix(pattern string) string {
	for i, r := range pattern {
		switch r {
		case '*', '?', '[', '\\':
			return pattern[:i]
		}
	}
	return pattern
}
