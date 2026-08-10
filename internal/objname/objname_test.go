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

package objname

import (
	"strings"
	"testing"
)

// TestValidName covers the shared package/base name rule: the AUR
// character set without a leading dash, capped at 255 bytes.
func TestValidName(t *testing.T) {
	valid := []string{
		"foo",
		"foo+bar_baz",
		"linux-zen",
		"foo@bar",
		"a.b-c_d+e",
		"0-9_A.Z+",
		strings.Repeat("x", 255),
	}
	for _, s := range valid {
		if !ValidPkgname(s) || !ValidPkgbase(s) {
			t.Errorf("ValidName(%q) = false, want true", s)
		}
	}

	invalid := []string{
		"",
		"-foo",
		"-",
		"a/b",
		"a b",
		"a\tb",
		"a\x00b",
		"é",
		"a\\b",
		"a~b",
		strings.Repeat("x", 256),
	}
	for _, s := range invalid {
		if ValidPkgname(s) || ValidPkgbase(s) {
			t.Errorf("ValidName(%q) = true, want false", s)
		}
	}
}
