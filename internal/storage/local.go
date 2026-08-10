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
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// copyBufSize bounds the per-call read/write buffer used by the streaming
// copy helpers, so that large objects never load more than this many bytes
// into memory at once (memory-cap test asserts this bound).
const copyBufSize = 256 << 10

// stagingPrefix is the fixed virtual path prefix of the staging tree on the
// local backend. The physical location is stagingDir; the virtual names
// keep this prefix so the staging namespace is stable and documented.
const stagingPrefix = "staging"

// localBackend implements Backend over a real filesystem directory. Names
// are validated and joined onto the backend root; all operations are
// confined to the root by construction. Staging names (virtual prefix
// "staging/") resolve onto the configured staging directory instead, which
// may lie outside the root.
type localBackend struct {
	root       string
	stagingDir string
	// stagingRel is the staging directory relative to the root, or "" when
	// the staging tree lies outside the root (then no subtree under the
	// root needs skipping during List).
	stagingRel string
}

// OpenLocal returns a Backend rooted at root (typically "/data/repo") with
// its staging upload tree at stagingDir. An empty stagingDir keeps the
// default <root>/staging. Both directories are created if missing.
func OpenLocal(root, stagingDir string) (Backend, error) {
	if root == "" {
		return nil, errors.New("storage: empty local root")
	}
	if stagingDir == "" {
		stagingDir = filepath.Join(root, "staging")
	} else if !filepath.IsAbs(stagingDir) {
		stagingDir = filepath.Join(root, stagingDir)
	}
	stagingDir = filepath.Clean(stagingDir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("storage: create local root %q: %w", root, err)
	}
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return nil, fmt.Errorf("storage: create local staging dir %q: %w", stagingDir, err)
	}
	stagingRel := ""
	if rel, err := filepath.Rel(root, stagingDir); err == nil && rel != "." &&
		rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		stagingRel = rel
	}
	return &localBackend{root: root, stagingDir: stagingDir, stagingRel: stagingRel}, nil
}

// StagingPath returns the virtual staging path of a task artifact:
// "staging/<taskID>/<fileName>", which resolves onto the configured
// staging directory.
func (b *localBackend) StagingPath(taskID, fileName string) string {
	return stagingPrefix + "/" + taskID + "/" + fileName
}

// StagingDir returns the physical staging directory of the backend.
func (b *localBackend) StagingDir() string { return b.stagingDir }

// resolve validates name and joins it onto the backend root, or onto the
// staging directory for staging-prefixed names.
func (b *localBackend) resolve(name string) (string, error) {
	if !validName(name) {
		return "", fmt.Errorf("storage: invalid name %q", name)
	}
	if strings.HasPrefix(name, stagingPrefix+"/") {
		rest := strings.TrimPrefix(name, stagingPrefix+"/")
		return filepath.Join(b.stagingDir, filepath.FromSlash(rest)), nil
	}
	return filepath.Join(b.root, filepath.FromSlash(name)), nil
}

// Put writes r atomically: the content is first written to a sibling temp
// file, fsynced, then renamed over the target. A crash may leave the temp
// file behind; the caller's staging sweep is responsible for it. size is
// informational and ignored.
func (b *localBackend) Put(ctx context.Context, name string, r io.Reader, size int64) error {
	target, err := b.resolve(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("storage: put %q: create parent: %w", name, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".tmp.*")
	if err != nil {
		return fmt.Errorf("storage: put %q: create temp: %w", name, err)
	}
	_, err = copyContext(ctx, tmp, r)
	if err == nil {
		err = tmp.Sync() // fsync before rename: only durable data is published
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("storage: put %q: %w", name, err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("storage: put %q: rename: %w", name, err)
	}
	return nil
}

// Get streams the full content of name into w. It returns ErrNotFound when
// the object does not exist.
func (b *localBackend) Get(ctx context.Context, name string, w io.Writer) error {
	target, err := b.resolve(name)
	if err != nil {
		return err
	}
	f, err := os.Open(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("storage: get %q: %w", name, ErrNotFound)
		}
		return fmt.Errorf("storage: get %q: %w", name, err)
	}
	defer f.Close()
	if _, err := copyContext(ctx, w, f); err != nil {
		return fmt.Errorf("storage: get %q: %w", name, err)
	}
	return nil
}

// Delete removes name. Deleting a missing object is a success (idempotent).
func (b *localBackend) Delete(ctx context.Context, name string) error {
	target, err := b.resolve(name)
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("storage: delete %q: %w", name, err)
	}
	return nil
}

// List walks the root and returns the file names in the flat root area that
// match the glob prefix. The staging tree (the physical staging directory
// when it lies under the root) is skipped, so staging entries never appear
// in results.
func (b *localBackend) List(ctx context.Context, prefix string) ([]string, error) {
	var names []string
	err := filepath.WalkDir(b.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(b.root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if b.stagingRel != "" && (rel == b.stagingRel || strings.HasPrefix(rel, b.stagingRel+string(filepath.Separator))) {
				return filepath.SkipDir
			}
			return nil
		}
		// Only the flat root area is listed; subdirectories other than
		// staging are not part of the documented layout.
		if strings.Contains(rel, string(filepath.Separator)) {
			return nil
		}
		rel = filepath.ToSlash(rel)
		ok, err := path.Match(prefix, rel)
		if err != nil {
			return fmt.Errorf("storage: list: bad glob %q: %w", prefix, err)
		}
		if ok {
			names = append(names, rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("storage: list %q: %w", prefix, err)
	}
	return names, nil
}

// Stat returns {Size, ModTime} of name, or ErrNotFound when missing.
func (b *localBackend) Stat(ctx context.Context, name string) (FileInfo, error) {
	target, err := b.resolve(name)
	if err != nil {
		return FileInfo{}, err
	}
	fi, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return FileInfo{}, fmt.Errorf("storage: stat %q: %w", name, ErrNotFound)
		}
		return FileInfo{}, fmt.Errorf("storage: stat %q: %w", name, err)
	}
	if fi.IsDir() {
		return FileInfo{}, fmt.Errorf("storage: stat %q: is a directory", name)
	}
	return FileInfo{Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// Move renames src onto dst. rename is atomic on the same filesystem and
// the source disappears afterwards (Mover capability).
func (b *localBackend) Move(ctx context.Context, src, dst string) error {
	srcPath, err := b.resolve(src)
	if err != nil {
		return err
	}
	dstPath, err := b.resolve(dst)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return fmt.Errorf("storage: move %q -> %q: create parent: %w", src, dst, err)
	}
	if err := os.Rename(srcPath, dstPath); err != nil {
		return fmt.Errorf("storage: move %q -> %q: %w", src, dst, err)
	}
	return nil
}

// Append appends the content of r at the end of name (Appender capability).
// The caller guarantees offset == current size (pre-checked); as a
// defensive guard the backend verifies it and refuses a mismatch. O_APPEND
// makes every write land at the end of the file, so a verified offset is
// appended to directly.
func (b *localBackend) Append(ctx context.Context, name string, r io.Reader, offset int64) error {
	target, err := b.resolve(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("storage: append %q: create parent: %w", name, err)
	}
	if err := verifyOffset(target, offset); err != nil {
		return fmt.Errorf("storage: append %q: %w", name, err)
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("storage: append %q: %w", name, err)
	}
	if _, err := copyContext(ctx, f, r); err != nil {
		f.Close()
		return fmt.Errorf("storage: append %q: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("storage: append %q: %w", name, err)
	}
	return nil
}

// verifyOffset checks that target does not exist (offset must be 0) or that
// its size equals offset, matching the Append precondition.
func verifyOffset(target string, offset int64) error {
	fi, err := os.Stat(target)
	if errors.Is(err, fs.ErrNotExist) {
		if offset != 0 {
			return fmt.Errorf("offset %d on missing object, want 0", offset)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Size() != offset {
		return fmt.Errorf("offset %d does not match current size %d", offset, fi.Size())
	}
	return nil
}

// copyContext streams src into dst in bounded chunks, honoring ctx
// cancellation between reads.
func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, copyBufSize)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}
