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

import "testing"

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
