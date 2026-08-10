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
	"bytes"
	"errors"
	"sync"
	"testing"
)

// TestLogsAppendRead covers the basic lifecycle: append creates the log,
// read returns the full content and missing logs yield ErrNotFound.
func TestLogsAppendRead(t *testing.T) {
	l := NewLogs(t.TempDir())
	if _, err := l.Read("1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(missing) = %v, want ErrNotFound", err)
	}
	if _, err := l.Size("1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Size(missing) = %v, want ErrNotFound", err)
	}
	if err := l.Append("1", []byte("hello ")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := l.Append("1", []byte("world\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := l.Read("1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "hello world\n" {
		t.Errorf("Read = %q, want %q", got, "hello world\n")
	}
	size, err := l.Size("1")
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if size != int64(len("hello world\n")) {
		t.Errorf("Size = %d, want %d", size, len("hello world\n"))
	}
	if p := l.Path("1"); p != "logs/1.log" {
		t.Errorf("Path = %q, want logs/1.log", p)
	}
}

// TestLogsTailFrom covers the incremental read semantics used by SSE: the
// returned offset is the absolute new position and repeated tails continue
// from there.
func TestLogsTailFrom(t *testing.T) {
	l := NewLogs(t.TempDir())
	if err := l.Append("2", []byte("0123456789")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	var buf bytes.Buffer
	off, err := l.TailFrom("2", 4, &buf, 0)
	if err != nil {
		t.Fatalf("TailFrom: %v", err)
	}
	if buf.String() != "456789" {
		t.Errorf("TailFrom(4) = %q, want 456789", buf.String())
	}
	if off != 10 {
		t.Errorf("new offset = %d, want 10", off)
	}
	buf.Reset()
	off, err = l.TailFrom("2", off, &buf, 0)
	if err != nil {
		t.Fatalf("TailFrom: %v", err)
	}
	if buf.Len() != 0 || off != 10 {
		t.Errorf("tail at end = %q/%d, want empty/10", buf.String(), off)
	}
	if _, err := l.TailFrom("2", -1, &buf, 0); err == nil {
		t.Error("TailFrom(negative) succeeded, want error")
	}
	if _, err := l.TailFrom("nope", 0, &buf, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("TailFrom(missing) = %v, want ErrNotFound", err)
	}
}

// TestLogsTailFromLimit covers the limit parameter: a positive limit stops
// the read after that many bytes with the offset advanced accordingly, a
// non-positive limit streams to the end, and both resume seamlessly.
func TestLogsTailFromLimit(t *testing.T) {
	l := NewLogs(t.TempDir())
	if err := l.Append("3", []byte("0123456789")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	var buf bytes.Buffer

	// A limited read returns only the first limit bytes.
	off, err := l.TailFrom("3", 0, &buf, 4)
	if err != nil {
		t.Fatalf("TailFrom(limit 4): %v", err)
	}
	if buf.String() != "0123" || off != 4 {
		t.Errorf("TailFrom(limit 4) = %q/%d, want 0123/4", buf.String(), off)
	}

	// The next read resumes from the limited offset.
	buf.Reset()
	off, err = l.TailFrom("3", off, &buf, 4)
	if err != nil {
		t.Fatalf("TailFrom(resume, limit 4): %v", err)
	}
	if buf.String() != "4567" || off != 8 {
		t.Errorf("TailFrom(resume, limit 4) = %q/%d, want 4567/8", buf.String(), off)
	}

	// A limit past the remainder clamps to the end.
	buf.Reset()
	off, err = l.TailFrom("3", off, &buf, 100)
	if err != nil {
		t.Fatalf("TailFrom(limit 100): %v", err)
	}
	if buf.String() != "89" || off != 10 {
		t.Errorf("TailFrom(limit 100) = %q/%d, want 89/10", buf.String(), off)
	}

	// Non-positive limits stream the whole remainder.
	buf.Reset()
	off, err = l.TailFrom("3", 0, &buf, 0)
	if err != nil {
		t.Fatalf("TailFrom(limit 0): %v", err)
	}
	if buf.String() != "0123456789" || off != 10 {
		t.Errorf("TailFrom(limit 0) = %q/%d, want the full log/10", buf.String(), off)
	}
	buf.Reset()
	off, err = l.TailFrom("3", 0, &buf, -1)
	if err != nil {
		t.Fatalf("TailFrom(limit -1): %v", err)
	}
	if buf.String() != "0123456789" || off != 10 {
		t.Errorf("TailFrom(limit -1) = %q/%d, want the full log/10", buf.String(), off)
	}
}

// TestLogsConcurrentAppend covers the per-build mutex: N concurrent appends
// must not lose or interleave bytes.
func TestLogsConcurrentAppend(t *testing.T) {
	l := NewLogs(t.TempDir())
	const n = 32
	chunk := []byte("0123456789abcdef")
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Append("c", chunk); err != nil {
				t.Errorf("concurrent Append: %v", err)
			}
		}()
	}
	wg.Wait()
	got, err := l.Read("c")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != n*len(chunk) {
		t.Fatalf("log length = %d, want %d (no bytes lost)", len(got), n*len(chunk))
	}
	// The content is a concatenation of whole chunks (never interleaved).
	for i := 0; i < len(got); i += len(chunk) {
		if string(got[i:i+len(chunk)]) != string(chunk) {
			t.Fatalf("interleaved append at %d: %q", i, got[i:i+len(chunk)])
		}
	}
}

// TestLogsDelete covers idempotent deletion.
func TestLogsDelete(t *testing.T) {
	l := NewLogs(t.TempDir())
	if err := l.Append("9", []byte("x")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := l.Delete("9"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := l.Delete("9"); err != nil {
		t.Errorf("Delete(missing) = %v, want nil", err)
	}
	if _, err := l.Read("9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read after Delete = %v, want ErrNotFound", err)
	}
}
