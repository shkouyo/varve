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

package agent

import (
	"regexp"
	"strings"
	"testing"
)

func TestGenerateNameFormat(t *testing.T) {
	re := regexp.MustCompile(`^[a-z]+-[a-z]+-[0-9]+$`)
	for i := 0; i < 50; i++ {
		name := generateName()
		if !re.MatchString(name) {
			t.Fatalf("generateName() = %q does not match adjective-animal-N", name)
		}
	}
}

func TestBackoffDelay(t *testing.T) {
	tests := []struct {
		failures int
		want     string
	}{
		{0, "5s"},
		{1, "10s"},
		{2, "20s"},
		{3, "40s"},
		{4, "1m0s"},
		{10, "1m0s"},
	}
	for _, tt := range tests {
		if got := backoffDelay(tt.failures).String(); got != tt.want {
			t.Errorf("backoffDelay(%d) = %s, want %s", tt.failures, got, tt.want)
		}
	}
}

func TestTruncateSummary(t *testing.T) {
	long := strings.Repeat("x", 3000)
	got := truncateSummary(long)
	if len(got) != 2000 {
		t.Errorf("truncateSummary length = %d, want 2000", len(got))
	}
	if got := truncateSummary("short"); got != "short" {
		t.Errorf("truncateSummary(short) = %q, want %q", got, "short")
	}
}
