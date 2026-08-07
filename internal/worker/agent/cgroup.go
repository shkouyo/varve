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
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"git.0x0f.dev/varve/internal/db"
)

// CgroupSampler reads the container's cgroup v2 resource statistics:
// cpu.stat usage_usec → CPU time in ns, memory.current → resident bytes,
// and the disk usage of the filesystem holding diskPath (statfs).
// Sample() caches for the configured interval (10s) so callers sample
// cheaply at their own cadence (log batches in one-shot mode, heartbeats in
// pool mode). Paths, interval and clock are injectable for tests.
//
// All methods are safe for concurrent use.
type CgroupSampler struct {
	cpuStatPath       string
	memoryCurrentPath string
	memoryMaxPath     string
	diskPath          string
	interval          time.Duration
	now               func() time.Time

	mu     sync.Mutex
	lastAt time.Time
	last   db.Sample
}

// NewCgroupSampler builds a sampler over the container cgroup v2 mount
// and the filesystem holding diskPath (the build work directory).
func NewCgroupSampler(diskPath string) *CgroupSampler {
	return &CgroupSampler{
		cpuStatPath:       "/sys/fs/cgroup/cpu.stat",
		memoryCurrentPath: "/sys/fs/cgroup/memory.current",
		memoryMaxPath:     "/sys/fs/cgroup/memory.max",
		diskPath:          diskPath,
		interval:          10 * time.Second,
		now:               time.Now,
	}
}

// Sample returns a fresh resource sample when the interval has elapsed,
// otherwise the cached one. Missing files degrade to zero values (tolerant
// parsing).
func (s *CgroupSampler) Sample() db.Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.lastAt.IsZero() || now.Sub(s.lastAt) >= s.interval {
		total, available, used := s.diskStats()
		s.last = db.Sample{
			At:                 now,
			CPUTimeNS:          s.readCPUTime(),
			MemoryBytes:        s.readMemoryCurrent(),
			DiskTotalBytes:     total,
			DiskAvailableBytes: available,
			DiskUsedBytes:      used,
		}
		s.lastAt = now
	}
	return s.last
}

func (s *CgroupSampler) readCPUTime() int64 {
	data, err := os.ReadFile(s.cpuStatPath)
	if err != nil {
		return 0
	}
	return parseUsageUSec(data) * 1000
}

func (s *CgroupSampler) readMemoryCurrent() int64 {
	data, err := os.ReadFile(s.memoryCurrentPath)
	if err != nil {
		return 0
	}
	return parseMemoryBytes(data)
}

// diskStats returns the total/available/used bytes of the filesystem
// holding the sampled path; a statfs failure degrades to zeros.
func (s *CgroupSampler) diskStats() (total, available, used int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.diskPath, &st); err != nil {
		return 0, 0, 0
	}
	bsize := int64(st.Bsize)
	return bsize * int64(st.Blocks), bsize * int64(st.Bavail), bsize * int64(st.Blocks-st.Bfree)
}

// parseUsageUSec extracts the usage_usec counter from a cpu.stat file
// (microseconds; callers convert to ns).
func parseUsageUSec(data []byte) int64 {
	for _, line := range strings.Split(string(data), "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || key != "usage_usec" {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// parseMemoryBytes parses a memory.current / memory.max value; "max" (no
// limit) and unparsable input yield zero.
func parseMemoryBytes(data []byte) int64 {
	v := strings.TrimSpace(string(data))
	if v == "" || v == "max" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
