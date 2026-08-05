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

package agent

import (
	"path/filepath"
	"testing"
)

func TestParseCPUTotals(t *testing.T) {
	const sample = `cpu  100 0 50 30 10 0 0 0 0 0
cpu0 100 0 50 30 10 0 0 0 0 0
`
	total, idle, ok := parseCPUTotals([]byte(sample))
	if !ok {
		t.Fatal("parseCPUTotals failed")
	}
	// total = 100+0+50+30+10 = 190; idle = 30+10 = 40.
	if total != 190 || idle != 40 {
		t.Errorf("totals = %d/%d, want 190/40", total, idle)
	}
	if _, _, ok := parseCPUTotals([]byte("intr 5\n")); ok {
		t.Error("missing cpu line should report !ok")
	}
}

func TestParseMeminfo(t *testing.T) {
	const sample = `MemTotal:       16000000 kB
MemFree:         1000000 kB
MemAvailable:    2000000 kB
Buffers:            5000 kB
`
	total, used, ok := parseMeminfo([]byte(sample))
	if !ok {
		t.Fatal("parseMeminfo failed")
	}
	if total != 16000000*1024 {
		t.Errorf("total = %d, want %d", total, int64(16000000*1024))
	}
	if used != (16000000-2000000)*1024 {
		t.Errorf("used = %d, want %d", used, int64(14000000*1024))
	}
	// MemAvailable missing → MemFree fallback.
	_, used, ok = parseMeminfo([]byte("MemTotal: 1000 kB\nMemFree: 400 kB\n"))
	if !ok || used != 600*1024 {
		t.Errorf("MemFree fallback used = %d, want %d", used, int64(600*1024))
	}
}

func TestParseUptime(t *testing.T) {
	secs, ok := parseUptime([]byte("302400.10 90000.00\n"))
	if !ok || secs != 302400 {
		t.Errorf("parseUptime = %d/%v, want 302400/true", secs, ok)
	}
}

func TestReadMetrics(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "stat"), "cpu  100 0 0 100 0 0 0 0 0 0\n")
	writeFile(t, filepath.Join(dir, "meminfo"), "MemTotal: 8000000 kB\nMemAvailable: 3000000 kB\n")
	writeFile(t, filepath.Join(dir, "uptime"), "60.5 10.0\n")

	m, counters := readMetrics(dir, cpuCounters{})
	if m.MemTotalBytes != 8000000*1024 {
		t.Errorf("mem total = %d, want %d", m.MemTotalBytes, int64(8000000*1024))
	}
	if m.MemUsedBytes != 5000000*1024 {
		t.Errorf("mem used = %d, want %d", m.MemUsedBytes, int64(5000000*1024))
	}
	if m.UptimeSecs != 60 {
		t.Errorf("uptime = %d, want 60", m.UptimeSecs)
	}
	if m.CPUPercent != 0 || !counters.valid {
		t.Errorf("first sample cpu = %v, valid=%v; want 0/true", m.CPUPercent, counters.valid)
	}

	// Second read: busy 100→200 jiffies of total 200→400 = 50%.
	writeFile(t, filepath.Join(dir, "stat"), "cpu  200 0 0 200 0 0 0 0 0 0\n")
	m, _ = readMetrics(dir, counters)
	if m.CPUPercent != 50 {
		t.Errorf("second sample cpu = %v, want 50", m.CPUPercent)
	}
}

func TestReadMetricsMissingFilesDegrade(t *testing.T) {
	m, _ := readMetrics(t.TempDir(), cpuCounters{})
	if m.CPUPercent != 0 || m.MemUsedBytes != 0 || m.UptimeSecs != 0 {
		t.Errorf("missing /proc files should degrade to zero, got %+v", m)
	}
}
