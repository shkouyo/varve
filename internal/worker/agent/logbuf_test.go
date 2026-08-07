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
}

func (r *logRecorder) append(seg api.LogSegment) (*api.LogAck, error) {
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
// batches — a single oversized segment would exceed the controller's
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
