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

// Package storage implements the unified file interface of the controller.
// It is the only channel through which the controller reads and writes
// repository files, staging area files and side files. Workers never use
// this module directly: all artifacts are relayed through the controller
// API.
//
// Two backends are provided: local (real filesystem, OpenLocal) and
// S3-compatible object stores (MinIO SDK, OpenS3). Both share one name
// validation rule and one interface contract, so callers behave the same
// regardless of the configured backend.
package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned by Get and Stat when the named object does not
// exist. Delete never returns it: deletion is idempotent and treats a
// missing object as success.
var ErrNotFound = errors.New("storage: object not found")

// FileInfo is the metadata returned by Stat.
type FileInfo struct {
	Size    int64
	ModTime time.Time
}

// Backend is the object-level file interface shared by both storage
// implementations. Names are virtual paths relative to the backend root:
// the root is flat for repository files ("<name>") and staging files live
// under "<stagingPrefix>/<taskID>/<name>" (see StagingPath).
//
// All methods are safe for concurrent use. Writes to the same name must be
// serialized by the caller (dispatch's single-writer mutex); the backend
// does not synchronize concurrent writers itself.
type Backend interface {
	// Put stores the complete content of r under name. size is the expected
	// byte count (may be -1 when unknown); the local backend ignores it.
	Put(ctx context.Context, name string, r io.Reader, size int64) error
	// Get streams the full content of name into w. It returns ErrNotFound
	// when the object does not exist.
	Get(ctx context.Context, name string, w io.Writer) error
	// Delete removes name. Deleting a missing object is a success.
	Delete(ctx context.Context, name string) error
	// List returns the names in the flat root area that match the glob
	// prefix. Staging area entries are never returned.
	List(ctx context.Context, prefix string) ([]string, error)
	// Stat returns the metadata of name, or ErrNotFound when missing.
	Stat(ctx context.Context, name string) (FileInfo, error)
	// StagingPath returns the virtual path of one task artifact in the
	// staging upload area: "<prefix>/<taskID>/<fileName>". The prefix and
	// the physical staging location are fixed per backend instance.
	StagingPath(taskID, fileName string) string
	// StagingDir returns the physical staging directory of the backend:
	// the local backend reports its configured directory (which may lie
	// outside the repository root), the s3 backend reports "" because an
	// object store has no physical staging tree.
	StagingDir() string
}

// Mover is an optional Backend capability for moving an object without a
// full copy through the controller: local renames the file, s3 degrades to
// Get+Put+Delete (non-atomic, safe under the single-writer mutex). Callers
// detect support with a type assertion.
type Mover interface {
	Move(ctx context.Context, src, dst string) error
}

// Appender is an optional Backend capability for resuming a segmented
// upload: it appends r to name assuming the stored size already equals
// offset. local appends with O_APPEND; s3 degrades to read-merge-reupload
// (correctness preserved, efficiency lost). Callers detect support with a
// type assertion.
type Appender interface {
	Append(ctx context.Context, name string, r io.Reader, offset int64) error
}
