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

package storage

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustLocal opens a local backend under a fresh temp dir.
func mustLocal(t *testing.T) *localBackend {
	t.Helper()
	b, err := OpenLocal(t.TempDir())
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	return b.(*localBackend)
}

// TestLocalOpenLocalValidation guards the OpenLocal contract: the root is
// required and created on demand.
func TestLocalOpenLocalValidation(t *testing.T) {
	if _, err := OpenLocal(""); err == nil {
		t.Error("OpenLocal(\"\") = nil error, want error")
	}
	root := t.TempDir() + "/nested/repo"
	if _, err := OpenLocal(root); err != nil {
		t.Fatalf("OpenLocal(%q): %v", root, err)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		t.Errorf("root %q not created as directory: %v", root, err)
	}
}

// TestLocalStreamingMemoryBound asserts that Put and Get stream large
// objects in bounded chunks instead of loading them whole into memory
// (DETAIL §5.7, memory-cap assertion): the reader never hands out more than
// copyBufSize bytes at once, and the writer never receives more.
func TestLocalStreamingMemoryBound(t *testing.T) {
	b := mustLocal(t)
	ctx := context.Background()

	const size = 8 << 20 // 8 MiB, far above the copy buffer
	big := make([]byte, size)
	for i := range big {
		big[i] = byte(i % 251)
	}

	src := &trackReader{r: bytes.NewReader(big)}
	if err := b.Put(ctx, "big.pkg.tar.zst", src, size); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if src.maxBuf > copyBufSize {
		t.Errorf("Put read max %d bytes per call, want <= %d", src.maxBuf, copyBufSize)
	}

	var sink trackWriter
	if err := b.Get(ctx, "big.pkg.tar.zst", &sink); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sink.maxBuf > copyBufSize {
		t.Errorf("Get wrote max %d bytes per call, want <= %d", sink.maxBuf, copyBufSize)
	}
	if !bytes.Equal(sink.b.Bytes(), big) {
		t.Error("large round trip content mismatch")
	}
}

// TestLocalAtomicWriteNoTmpLeftover asserts that successful Puts leave no
// temp files behind (atomic write, DETAIL §5.4).
func TestLocalAtomicWriteNoTmpLeftover(t *testing.T) {
	b := mustLocal(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		content := strings.Repeat("x", i*137)
		if err := b.Put(ctx, "pkg-"+string(rune('a'+i))+".pkg.tar.zst", strings.NewReader(content), int64(len(content))); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	err := filepath.WalkDir(b.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.Contains(d.Name(), ".tmp.") {
			t.Errorf("leftover temp file: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// trackReader wraps a reader and records the largest buffer it was asked to
// fill.
type trackReader struct {
	r      io.Reader
	maxBuf int
}

func (t *trackReader) Read(p []byte) (int, error) {
	if len(p) > t.maxBuf {
		t.maxBuf = len(p)
	}
	return t.r.Read(p)
}

// trackWriter wraps a buffer and records the largest write it received.
type trackWriter struct {
	b      bytes.Buffer
	maxBuf int
}

func (t *trackWriter) Write(p []byte) (int, error) {
	if len(p) > t.maxBuf {
		t.maxBuf = len(p)
	}
	return t.b.Write(p)
}
