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
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseUsageUSec(t *testing.T) {
	const sample = `usage_usec 1234567
user_usec 1000000
system_usec 234567
nr_periods 0
`
	if got := parseUsageUSec([]byte(sample)); got != 1234567 {
		t.Errorf("parseUsageUSec = %d, want 1234567", got)
	}
	// Missing / unknown key yields zero.
	if got := parseUsageUSec([]byte("user_usec 5\n")); got != 0 {
		t.Errorf("parseUsageUSec without usage_usec = %d, want 0", got)
	}
}

func TestParseMemoryBytes(t *testing.T) {
	if got := parseMemoryBytes([]byte("104857600\n")); got != 104857600 {
		t.Errorf("parseMemoryBytes = %d, want 104857600", got)
	}
	// "max" (no limit) and empty input yield zero.
	if got := parseMemoryBytes([]byte("max\n")); got != 0 {
		t.Errorf("parseMemoryBytes(max) = %d, want 0", got)
	}
	if got := parseMemoryBytes([]byte("")); got != 0 {
		t.Errorf("parseMemoryBytes(empty) = %d, want 0", got)
	}
}

func TestCgroupSamplerSample(t *testing.T) {
	dir := t.TempDir()
	cpu := filepath.Join(dir, "cpu.stat")
	memCur := filepath.Join(dir, "memory.current")
	memMax := filepath.Join(dir, "memory.max")
	writeFile(t, cpu, "usage_usec 250\n")
	writeFile(t, memCur, "8388608\n")
	writeFile(t, memMax, "max\n")

	clock := newFakeClock(time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC))
	s := &CgroupSampler{
		cpuStatPath:       cpu,
		memoryCurrentPath: memCur,
		memoryMaxPath:     memMax,
		interval:          10 * time.Second,
		now:               clock.now,
	}
	sm := s.Sample()
	if sm.CPUTimeNS != 250*1000 {
		t.Errorf("cpu time = %d, want %d (usec→ns)", sm.CPUTimeNS, 250*1000)
	}
	if sm.MemoryBytes != 8388608 {
		t.Errorf("memory = %d, want 8388608", sm.MemoryBytes)
	}
	if sm.At != clock.now() {
		t.Errorf("sample time = %v, want %v", sm.At, clock.now())
	}

	// Within the interval the value is cached.
	writeFile(t, cpu, "usage_usec 999\n")
	writeFile(t, memCur, "1\n")
	clock.advance(5 * time.Second)
	again := s.Sample()
	if again.CPUTimeNS != 250*1000 || again.MemoryBytes != 8388608 {
		t.Errorf("cached sample changed: %+v", again)
	}

	// After the interval a fresh sample is read.
	clock.advance(10 * time.Second)
	fresh := s.Sample()
	if fresh.CPUTimeNS != 999*1000 || fresh.MemoryBytes != 1 {
		t.Errorf("fresh sample = %+v, want cpu 999000 mem 1", fresh)
	}
}

func TestCgroupSamplerMissingFilesTolerated(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC))
	s := &CgroupSampler{
		cpuStatPath:       filepath.Join(t.TempDir(), "nope"),
		memoryCurrentPath: filepath.Join(t.TempDir(), "nope"),
		memoryMaxPath:     filepath.Join(t.TempDir(), "nope"),
		interval:          time.Second,
		now:               clock.now,
	}
	sm := s.Sample()
	if sm.CPUTimeNS != 0 || sm.MemoryBytes != 0 {
		t.Errorf("missing cgroup files should degrade to zero, got %+v", sm)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
