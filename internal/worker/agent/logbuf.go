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
	"log"
	"net/http"
	"sync"
	"time"

	"git.0x0f.dev/varve/internal/api"
)

// progressFn produces the optional one-shot progress sample attached to a
// log segment: one-shot agents report samples with their log batches; pool
// agents report them with heartbeats instead.
type progressFn func() *api.TaskProgress

// LogBuffer batches build output into log segments: a batch is flushed on
// the earlier of the byte threshold and the interval tick, and the final
// Close flushes whatever is left. The controller acknowledges every segment
// with the next offset and a durable cancellation flag (channel 2). Both
// parameters are injectable so tests can trigger fast flushes.
//
// Write is safe for concurrent use (os/exec feeds stdout/stderr from its
// own copy goroutine); the flushing goroutine is the single AppendLog
// caller.
type LogBuffer struct {
	append    func(api.LogSegment) (*api.LogAck, error)
	threshold int
	progress  progressFn

	mu     sync.Mutex
	buf    bytes.Buffer
	offset int64
	// discarded counts output bytes dropped by the Write cap while the
	// buffer was full (the controller unreachable for a long stretch).
	discarded int64
	closed    bool
	cancel    chan struct{}
	once      sync.Once
	flushCh   chan struct{}
	done      chan struct{}
	wg        sync.WaitGroup
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

// maxBufferedLog caps the output a LogBuffer keeps while the controller
// is unreachable. Writes past the cap are dropped and counted instead of
// growing memory without bound; the stream stays consistent because only
// never-acknowledged tail bytes are lost and the wire offset contract
// (server size vs segment offset, resynced on 409) prevents duplicates or
// reordering. A variable so tests can shrink it.
var maxBufferedLog = 64 << 20

// Write appends output, triggering a flush when the threshold is reached.
// When the buffer already holds maxBufferedLog bytes the input is dropped
// (counted, logged once) so a stalled flush loop cannot grow memory.
func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	if len(p) > 0 && b.buf.Len() >= maxBufferedLog {
		if b.discarded == 0 {
			log.Printf("agent: log buffer full (%d bytes), dropping build output", maxBufferedLog)
		}
		b.discarded += int64(len(p))
		b.mu.Unlock()
		return len(p), nil
	}
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
			// Final drain: flush the remainder in bounded segments.
			// The loop stops when the buffer is empty or an append
			// fails, so Close cannot hang on a dead controller.
			for b.flush() {
			}
			b.mu.Lock()
			left := b.buf.Len()
			b.mu.Unlock()
			if left > 0 {
				log.Printf("agent: %d bytes of build log dropped at close", left)
			}
			return
		case <-b.flushCh:
			b.flush()
		case <-t.C:
			if !b.flush() {
				b.flushHeartbeat()
			}
		}
	}
}

// flush sends one bounded log segment, or keeps the data for a later
// retry when the append fails (resyncing the offset from a 409 conflict so
// a gap can never wedge the stream). A conflict whose resync offset does
// not advance the stream ends it: the controller refuses the segment for
// good (the task became terminal), so the buffered tail is dropped
// instead of retried forever. At most threshold bytes are sent per
// segment: the producer drains the whole pipe into the buffer while the
// flush loop is busy with an in-flight append, and a single oversized
// segment would exceed the controller's segment cap, stalling the stream
// forever. The remainder stays buffered for the next flush; Close drains
// it. flush reports whether a segment was sent (a successful append); the
// done-path drain stops when the buffer is empty or an append fails.
func (b *LogBuffer) flush() bool {
	b.mu.Lock()
	if b.buf.Len() == 0 {
		b.mu.Unlock()
		return false
	}
	n := b.threshold
	if b.buf.Len() < n {
		n = b.buf.Len()
	}
	data := string(b.buf.Next(n))
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
			if apiErr.Offset > b.offset {
				// Resumable conflict: the server holds more bytes than
				// the client believes; resync and re-send from there.
				b.offset = apiErr.Offset
			} else {
				// The conflict does not advance the stream: the server
				// refuses the segment for good (the task became
				// terminal). The log stream is over; drop the tail.
				b.buf.Reset()
			}
		}
		log.Printf("agent: log flush (offset %d): %v", offset, err)
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
	return err == nil
}

// flushHeartbeat sends an empty segment carrying only a progress sample
// when the buffer holds nothing to flush. A one-shot build that produces
// no output would otherwise never reach the controller: no log segment
// means no acknowledgement, so a cancellation could never arrive and the
// server-side last_progress_at stamp would go stale (the scheduler reaps
// tasks without progress). The empty segment is acknowledged like any
// other (the controller appends zero bytes and answers with the
// cancellation flag), so a silent build keeps a live cancellation
// channel and a fresh progress stamp. Pool agents do not use this path
// (their progress is nil): their heartbeats already carry the
// cancellation signal.
func (b *LogBuffer) flushHeartbeat() {
	b.mu.Lock()
	if b.progress == nil {
		b.mu.Unlock()
		return
	}
	progress := b.progress()
	offset := b.offset
	b.mu.Unlock()

	ack, err := b.append(api.LogSegment{Offset: offset, Progress: progress})

	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		log.Printf("agent: log heartbeat (offset %d): %v", offset, err)
		return
	}
	b.offset = ack.Offset
	if ack.Cancelled {
		b.once.Do(func() { close(b.cancel) })
	}
}

// tailBuffer keeps the last max bytes written; it feeds failure summaries
// that include the tail of the build log.
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
