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
	"path/filepath"
	"strconv"
	"strings"

	"git.0x0f.dev/varve/internal/api"
)

// cpuCounters holds the last /proc/stat CPU totals used to compute the
// heartbeat CPU percentage as a delta over the sampling window.
type cpuCounters struct {
	total, idle uint64
	valid       bool
}

// readMetrics reads node-level system metrics from procDir (default
// /proc): CPU percentage (delta against prev), memory usage and uptime.
// Pool heartbeats carry these. Missing or unparsable files degrade to zero
// values; the /proc paths are injectable for tests.
func readMetrics(procDir string, prev cpuCounters) (api.Metrics, cpuCounters) {
	m := api.Metrics{}

	if data, err := os.ReadFile(filepath.Join(procDir, "stat")); err == nil {
		if total, idle, ok := parseCPUTotals(data); ok {
			cur := cpuCounters{total: total, idle: idle, valid: true}
			if prev.valid && cur.total > prev.total {
				busyDelta := (cur.total - cur.idle) - (prev.total - prev.idle)
				totalDelta := cur.total - prev.total
				if totalDelta > 0 {
					pct := 100 * float64(busyDelta) / float64(totalDelta)
					if pct < 0 {
						pct = 0
					} else if pct > 100 {
						pct = 100
					}
					m.CPUPercent = pct
				}
			}
			prev = cur
		}
	}

	if data, err := os.ReadFile(filepath.Join(procDir, "meminfo")); err == nil {
		if total, used, ok := parseMeminfo(data); ok {
			m.MemTotalBytes, m.MemUsedBytes = total, used
		}
	}

	if data, err := os.ReadFile(filepath.Join(procDir, "uptime")); err == nil {
		if secs, ok := parseUptime(data); ok {
			m.UptimeSecs = secs
		}
	}
	return m, prev
}

// parseCPUTotals sums the jiffie counters of the first "cpu " line of
// /proc/stat (total busy+idle and the idle portion).
func parseCPUTotals(data []byte) (total, idle uint64, ok bool) {
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		if len(fields) < 5 {
			return 0, 0, false
		}
		var sum uint64
		for _, f := range fields {
			n, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			sum += n
		}
		idleN, err := strconv.ParseUint(fields[3], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		iowait := uint64(0)
		if len(fields) > 4 {
			if n, err := strconv.ParseUint(fields[4], 10, 64); err == nil {
				iowait = n
			}
		}
		return sum, idleN + iowait, true
	}
	return 0, 0, false
}

// parseMeminfo extracts MemTotal and MemAvailable (kB → bytes); MemFree is
// the fallback when MemAvailable is absent.
func parseMeminfo(data []byte) (total, used int64, ok bool) {
	var memTotal, memAvail int64
	sawTotal := false
	for _, line := range strings.Split(string(data), "\n") {
		key, val, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found {
			continue
		}
		v := strings.TrimSpace(strings.TrimSuffix(val, "kB"))
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(key) {
		case "MemTotal":
			memTotal, sawTotal = n, true
		case "MemAvailable":
			memAvail = n
		case "MemFree":
			if memAvail == 0 {
				memAvail = n
			}
		}
	}
	if !sawTotal {
		return 0, 0, false
	}
	return memTotal * 1024, (memTotal - memAvail) * 1024, true
}

// parseUptime returns the first field of /proc/uptime (seconds).
func parseUptime(data []byte) (int64, bool) {
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, false
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return int64(f), true
}
