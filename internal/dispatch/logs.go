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

package dispatch

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Logs stores one build log per build as "<dir>/<buildID>.log"
// (decision A6, DESIGN §2.4). Append uses O_APPEND under a per-build mutex
// so concurrent appends never lose bytes; Read/TailFrom/Size surface the
// full history for the web UI. Logs of failed builds are kept forever by
// the retention policy (scheduler.go), so this structure is never asked to
// age them out.
type Logs struct {
	dir string
	mu  sync.Mutex
	lk  map[string]*sync.Mutex
}

// NewLogs builds a log store rooted at dir (typically cfg.Logs.Dir). The
// directory is created on demand.
func NewLogs(dir string) *Logs {
	return &Logs{dir: dir, lk: make(map[string]*sync.Mutex)}
}

// ErrNotFound is the package sentinel (orchestrator.go); log reads reuse it
// so the API maps a missing log to 404 uniformly.
// lock returns the per-build mutex, creating it on first use. Locks are
// never removed except by Delete, bounding the map to live builds.
func (l *Logs) lock(buildID string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.lk[buildID]
	if !ok {
		m = &sync.Mutex{}
		l.lk[buildID] = m
	}
	return m
}

// path resolves the on-disk location of a build log. It is the physical
// counterpart of Path (which renders the logical "logs/<buildID>.log").
func (l *Logs) path(buildID string) string {
	return filepath.Join(l.dir, buildID+".log")
}

// Path renders the logical log path recorded on the builds row
// ("logs/<buildID>.log", DESIGN §2.4).
func (l *Logs) Path(buildID string) string {
	return filepath.Join("logs", buildID+".log")
}

// Append writes data at the end of the build log, creating it on first
// use. O_APPEND plus the per-build mutex guarantees concurrent appends do
// not interleave or lose bytes. Concurrently safe.
func (l *Logs) Append(buildID string, data []byte) error {
	m := l.lock(buildID)
	m.Lock()
	defer m.Unlock()
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return fmt.Errorf("dispatch: logs: create dir: %w", err)
	}
	f, err := os.OpenFile(l.path(buildID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("dispatch: logs: append %s: %w", buildID, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("dispatch: logs: append %s: %w", buildID, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("dispatch: logs: append %s: %w", buildID, err)
	}
	return nil
}

// Read returns the full log content. ErrNotFound when no log exists.
// Concurrently safe.
func (l *Logs) Read(buildID string) ([]byte, error) {
	data, err := os.ReadFile(l.path(buildID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("dispatch: logs: read %s: %w", buildID, err)
	}
	return data, nil
}

// TailFrom streams the log bytes from offset onwards into w (SSE
// incremental reads) and returns the new offset. ErrNotFound when no log
// exists. Offsets past the end are clamped to the end (nothing to read).
// Concurrently safe.
func (l *Logs) TailFrom(buildID string, offset int64, w io.Writer) (int64, error) {
	if offset < 0 {
		return 0, fmt.Errorf("dispatch: logs: tail %s: negative offset %d", buildID, offset)
	}
	f, err := os.Open(l.path(buildID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("dispatch: logs: tail %s: %w", buildID, err)
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return 0, fmt.Errorf("dispatch: logs: tail %s: %w", buildID, err)
		}
	}
	n, err := io.Copy(w, f)
	if err != nil {
		return 0, fmt.Errorf("dispatch: logs: tail %s: %w", buildID, err)
	}
	return offset + n, nil
}

// Size returns the current byte length of a build log (0 for a missing
// log, which the append protocol treats as the initial offset). ErrNotFound
// is returned so callers can distinguish "no log yet" from a real error.
// Concurrently safe.
func (l *Logs) Size(buildID string) (int64, error) {
	fi, err := os.Stat(l.path(buildID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("dispatch: logs: size %s: %w", buildID, err)
	}
	return fi.Size(), nil
}

// Delete removes a build log. Deleting a missing log is a success so the
// retention sweep is idempotent; the per-build lock entry is dropped too.
// Concurrently safe.
func (l *Logs) Delete(buildID string) error {
	if err := os.Remove(l.path(buildID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("dispatch: logs: delete %s: %w", buildID, err)
	}
	l.mu.Lock()
	delete(l.lk, buildID)
	l.mu.Unlock()
	return nil
}
