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
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/api"
)

// recorder accumulates the segments handed to the LogBuffer append.
type logRecorder struct {
	mu       sync.Mutex
	segments []api.LogSegment
	acks     []api.LogAck
	errors   []error
	// block, when non-nil, parks every append call until the channel is
	// closed (simulates an in-flight request against a slow controller).
	block <-chan struct{}
}

func (r *logRecorder) append(seg api.LogSegment) (*api.LogAck, error) {
	if r.block != nil {
		<-r.block
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.segments = append(r.segments, seg)
	if len(r.errors) > 0 {
		err := r.errors[0]
		r.errors = r.errors[1:]
		return nil, err
	}
	var ack api.LogAck
	if len(r.acks) > 0 {
		ack = r.acks[0]
		r.acks = r.acks[1:]
	}
	if ack.Offset == 0 {
		ack.Offset = int64(len(seg.Data))
	}
	return &ack, nil
}

func (r *logRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.segments)
}

func TestLogBufferThresholdFlush(t *testing.T) {
	rec := &logRecorder{}
	b := NewLogBuffer(rec.append, 64, time.Hour, nil) // interval never fires
	defer b.Close()

	// 64 bytes exactly reach the threshold on the final write.
	for i := 0; i < 4; i++ {
		if _, err := b.Write([]byte(strings.Repeat("x", 16))); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if !waitFor(t, time.Second, func() bool { return rec.count() == 1 }) {
		t.Fatalf("threshold flush never happened")
	}
	seg := rec.segments[0]
	if seg.Offset != 0 {
		t.Errorf("first segment offset = %d, want 0", seg.Offset)
	}
	if len(seg.Data) != 64 {
		t.Errorf("segment data length = %d, want 64", len(seg.Data))
	}
	if seg.Progress != nil {
		t.Errorf("unexpected progress on segment: %+v", seg.Progress)
	}
}

func TestLogBufferIntervalFlush(t *testing.T) {
	rec := &logRecorder{}
	b := NewLogBuffer(rec.append, 64*1024, 10*time.Millisecond, nil)
	defer b.Close()
	if _, err := b.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !waitFor(t, time.Second, func() bool { return rec.count() == 1 }) {
		t.Fatalf("interval flush never happened")
	}
	if rec.segments[0].Data != "hello" {
		t.Errorf("segment data = %q, want %q", rec.segments[0].Data, "hello")
	}
}

func TestLogBufferCloseFlushes(t *testing.T) {
	rec := &logRecorder{}
	b := NewLogBuffer(rec.append, 64*1024, time.Hour, nil)
	if _, err := b.Write([]byte("tail")); err != nil {
		t.Fatalf("write: %v", err)
	}
	b.Close()
	if rec.count() != 1 {
		t.Fatalf("close flush did not send the buffer (segments=%d)", rec.count())
	}
	if rec.segments[0].Data != "tail" {
		t.Errorf("segment data = %q, want %q", rec.segments[0].Data, "tail")
	}
}

func TestLogBufferOffsetAdvancesByAck(t *testing.T) {
	rec := &logRecorder{}
	rec.acks = []api.LogAck{{Offset: 100}, {Offset: 150}}
	b := NewLogBuffer(rec.append, 16, time.Hour, nil)
	defer b.Close()
	if _, err := b.Write([]byte(strings.Repeat("a", 16))); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !waitFor(t, time.Second, func() bool { return rec.count() == 1 }) {
		t.Fatalf("first flush never happened")
	}
	if got := b.Offset(); got != 100 {
		t.Errorf("offset after first ack = %d, want 100", got)
	}
	if _, err := b.Write([]byte(strings.Repeat("b", 16))); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !waitFor(t, time.Second, func() bool { return rec.count() == 2 }) {
		t.Fatalf("second flush never happened")
	}
	if got := rec.segments[1].Offset; got != 100 {
		t.Errorf("second segment offset = %d, want 100", got)
	}
	if b.Offset() != 150 {
		t.Errorf("offset after second ack = %d, want 150", b.Offset())
	}
}

func TestLogBufferCancelledFlagClosesChannel(t *testing.T) {
	rec := &logRecorder{}
	rec.acks = []api.LogAck{{Offset: 5, Cancelled: true}}
	b := NewLogBuffer(rec.append, 16, time.Hour, nil)
	defer b.Close()
	if _, err := b.Write([]byte(strings.Repeat("x", 16))); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-b.Cancelled():
	case <-time.After(time.Second):
		t.Fatalf("cancelled channel never closed")
	}
}

func TestLogBufferConflictResyncsOffset(t *testing.T) {
	rec := &logRecorder{}
	rec.errors = []error{&api.APIError{Status: http.StatusConflict, Offset: 55}}
	rec.acks = []api.LogAck{{Offset: 70}}
	b := NewLogBuffer(rec.append, 16, time.Hour, nil)
	defer b.Close()
	if _, err := b.Write([]byte(strings.Repeat("x", 16))); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !waitFor(t, time.Second, func() bool { return rec.count() == 2 }) {
		t.Fatalf("retry after 409 never happened")
	}
	if b.Offset() != 70 {
		t.Errorf("offset after resync = %d, want 70", b.Offset())
	}
	// The failed batch must be preserved and re-sent from the resynced
	// offset.
	if rec.segments[1].Offset != 55 {
		t.Errorf("retried segment offset = %d, want 55", rec.segments[1].Offset)
	}
	if rec.segments[1].Data != strings.Repeat("x", 16) {
		t.Errorf("retried segment data lost")
	}
}

// TestLogBufferBurstSplitsSegments reproduces the oversize-batch stall:
// the producer drains the whole pipe into the buffer while the flush loop
// is busy with an in-flight append, so one flush can find far more than
// threshold bytes buffered. The segment must be split into bounded
// batches; a single oversized segment would exceed the controller's
// segment cap, get rejected, and stall the log stream forever (the build
// succeeds but the log is truncated).
func TestLogBufferBurstSplitsSegments(t *testing.T) {
	rec := &logRecorder{}
	b := NewLogBuffer(rec.append, 64, time.Hour, nil) // interval never fires
	defer b.Close()

	burst := strings.Repeat("x", 64*64) // one burst of 64 thresholds
	if _, err := b.Write([]byte(burst)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return rec.count() == 64 }) {
		t.Fatalf("burst was not fully split: segments=%d, want 64", rec.count())
	}
	var joined []byte
	for i, seg := range rec.segments {
		if len(seg.Data) > 64 {
			t.Errorf("segment %d length = %d, exceeds the threshold 64", i, len(seg.Data))
		}
		joined = append(joined, seg.Data...)
	}
	if len(joined) != len(burst) || string(joined) != burst {
		t.Errorf("delivered %d bytes, want the full %d-byte burst (no loss, no duplication)", len(joined), len(burst))
	}
}

func TestLogBufferProgressAttached(t *testing.T) {
	rec := &logRecorder{}
	prog := &api.TaskProgress{TaskID: "t-1", Stage: "makepkg", CPUTimeNS: 42, MemoryBytes: 7}
	b := NewLogBuffer(rec.append, 16, time.Hour, func() *api.TaskProgress { return prog })
	defer b.Close()
	if _, err := b.Write([]byte(strings.Repeat("x", 16))); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !waitFor(t, time.Second, func() bool { return rec.count() == 1 }) {
		t.Fatalf("flush never happened")
	}
	if rec.segments[0].Progress == nil || rec.segments[0].Progress.CPUTimeNS != 42 {
		t.Errorf("segment progress not attached: %+v", rec.segments[0].Progress)
	}
}

// TestLogBufferDropsWhenFull asserts the hard buffer cap: writes past
// maxBufferedLog are dropped and counted instead of growing memory, and
// the retained prefix still flushes on Close.
func TestLogBufferDropsWhenFull(t *testing.T) {
	orig := maxBufferedLog
	maxBufferedLog = 64
	defer func() { maxBufferedLog = orig }()

	rec := &logRecorder{}
	b := NewLogBuffer(rec.append, 1<<20, time.Hour, nil) // threshold unreachable: nothing flushes early
	for i := 0; i < 10; i++ {
		if _, err := b.Write([]byte("0123456789")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	b.mu.Lock()
	buffered := b.buf.Len()
	discarded := b.discarded
	b.mu.Unlock()
	if discarded != 30 {
		t.Errorf("discarded = %d, want 30 (the last three 10-byte writes)", discarded)
	}
	if buffered > maxBufferedLog+9 {
		t.Errorf("buffered = %d, want at most %d", buffered, maxBufferedLog+9)
	}
	b.Close()
	if rec.count() != 1 {
		t.Fatalf("close flush segments = %d, want 1", rec.count())
	}
	if got := rec.segments[0].Data; got != strings.Repeat("0123456789", 7) {
		t.Errorf("flushed data = %d bytes, want the retained 70-byte prefix", len(got))
	}
}

// TestLogBufferSustainedFailureBounded asserts a controller that is slow
// (append parked) neither grows the buffer past the cap nor hangs Close:
// output beyond the cap is dropped, and after the controller responds the
// retained prefix flushes with a continuous offset sequence (no
// duplicates, no reordering).
func TestLogBufferSustainedFailureBounded(t *testing.T) {
	orig := maxBufferedLog
	maxBufferedLog = 64
	defer func() { maxBufferedLog = orig }()

	block := make(chan struct{})
	rec := &logRecorder{block: block}
	b := NewLogBuffer(rec.append, 16, time.Hour, nil)
	defer b.Close()

	burst := strings.Repeat("x", 4096)
	for i := 0; i < 256; i++ {
		if _, err := b.Write([]byte(burst[i*16 : (i+1)*16])); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	b.mu.Lock()
	buffered := b.buf.Len()
	discarded := b.discarded
	b.mu.Unlock()
	if discarded == 0 {
		t.Error("discarded = 0, want drops past the cap")
	}
	if buffered > maxBufferedLog+15 {
		t.Errorf("buffered = %d, want at most %d", buffered, maxBufferedLog+15)
	}

	// Release the append. Every retained byte must eventually flush; the
	// in-flight segment was either inside the buffer or parked in append
	// when the counters were read, so the delivered total is exactly
	// 4096 - discarded either way.
	close(block)
	delivered := 0
	if !waitFor(t, time.Second, func() bool {
		rec.mu.Lock()
		var n int
		for _, seg := range rec.segments {
			n += len(seg.Data)
		}
		rec.mu.Unlock()
		delivered = n
		return n == 4096-int(discarded)
	}) {
		t.Fatalf("retained prefix not fully flushed: delivered = %d, want %d", delivered, 4096-int(discarded))
	}
	rec.mu.Lock()
	joined := make([]byte, 0, delivered)
	for _, seg := range rec.segments {
		joined = append(joined, seg.Data...)
	}
	rec.mu.Unlock()
	if string(joined) != burst[:delivered] {
		t.Error("delivered bytes are not the contiguous write prefix (gap or reorder)")
	}
	b.Close() // must not hang
}

func TestTailBufferKeepsTail(t *testing.T) {
	tb := newTailBuffer(16)
	tb.Write([]byte("hello world"))
	tb.Write([]byte(", again"))
	if got := tb.Last(16); got != "llo world, again" {
		t.Errorf("tail = %q, want %q", got, "llo world, again")
	}
	if got := tb.Last(100); got != "llo world, again" {
		t.Errorf("tail beyond max = %q, want the capped buffer", got)
	}
}

// TestLogBufferDropsOnNonAdvancingConflict asserts a 409 whose resync
// offset does not advance the stream (the controller refuses the segment
// for good because the task became terminal) ends the log stream: the
// buffered tail is dropped instead of being retried in a tight loop.
func TestLogBufferDropsOnNonAdvancingConflict(t *testing.T) {
	rec := &logRecorder{}
	rec.errors = []error{&api.APIError{Status: http.StatusConflict}} // no offset field
	b := NewLogBuffer(rec.append, 16, time.Hour, nil)
	defer b.Close()
	if _, err := b.Write([]byte(strings.Repeat("x", 16))); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !waitFor(t, time.Second, func() bool { return rec.count() == 1 }) {
		t.Fatalf("first flush never happened")
	}
	// A retry after the drop would mean a second append call; give the
	// loop a moment and assert it did not happen.
	time.Sleep(50 * time.Millisecond)
	if rec.count() != 1 {
		t.Fatalf("appends = %d, want 1 (dropped tail must not be retried)", rec.count())
	}
	// The offset stays where it was: the server never acked the segment.
	if b.Offset() != 0 {
		t.Errorf("offset = %d, want 0", b.Offset())
	}
}

// TestLogBufferResyncStillRetries guards the other branch: a 409 that
// advances the offset (server holds more bytes) still retries from there
// (see TestLogBufferConflictResyncsOffset); the drop must only trigger on
// non-advancing conflicts.
func TestLogBufferResyncStillRetries(t *testing.T) {
	rec := &logRecorder{}
	rec.errors = []error{&api.APIError{Status: http.StatusConflict, Offset: 4}}
	b := NewLogBuffer(rec.append, 16, time.Hour, nil)
	defer b.Close()
	if _, err := b.Write([]byte(strings.Repeat("x", 16))); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !waitFor(t, time.Second, func() bool { return rec.count() >= 2 }) {
		t.Fatalf("retry after advancing 409 never happened")
	}
}

// TestLogBufferHeartbeatOnEmptyInterval covers the one-shot heartbeat
// path: a silent build (empty buffer at the interval tick) still reaches
// the controller through an empty segment carrying a progress sample, and
// a Cancelled acknowledgement on it closes the cancellation channel.
func TestLogBufferHeartbeatOnEmptyInterval(t *testing.T) {
	rec := &logRecorder{}
	rec.acks = []api.LogAck{{Cancelled: true}}
	prog := &api.TaskProgress{TaskID: "t-1", Stage: "makepkg", CPUTimeNS: 7}
	b := NewLogBuffer(rec.append, 1<<20, 10*time.Millisecond, func() *api.TaskProgress { return prog })
	defer b.Close()

	select {
	case <-b.Cancelled():
	case <-time.After(time.Second):
		t.Fatal("cancellation channel never closed: the heartbeat ack never arrived")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.segments) == 0 {
		t.Fatal("no heartbeat segment was appended")
	}
	seg := rec.segments[0]
	if seg.Data != "" {
		t.Errorf("heartbeat data = %q, want empty", seg.Data)
	}
	if seg.Progress == nil || seg.Progress.CPUTimeNS != 7 {
		t.Errorf("heartbeat progress = %+v, want the sample", seg.Progress)
	}
}

// TestLogBufferNoHeartbeatWithoutProgress asserts the heartbeat path is
// one-shot only: a buffer without a progress function (pool agents) never
// sends empty segments, because their heartbeats already carry the
// cancellation signal.
func TestLogBufferNoHeartbeatWithoutProgress(t *testing.T) {
	rec := &logRecorder{}
	b := NewLogBuffer(rec.append, 1<<20, 5*time.Millisecond, nil)
	time.Sleep(40 * time.Millisecond) // several interval ticks
	b.Close()
	if rec.count() != 0 {
		t.Errorf("appends = %d, want none without a progress function", rec.count())
	}
}

// TestLogBufferHeartbeatCloseDoesNotLoop asserts an empty Close does not
// turn the heartbeat path into an endless flush loop: the done-path drain
// only ever sends real data segments.
func TestLogBufferHeartbeatCloseDoesNotLoop(t *testing.T) {
	rec := &logRecorder{}
	prog := &api.TaskProgress{TaskID: "t-1"}
	b := NewLogBuffer(rec.append, 1<<20, time.Hour, func() *api.TaskProgress { return prog })
	time.Sleep(10 * time.Millisecond) // at least one tick with an empty buffer
	b.Close()
	for _, seg := range rec.segments {
		if seg.Data == "" {
			t.Errorf("done-path drain sent an empty segment: %+v", seg)
		}
	}
}
