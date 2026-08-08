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
