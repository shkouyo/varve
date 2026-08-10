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
	"testing"
	"time"
)

// TestFormatDuration pins the human duration scaling at the unit
// boundaries: bare seconds under a minute, minutes with seconds, hours
// with minutes, zero sub-units dropped, and negative or sub-second
// spans rendered as "0s".
func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{500 * time.Millisecond, "0s"}, // sub-second truncates to zero
		{42 * time.Second, "42s"},
		{83 * time.Second, "1m 23s"},
		{90 * time.Second, "1m 30s"},
		{2 * time.Minute, "2m"}, // zero seconds dropped
		{2*time.Hour + 5*time.Minute, "2h 5m"},
		{1 * time.Hour, "1h"}, // zero minutes dropped
		{-10 * time.Second, "0s"},
	}
	for _, tc := range cases {
		if got := formatDuration(tc.d); got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestBuildDuration covers the finished-build span derivation: a build
// with both timestamps renders the wall-clock duration, a missing
// started or finished timestamp renders empty, and a negative span
// (clock skew) is treated as unknown.
func TestBuildDuration(t *testing.T) {
	start := time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)
	finish := start.Add(83 * time.Second)
	if got := buildDuration(&start, &finish); got != "1m 23s" {
		t.Errorf("buildDuration = %q, want 1m 23s", got)
	}
	if got := buildDuration(nil, &finish); got != "" {
		t.Errorf("buildDuration(nil started) = %q, want empty", got)
	}
	if got := buildDuration(&start, nil); got != "" {
		t.Errorf("buildDuration(nil finished) = %q, want empty", got)
	}
	if got := buildDuration(nil, nil); got != "" {
		t.Errorf("buildDuration(nil, nil) = %q, want empty", got)
	}
	if got := buildDuration(&finish, &start); got != "" {
		t.Errorf("buildDuration(negative span) = %q, want empty", got)
	}
}

// TestQueueWait covers the queue wait derivation (started minus
// created): a full span renders, a missing timestamp renders empty, and
// a negative span is treated as unknown.
func TestQueueWait(t *testing.T) {
	created := time.Date(2026, 2, 3, 3, 55, 0, 0, time.UTC)
	started := created.Add(5 * time.Minute)
	if got := queueWait(&created, &started); got != "5m" {
		t.Errorf("queueWait = %q, want 5m", got)
	}
	if got := queueWait(nil, &started); got != "" {
		t.Errorf("queueWait(nil created) = %q, want empty", got)
	}
	if got := queueWait(&created, nil); got != "" {
		t.Errorf("queueWait(nil started) = %q, want empty", got)
	}
	if got := queueWait(&started, &created); got != "" {
		t.Errorf("queueWait(negative span) = %q, want empty", got)
	}
}

// TestAbsTime pins the site-wide absolute timestamp layout: nil renders
// "never", and a fixed instant renders as local wall-clock time with
// second precision ("2006-01-02 15:04:05"), the unified site format.
func TestAbsTime(t *testing.T) {
	if got := absTime(nil); got != "never" {
		t.Errorf("absTime(nil) = %q, want \"never\"", got)
	}
	when := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	got := absTime(&when)
	want := when.Local().Format("2006-01-02 15:04:05")
	if got != want {
		t.Errorf("absTime(%v) = %q, want %q (second precision)", when, got, want)
	}
}

// TestAURErrorSummary pins the web-safe rendering of a recorded AUR push
// error: control characters and whitespace collapse to single spaces (the
// summary stays one line), the text is trimmed, and over-long output is
// capped at maxAURSummaryLen runes with an ellipsis. The empty input
// renders empty, so the template gate decides visibility.
func TestAURErrorSummary(t *testing.T) {
	if got := aurErrorSummary(""); got != "" {
		t.Errorf("aurErrorSummary(\"\") = %q, want empty", got)
	}
	in := "git push demo: exit status 1:\r\n\tfatal: unable to access 'ssh://aur@aur.archlinux.org/demo.git/':\r\n\tPermission denied (publickey)"
	want := "git push demo: exit status 1: fatal: unable to access 'ssh://aur@aur.archlinux.org/demo.git/': Permission denied (publickey)"
	if got := aurErrorSummary(in); got != want {
		t.Errorf("aurErrorSummary collapse = %q, want %q", got, want)
	}
	long := strings.Repeat("x", 250)
	if got := aurErrorSummary(long); len([]rune(got)) != maxAURSummaryLen+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("aurErrorSummary cap = %d runes, want %d plus ellipsis", len([]rune(got)), maxAURSummaryLen)
	}
}

// TestPkgEpoch renders the epoch-prefixed version only when the backend
// row carries an Epoch field: an int epoch above zero renders
// ("e:2:2.0.0-1"), absent fields and zero epochs render "", and
// non-struct inputs are ignored. The field is landing in a parallel
// wave, so the helper resolves it by reflection.
func TestPkgEpoch(t *testing.T) {
	type pkg struct {
		Pkgver string
		Pkgrel string
		Epoch  int
	}
	with := pkg{Pkgver: "2.0.0", Pkgrel: "1", Epoch: 2}
	if got := pkgEpoch(with); got != "e:2:2.0.0-1" {
		t.Errorf("pkgEpoch(epoch 2) = %q, want e:2:2.0.0-1", got)
	}
	if got := pkgEpoch(pkg{Pkgver: "2.0.0", Pkgrel: "1"}); got != "" {
		t.Errorf("pkgEpoch(no epoch) = %q, want \"\"", got)
	}
	if got := pkgEpoch(42); got != "" {
		t.Errorf("pkgEpoch(non-struct) = %q, want \"\"", got)
	}
}

// TestRenderTimeUnits pins the footer render-time scaling at the unit
// boundary: 1ms and up render as whole milliseconds, everything below
// as whole microseconds, and a sub-microsecond elapsed render reads as
// 1µs rather than a bare zero. The pure formatter is fed fixed
// durations, so the scaling never depends on live timer noise.
func TestRenderTimeUnits(t *testing.T) {
	cases := []struct {
		elapsed time.Duration
		want    string
	}{
		{0, "1µs"},                     // sub-microsecond renders clamp to 1µs
		{500 * time.Nanosecond, "1µs"}, // 0.5µs truncates to 0, clamped
		{999 * time.Microsecond, "999µs"},
		{1000 * time.Microsecond, "1ms"}, // exactly 1ms crosses to ms
		{1500 * time.Microsecond, "1ms"}, // whole milliseconds truncate
		{5 * time.Millisecond, "5ms"},
	}
	for _, tc := range cases {
		if got := formatRenderTime(tc.elapsed); got != tc.want {
			t.Errorf("formatRenderTime(%v) = %q, want %q", tc.elapsed, got, tc.want)
		}
	}
}
