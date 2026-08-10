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

package web

import (
	"strconv"
	"time"
)

// formatDuration renders a duration in a compact human form: hours with
// minutes ("2h 5m"), minutes with seconds ("1m 23s"), or bare seconds
// ("42s"). Zero sub-units are dropped ("2h", "1m") and a sub-second or
// negative duration renders as "0s".
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= time.Hour:
		h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
		if m == 0 {
			return strconv.Itoa(h) + "h"
		}
		return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
	case d >= time.Minute:
		m, s := int(d/time.Minute), int(d%time.Minute/time.Second)
		if s == 0 {
			return strconv.Itoa(m) + "m"
		}
		return strconv.Itoa(m) + "m " + strconv.Itoa(s) + "s"
	default:
		return strconv.Itoa(int(d/time.Second)) + "s"
	}
}

// buildDuration renders the wall-clock duration of a finished build
// (finished_at minus started_at). A build that never started or never
// finished renders empty, and a negative span (clock skew) is treated
// as unknown too.
func buildDuration(started, finished *time.Time) string {
	if started == nil || finished == nil {
		return ""
	}
	d := finished.Sub(*started)
	if d < 0 {
		return ""
	}
	return formatDuration(d)
}

// queueWait renders the time a build waited in the queue (started_at
// minus created_at). It is empty when either timestamp is missing or
// the span is negative.
func queueWait(created, started *time.Time) string {
	if created == nil || started == nil {
		return ""
	}
	d := started.Sub(*created)
	if d < 0 {
		return ""
	}
	return formatDuration(d)
}
