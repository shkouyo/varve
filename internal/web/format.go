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

package web

import (
	"strings"
	"time"
)

// displayArch renders the stored architecture set ("x86_64",
// "aarch64|x86_64", "any") for display: "any" dominates, then x86_64,
// matching the farm's single display vocabulary. Exotic-only sets (no
// any, no x86_64) render as-is rather than lying about the build target.
func displayArch(set string) string {
	for _, elem := range strings.Split(set, "|") {
		if elem == "any" {
			return "any"
		}
	}
	for _, elem := range strings.Split(set, "|") {
		if elem == "x86_64" {
			return "x86_64"
		}
	}
	return set
}

// shortID truncates a long identifier (task uuid) to its first eight
// characters for table cells; build ids (16 hex) stay full.
func shortID(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8] + "…"
}

// relTime renders an optional timestamp as a relative age ("3m ago").
func relTime(t *time.Time) string {
	return formatWhen(t, time.Now())
}

// absTime renders an optional timestamp as local wall-clock time.
func absTime(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Local().Format("2006-01-02 15:04")
}
