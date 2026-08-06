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

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"git.0x0f.dev/varve/internal/api"
)

// metricsReader samples host metrics from /proc (Linux). The proc
// directory is injectable so tests can use a fake tree; missing files
// degrade to zero values instead of failing the heartbeat.
type metricsReader struct {
	procDir string
	prevCPU *cpuSample
}

// cpuSample is one /proc/stat aggregate snapshot.
type cpuSample struct {
	total uint64 // user+nice+system+idle+iowait+irq+softirq+steal jiffies
	idle  uint64 // idle+iowait jiffies
}

// newMetricsReader builds a reader over procDir ("/proc" in production).
func newMetricsReader(procDir string) *metricsReader {
	return &metricsReader{procDir: procDir}
}

// sample returns the current host metrics: /proc/stat CPU percentage,
// /proc/meminfo used/total memory and /proc/uptime. CPU percent is derived
// from the delta between consecutive samples; the first sample reports 0. A
// missing or unparsable file contributes zero values rather than an error
// (missing-file tolerance).
func (m *metricsReader) sample() api.Metrics {
	var out api.Metrics
	if cur, ok := readCPUStat(filepath.Join(m.procDir, "stat")); ok {
		if m.prevCPU != nil {
			dTotal := cur.total - m.prevCPU.total
			dIdle := cur.idle - m.prevCPU.idle
			if dTotal > 0 {
				out.CPUPercent = float64(dTotal-dIdle) * 100 / float64(dTotal)
			}
		}
		m.prevCPU = &cur
	}
	if total, used, ok := readMeminfo(filepath.Join(m.procDir, "meminfo")); ok {
		out.MemTotalBytes = total
		out.MemUsedBytes = used
	}
	if secs, ok := readUptime(filepath.Join(m.procDir, "uptime")); ok {
		out.UptimeSecs = secs
	}
	return out
}

// readCPUStat parses the aggregate "cpu " line of /proc/stat: jiffies
// user nice system idle iowait irq softirq steal (the guest fields are
// already counted inside user/nice and are skipped).
func readCPUStat(path string) (cpuSample, bool) {
	f, err := os.Open(path)
	if err != nil {
		return cpuSample{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 9 || fields[0] != "cpu" {
			continue
		}
		var total, idle uint64
		for i := 1; i <= 8; i++ {
			v, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				return cpuSample{}, false
			}
			total += v
			if i == 4 || i == 5 { // idle (4), iowait (5)
				idle += v
			}
		}
		return cpuSample{total: total, idle: idle}, true
	}
	return cpuSample{}, false
}

// readMeminfo parses MemTotal and MemAvailable (kB) and returns total and
// used bytes; MemUsed = MemTotal - MemAvailable. A missing MemAvailable
// leaves used at zero (total is still reported).
func readMeminfo(path string) (totalBytes, usedBytes int64, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	var totalKB, availKB int64
	var haveTotal, haveAvail bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				totalKB, haveTotal = v, true
			}
		case "MemAvailable:":
			if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				availKB, haveAvail = v, true
			}
		}
	}
	if !haveTotal {
		return 0, 0, false
	}
	totalBytes = totalKB * 1024
	if haveAvail && availKB < totalKB {
		usedBytes = (totalKB - availKB) * 1024
	}
	return totalBytes, usedBytes, true
}

// readUptime parses the first field of /proc/uptime (seconds as a float).
func readUptime(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, false
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return int64(f), true
}
