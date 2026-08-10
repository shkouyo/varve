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
	"context"
	"errors"
	"io"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/storage"
)

// UploadFile streams one artifact segment into the task staging area
// (all artifacts pass through the controller). The client's offset must
// equal the current staged size (ErrConflict carrying the current offset
// otherwise, for resumable uploads). Claim-token protected. Concurrently
// safe.
func (o *OrchestratorImpl) UploadFile(ctx context.Context, taskID, token, name string, r io.Reader, size, offset int64) (*FileMeta, error) {
	if err := o.checkToken(ctx, taskID, token); err != nil {
		return nil, err
	}
	if _, err := o.store.GetTask(ctx, taskID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	staging := o.storage.StagingPath(taskID, name)
	current, err := o.stagingSize(ctx, staging)
	if err != nil {
		return nil, err
	}
	if current != offset {
		return nil, &OffsetError{Current: current}
	}
	if err := o.appendStaged(ctx, staging, r, offset); err != nil {
		return nil, err
	}
	fi, err := o.storage.Stat(ctx, staging)
	if err != nil {
		return nil, err
	}
	return &FileMeta{Name: name, Offset: fi.Size}, nil
}

// DownloadFile streams a staged file to the worker (source snapshot
// fallback and artifact verification downloads). ErrNotFound when the
// file does not exist. Claim-token protected. Concurrently safe.
func (o *OrchestratorImpl) DownloadFile(ctx context.Context, taskID, token, name string) (io.ReadCloser, error) {
	if err := o.checkToken(ctx, taskID, token); err != nil {
		return nil, err
	}
	if _, err := o.store.GetTask(ctx, taskID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	staging := o.storage.StagingPath(taskID, name)
	// Existence is checked up front so the 404 reaches the client before any
	// bytes are streamed; the read error path is still surfaced through the
	// pipe for races.
	if _, err := o.storage.Stat(ctx, staging); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		err := o.storage.Get(ctx, staging, pw)
		pw.CloseWithError(err)
	}()
	return pr, nil
}

// stagingSize returns the current size of a staged object (0 for a missing
// object, the initial offset of a segmented upload).
func (o *OrchestratorImpl) stagingSize(ctx context.Context, name string) (int64, error) {
	fi, err := o.storage.Stat(ctx, name)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return fi.Size, nil
}

// appendStaged appends r at the end of a staged object, preferring the
// Appender capability (local: O_APPEND; s3: read-merge-reupload) and
// degrading to read-merge-write when the backend lacks it.
func (o *OrchestratorImpl) appendStaged(ctx context.Context, name string, r io.Reader, offset int64) error {
	if app, ok := o.storage.(storage.Appender); ok {
		return app.Append(ctx, name, r, offset)
	}
	// Degraded path: read the existing object, merge, rewrite. The offset
	// was already validated against the current size by the caller.
	var buf byteBuffer
	if err := o.storage.Get(ctx, name, &buf); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if _, err := io.Copy(&buf, r); err != nil {
		return err
	}
	return o.storage.Put(ctx, name, &buf, int64(buf.Len()))
}

// byteBuffer wraps a plain []byte with io.Writer semantics.
type byteBuffer struct {
	b []byte
}

func (b *byteBuffer) Write(p []byte) (int, error) {
	b.b = append(b.b, p...)
	return len(p), nil
}

func (b *byteBuffer) Len() int { return len(b.b) }

// Read implements io.Reader for the Put fallback.
func (b *byteBuffer) Read(p []byte) (int, error) {
	if len(b.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.b)
	b.b = b.b[n:]
	return n, nil
}
