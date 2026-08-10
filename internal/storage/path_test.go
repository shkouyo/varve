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

package storage

import (
	"testing"
)

// TestValidName is the path traversal and whitelist table.
func TestValidName(t *testing.T) {
	valid := []string{
		"foo-1.2.3-1-x86_64.pkg.tar.zst",
		"foo.meta.toml",
		"a_b+c.d-e",
		".SRCINFO",
		"source.tar.zst",
		"staging/task-1/foo.pkg.tar.zst",
		"uploads/tmp/task-1/foo.pkg.tar.zst",
	}
	for _, name := range valid {
		if !validName(name) {
			t.Errorf("validName(%q) = false, want true", name)
		}
	}

	invalid := []string{
		"../x",         // parent traversal
		"a/../b",       // parent traversal mid-path
		"/abs",         // absolute path
		"",             // empty name
		"a//b",         // redundant separator
		"a/",           // trailing slash
		".",            // current directory
		"..",           // parent directory
		"staging/../x", // traversal below a valid prefix
		"a b",          // space not in whitelist
		"a@b",          // '@' not in whitelist
		"a\\b",         // backslash not in whitelist
		"a~b",          // '~' not in whitelist
		"-q",           // leading dash in a single segment
		"a/-b",         // leading dash in a later segment
		"-foo.pkg.tar.zst",
	}
	for _, name := range invalid {
		if validName(name) {
			t.Errorf("validName(%q) = true, want false", name)
		}
	}
}

// TestGlobLiteralPrefix asserts the literal-prefix extraction used to narrow
// server-side listings.
func TestGlobLiteralPrefix(t *testing.T) {
	cases := []struct {
		pattern, want string
	}{
		{"foo*", "foo"},
		{"*.meta.toml", ""},
		{"foo?.pkg", "foo"},
		{"staging/*", "staging/"},
		{"exact-name", "exact-name"},
		{"", ""},
	}
	for _, c := range cases {
		if got := globLiteralPrefix(c.pattern); got != c.want {
			t.Errorf("globLiteralPrefix(%q) = %q, want %q", c.pattern, got, c.want)
		}
	}
}
