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
	"bytes"
	"errors"
	"net/http"
	"sync"
	"time"

	"git.0x0f.dev/varve/internal/api"
)

// progressFn produces the optional one-shot progress sample attached to a
// log segment (decision A10: one-shot agents report samples with their log
// batches; pool agents report them with heartbeats instead).
type progressFn func() *api.TaskProgress

// LogBuffer batches build output into log segments (DETAIL §12.4 #4,
// optimization O2): a batch is flushed on the earlier of the byte
// threshold and the interval tick, and the final Close flushes whatever is
// left. The controller acknowledges every segment with the next offset and
// a durable cancellation flag (channel 2, DESIGN §7.8). Both parameters
// are injectable so tests can trigger fast flushes.
//
// Write is safe for concurrent use (os/exec feeds stdout/stderr from its
// own copy goroutine); the flushing goroutine is the single AppendLog
// caller.
type LogBuffer struct {
	append    func(api.LogSegment) (*api.LogAck, error)
	threshold int
	progress  progressFn

	mu      sync.Mutex
	buf     bytes.Buffer
	offset  int64
	closed  bool
	cancel  chan struct{}
	once    sync.Once
	flushCh chan struct{}
	done    chan struct{}
	wg      sync.WaitGroup
}

// NewLogBuffer starts a LogBuffer. append sends one buffered segment and
// returns the controller acknowledgement; the returned buffer must be
// closed when the producer is done.
func NewLogBuffer(append func(api.LogSegment) (*api.LogAck, error), threshold int, interval time.Duration, progress progressFn) *LogBuffer {
	b := &LogBuffer{
		append:    append,
		threshold: threshold,
		progress:  progress,
		cancel:    make(chan struct{}),
		flushCh:   make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
	b.wg.Add(1)
	go b.loop(interval)
	return b
}

// Write appends output, triggering a flush when the threshold is reached.
func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.buf.Write(p)
	full := b.buf.Len() >= b.threshold
	b.mu.Unlock()
	if full {
		b.signal()
	}
	return len(p), nil
}

// Close stops the flush goroutine after one final flush. It is idempotent.
func (b *LogBuffer) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.mu.Unlock()
	close(b.done)
	b.wg.Wait()
}

// Cancelled is closed when a log acknowledgement carries the durable
// cancellation flag (channel 2).
func (b *LogBuffer) Cancelled() <-chan struct{} { return b.cancel }

// Offset returns the next segment offset (server-acknowledged).
func (b *LogBuffer) Offset() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.offset
}

func (b *LogBuffer) signal() {
	select {
	case b.flushCh <- struct{}{}:
	default:
	}
}

func (b *LogBuffer) loop(interval time.Duration) {
	defer b.wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-b.done:
			b.flush()
			return
		case <-b.flushCh:
			b.flush()
		case <-t.C:
			b.flush()
		}
	}
}

// flush sends the buffered data as one segment, or keeps it for a later
// retry when the append fails (resyncing the offset from a 409 conflict so
// a gap can never wedge the stream).
func (b *LogBuffer) flush() {
	b.mu.Lock()
	if b.buf.Len() == 0 {
		b.mu.Unlock()
		return
	}
	data := b.buf.String()
	b.buf.Reset()
	offset := b.offset
	var progress *api.TaskProgress
	if b.progress != nil {
		progress = b.progress()
	}
	b.mu.Unlock()

	ack, err := b.append(api.LogSegment{Offset: offset, Data: data, Progress: progress})

	b.mu.Lock()
	if err != nil {
		combined := data + b.buf.String()
		b.buf.Reset()
		b.buf.WriteString(combined)
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
			b.offset = apiErr.Offset
		}
	} else {
		b.offset = ack.Offset
		if ack.Cancelled {
			b.once.Do(func() { close(b.cancel) })
		}
	}
	if b.buf.Len() >= b.threshold {
		b.signal()
	}
	b.mu.Unlock()
}

// tailBuffer keeps the last max bytes written; it feeds failure summaries
// that include the tail of the build log (DETAIL §12.5).
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

// Last returns the last n bytes (or everything when shorter).
func (t *tailBuffer) Last(n int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n >= len(t.buf) {
		return string(t.buf)
	}
	return string(t.buf[len(t.buf)-n:])
}
