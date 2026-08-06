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

package host

import "testing"

// TestSampleMetrics verifies /proc parsing against a fake tree: memory
// (kB→bytes), uptime and the CPU percent delta between two stat samples.
func TestSampleMetrics(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "stat",
		"cpu  100 0 0 900 0 0 0 0\ncpu0 10 0 0 10 0 0 0 0\nintr 7\n")
	writeFile(t, dir, "meminfo",
		"MemTotal:       16384000 kB\nMemAvailable:    8388608 kB\nSwapTotal:       0 kB\n")
	writeFile(t, dir, "uptime", "12345.67 98765.43\n")

	m := newMetricsReader(dir)
	first := m.sample()
	if first.CPUPercent != 0 {
		t.Errorf("first sample CPUPercent = %v, want 0 (no delta yet)", first.CPUPercent)
	}
	if want := int64(16384000 * 1024); first.MemTotalBytes != want {
		t.Errorf("MemTotalBytes = %d, want %d", first.MemTotalBytes, want)
	}
	if want := int64((16384000 - 8388608) * 1024); first.MemUsedBytes != want {
		t.Errorf("MemUsedBytes = %d, want %d", first.MemUsedBytes, want)
	}
	if first.UptimeSecs != 12345 {
		t.Errorf("UptimeSecs = %d, want 12345", first.UptimeSecs)
	}

	// Second sample: +100 busy jiffies, idle unchanged → 100% busy delta.
	writeFile(t, dir, "stat", "cpu  200 0 0 900 0 0 0 0\n")
	second := m.sample()
	if second.CPUPercent != 100 {
		t.Errorf("second sample CPUPercent = %v, want 100", second.CPUPercent)
	}
}

// TestSampleMissingFiles verifies the missing-file tolerance: an empty
// fake /proc yields a zero metrics struct without error.
func TestSampleMissingFiles(t *testing.T) {
	m := newMetricsReader(t.TempDir())
	out := m.sample()
	if out.CPUPercent != 0 || out.MemTotalBytes != 0 || out.MemUsedBytes != 0 || out.UptimeSecs != 0 {
		t.Errorf("missing files must degrade to zeros, got %+v", out)
	}
}

// TestMeminfoWithoutAvailable verifies used stays 0 when MemAvailable is
// missing while total is still reported.
func TestMeminfoWithoutAvailable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "meminfo", "MemTotal: 1000 kB\n")
	m := newMetricsReader(dir)
	out := m.sample()
	if out.MemTotalBytes != 1000*1024 {
		t.Errorf("MemTotalBytes = %d, want %d", out.MemTotalBytes, 1000*1024)
	}
	if out.MemUsedBytes != 0 {
		t.Errorf("MemUsedBytes = %d, want 0 without MemAvailable", out.MemUsedBytes)
	}
}

// TestUptimeTruncation verifies fractional uptime is truncated to whole
// seconds.
func TestUptimeTruncation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "uptime", "59.99 30.00\n")
	m := newMetricsReader(dir)
	if got := m.sample().UptimeSecs; got != 59 {
		t.Errorf("UptimeSecs = %d, want 59", got)
	}
}
