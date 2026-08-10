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

// Package objname validates Arch Linux package base and package names
// (the AUR character set). It is a leaf package shared by the web, api,
// dispatch and repo layers so that every boundary applies the same rule.
package objname

// maxNameLen caps a validated name at 255 bytes, aligned with the upload
// file name bound of the worker protocol.
const maxNameLen = 255

// ValidPkgbase reports whether s is a well-formed package base name:
// one to maxNameLen bytes of the AUR separator set (@ . _ + -) or
// letters/digits, without a leading dash.
func ValidPkgbase(s string) bool {
	return validName(s)
}

// ValidPkgname reports whether s is a well-formed package name, using
// the same character set and bounds as the package base name.
func ValidPkgname(s string) bool {
	return validName(s)
}

// ValidChar reports whether c is in the shared upload whitelist
// [A-Za-z0-9._+-], the single character-class source for every layer
// that validates object keys, upload names or config prefixes. The '@'
// of the AUR package-name set is deliberately absent: only
// ValidPkgname/ValidPkgbase admit it.
func ValidChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '.' || c == '_' || c == '+' || c == '-':
		return true
	}
	return false
}

// validName implements the shared rule: 1..maxNameLen bytes, each either
// an ASCII letter/digit or one of "@._+-", with no leading dash (a
// dash-prefixed name would be parsed as an option by pacman tools). The
// package set is the upload whitelist plus '@' for AUR separators.
func validName(s string) bool {
	if s == "" || len(s) > maxNameLen || s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !ValidChar(s[i]) && s[i] != '@' {
			return false
		}
	}
	return true
}
