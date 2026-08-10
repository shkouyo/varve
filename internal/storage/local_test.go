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

package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// mustLocal opens a local backend under a fresh temp dir.
func mustLocal(t *testing.T) *localBackend {
	t.Helper()
	b, err := OpenLocal(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	return b.(*localBackend)
}

// TestLocalStagingDir asserts the configurable staging tree: staged files
// land in the configured physical directory (default <root>/staging,
// absolute paths used as-is, relative paths joined onto the root), the
// ingest-style Move relocates them into the flat root, and List never
// surfaces staging entries even when the tree lies under a differently
// named subdirectory of the root.
func TestLocalStagingDir(t *testing.T) {
	cases := []struct {
		name            string
		staging         string // "" = default
		wantRel         string // physical staging dir relative to a fresh root
		noStagingInRoot bool   // root must not gain a "staging" entry
	}{
		{"default", "", "staging", false},
		{"relative", "tmp/uploads", "tmp/uploads", false},
		{"absolute outside root", "OUTSIDE", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			staging := tc.staging
			if staging == "OUTSIDE" {
				staging = t.TempDir()
			}
			wantDir := filepath.Join(root, tc.wantRel)
			if tc.wantRel == "" {
				wantDir = staging
			}
			b, err := OpenLocal(root, staging)
			if err != nil {
				t.Fatalf("OpenLocal: %v", err)
			}
			lb := b.(*localBackend)
			if got := lb.StagingDir(); got != wantDir {
				t.Errorf("StagingDir() = %q, want %q", got, wantDir)
			}

			ctx := context.Background()
			const file = "pkg-1.0-1-x86_64.pkg.tar.zst"
			staged := lb.StagingPath("t-1", file)
			if err := b.Put(ctx, staged, strings.NewReader("staged"), 6); err != nil {
				t.Fatalf("Put staged: %v", err)
			}
			if _, err := os.Stat(filepath.Join(wantDir, "t-1", file)); err != nil {
				t.Errorf("staged file not under %s: %v", wantDir, err)
			}

			// Ingest-style move from staging into the flat root.
			m := b.(Mover)
			if err := m.Move(ctx, staged, file); err != nil {
				t.Fatalf("Move: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, file)); err != nil {
				t.Errorf("moved file missing from root: %v", err)
			}
			// The staged name is gone after the move.
			if err := b.Delete(ctx, staged); err != nil {
				t.Fatalf("Delete staged: %v", err)
			}
			if _, err := os.Stat(filepath.Join(wantDir, "t-1", file)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("staged file still present after move: %v", err)
			}

			// List only sees the flat root file.
			got, err := b.List(ctx, "*")
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(got) != 1 || got[0] != file {
				t.Errorf("List = %v, want only %q", got, file)
			}
			if tc.noStagingInRoot {
				if _, err := os.Stat(filepath.Join(root, "staging")); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("root gained a staging entry: %v", err)
				}
			}
		})
	}
}

// TestLocalOpenLocalValidation guards the OpenLocal contract: the root is
// required and created on demand.
func TestLocalOpenLocalValidation(t *testing.T) {
	if _, err := OpenLocal("", ""); err == nil {
		t.Error("OpenLocal(\"\") = nil error, want error")
	}
	root := t.TempDir() + "/nested/repo"
	if _, err := OpenLocal(root, ""); err != nil {
		t.Fatalf("OpenLocal(%q): %v", root, err)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		t.Errorf("root %q not created as directory: %v", root, err)
	}
}

// TestLocalStreamingMemoryBound asserts that Put and Get stream large
// objects in bounded chunks instead of loading them whole into memory: the
// reader never hands out more than copyBufSize bytes at once, and the
// writer never receives more.
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
// temp files behind (atomic write).
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

// TestLocalSymlinkEntries asserts the pacman-facing repository names
// (<name>.db / <name>.files, created by repo-add as symlinks to the gzip
// archives) are served like regular files: List returns the symlink
// entries, Get streams the archive bytes through the symlink and Stat
// reports the archive size.
func TestLocalSymlinkEntries(t *testing.T) {
	b := mustLocal(t)
	ctx := context.Background()

	const (
		dbArchive = "varve.db.tar.gz"
		dbName    = "varve.db"
		filesName = "varve.files"
	)
	archive := []byte("gzip-db-bytes")
	if err := b.Put(ctx, dbArchive, bytes.NewReader(archive), int64(len(archive))); err != nil {
		t.Fatalf("Put archive: %v", err)
	}
	// repo-add links <name>.db / <name>.files to the archives in the root.
	for _, pair := range [][2]string{{dbName, dbArchive}, {filesName, "varve.files.tar.gz"}} {
		if err := os.Symlink(pair[1], filepath.Join(b.root, pair[0])); err != nil {
			t.Fatalf("symlink %s: %v", pair[0], err)
		}
	}

	// List returns the symlink entries, not only the archives.
	got, err := b.List(ctx, "*")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, name := range []string{dbName, dbArchive, filesName} {
		if !slices.Contains(got, name) {
			t.Errorf("List = %v, want it to include %s", got, name)
		}
	}

	// Get through the symlink streams the archive bytes.
	var buf bytes.Buffer
	if err := b.Get(ctx, dbName, &buf); err != nil {
		t.Fatalf("Get %s: %v", dbName, err)
	}
	if !bytes.Equal(buf.Bytes(), archive) {
		t.Errorf("Get %s = %q, want the archive bytes %q", dbName, buf.Bytes(), archive)
	}

	// Stat through the symlink reports the archive size.
	fi, err := b.Stat(ctx, dbName)
	if err != nil {
		t.Fatalf("Stat %s: %v", dbName, err)
	}
	if fi.Size != int64(len(archive)) {
		t.Errorf("Stat %s size = %d, want %d", dbName, fi.Size, len(archive))
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
