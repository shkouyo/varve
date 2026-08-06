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
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"git.0x0f.dev/varve/internal/config"
)

// objectInfo is the metadata of a stored object.
type objectInfo struct {
	key     string
	size    int64
	modTime time.Time
}

// objectListPage is one page of a paged object listing: objects are keys
// that start with the requested prefix; nextToken is the continuation token
// to pass to the next call, empty on the last page.
type objectListPage struct {
	objects   []objectInfo
	nextToken string
}

// objectAPI is the narrow object-store surface the s3 backend needs. It
// narrows minio.Client to the five operations used here and is the test
// double point: tests substitute an in-memory fake.
//
// ListObjects deliberately deviates from minio.Client's channel API: it
// returns one explicit page plus a continuation token so that pagination is
// driven — and testable — in this package. The real adapter drains
// minio-go's internally-paged ListObjectsV2 stream into one page.
type objectAPI interface {
	// PutObject stores an object. size < 0 means the length is unknown and
	// the reader is streamed until EOF.
	PutObject(ctx context.Context, bucket, key string, r io.Reader, size int64) error
	// GetObject returns the object content. A missing object surfaces as an
	// S3 NoSuchKey error either on the call or on the first read of the
	// returned reader.
	GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error)
	// DeleteObject removes an object. Deleting a missing object succeeds
	// (S3 DeleteObject is idempotent).
	DeleteObject(ctx context.Context, bucket, key string) error
	// ListObjects returns one page of objects whose keys start with prefix,
	// continuing from token (empty on the first call).
	ListObjects(ctx context.Context, bucket, prefix, token string) (objectListPage, error)
	// StatObject returns object metadata; a missing object yields a
	// NoSuchKey error.
	StatObject(ctx context.Context, bucket, key string) (objectInfo, error)
}

// s3Client adapts minio.Client to objectAPI.
type s3Client struct {
	c *minio.Client
}

func (s *s3Client) PutObject(ctx context.Context, bucket, key string, r io.Reader, size int64) error {
	_, err := s.c.PutObject(ctx, bucket, key, r, size, minio.PutObjectOptions{})
	return err
}

func (s *s3Client) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	return s.c.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
}

func (s *s3Client) DeleteObject(ctx context.Context, bucket, key string) error {
	return s.c.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{})
}

func (s *s3Client) ListObjects(ctx context.Context, bucket, prefix, token string) (objectListPage, error) {
	// minio-go pages internally via ListObjectsV2 + continuation tokens;
	// drain the whole stream into a single page.
	ch := s.c.ListObjects(ctx, bucket, minio.ListObjectsOptions{
		Prefix:     prefix,
		Recursive:  true,
		StartAfter: token,
	})
	page := objectListPage{}
	for obj := range ch {
		if obj.Err != nil {
			return objectListPage{}, obj.Err
		}
		page.objects = append(page.objects, objectInfo{
			key:     obj.Key,
			size:    obj.Size,
			modTime: obj.LastModified,
		})
	}
	return page, nil
}

func (s *s3Client) StatObject(ctx context.Context, bucket, key string) (objectInfo, error) {
	info, err := s.c.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return objectInfo{}, err
	}
	return objectInfo{key: info.Key, size: info.Size, modTime: info.LastModified}, nil
}

// s3Backend implements Backend over an S3-compatible object store. Object
// keys are the virtual paths; the bucket root corresponds to the repository
// root.
type s3Backend struct {
	client objectAPI
	bucket string
}

// OpenS3 returns a Backend backed by an S3-compatible object store
// (MinIO / SeaweedFS / Ceph RGW / Cloudflare R2). The client is constructed
// from the controller's S3 configuration; no network I/O happens until the
// first operation. Multipart uploads are handled automatically by
// minio-go.
func OpenS3(cfg config.S3Config) (Backend, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("storage: s3 endpoint and bucket are required")
	}
	lookup := minio.BucketLookupPath
	if !cfg.PathStyle {
		lookup = minio.BucketLookupDNS
	}
	// minio-go builds the endpoint itself and selects the scheme via the
	// Secure option, so the configured URL's scheme must be stripped first
	// (the config may write it with a scheme, e.g. https://s3.example.org).
	endpoint, secure := splitEndpoint(cfg.Endpoint)
	c, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       secure,
		Region:       cfg.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("storage: open s3 client: %w", err)
	}
	return &s3Backend{client: &s3Client{c: c}, bucket: cfg.Bucket}, nil
}

// Put stores the content of r under name. size is passed through to the
// object store (unknown when negative).
func (b *s3Backend) Put(ctx context.Context, name string, r io.Reader, size int64) error {
	if !validName(name) {
		return fmt.Errorf("storage: invalid name %q", name)
	}
	if err := b.client.PutObject(ctx, b.bucket, name, r, size); err != nil {
		return fmt.Errorf("storage: put %q in bucket %q: %w", name, b.bucket, err)
	}
	return nil
}

// Get streams the full content of name into w. A missing object maps to
// ErrNotFound (the 404 may surface lazily on the first read).
func (b *s3Backend) Get(ctx context.Context, name string, w io.Writer) error {
	if !validName(name) {
		return fmt.Errorf("storage: invalid name %q", name)
	}
	obj, err := b.client.GetObject(ctx, b.bucket, name)
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("storage: get %q: %w", name, ErrNotFound)
		}
		return fmt.Errorf("storage: get %q in bucket %q: %w", name, b.bucket, err)
	}
	defer obj.Close()
	if _, err := copyContext(ctx, w, obj); err != nil {
		if isNotFound(err) {
			return fmt.Errorf("storage: get %q: %w", name, ErrNotFound)
		}
		return fmt.Errorf("storage: get %q: %w", name, err)
	}
	return nil
}

// Delete removes name; deleting a missing object succeeds.
func (b *s3Backend) Delete(ctx context.Context, name string) error {
	if !validName(name) {
		return fmt.Errorf("storage: invalid name %q", name)
	}
	if err := b.client.DeleteObject(ctx, b.bucket, name); err != nil {
		return fmt.Errorf("storage: delete %q in bucket %q: %w", name, b.bucket, err)
	}
	return nil
}

// List returns the flat-root names matching the glob prefix: the server
// lists objects under the literal prefix extracted from the glob, results
// are filtered client-side with path.Match, and staging entries are never
// returned. Pagination follows the continuation token until the last page.
func (b *s3Backend) List(ctx context.Context, prefix string) ([]string, error) {
	literal := globLiteralPrefix(prefix)
	var names []string
	token := ""
	for {
		page, err := b.client.ListObjects(ctx, b.bucket, literal, token)
		if err != nil {
			return nil, fmt.Errorf("storage: list %q in bucket %q: %w", prefix, b.bucket, err)
		}
		for _, obj := range page.objects {
			if strings.HasPrefix(obj.key, "staging/") {
				continue // the staging tree is never listed
			}
			ok, err := path.Match(prefix, obj.key)
			if err != nil {
				return nil, fmt.Errorf("storage: list %q: bad glob: %w", prefix, err)
			}
			if ok {
				names = append(names, obj.key)
			}
		}
		if page.nextToken == "" {
			break
		}
		token = page.nextToken
	}
	return names, nil
}

// Stat returns {Size, ModTime} of name, or ErrNotFound when missing.
func (b *s3Backend) Stat(ctx context.Context, name string) (FileInfo, error) {
	if !validName(name) {
		return FileInfo{}, fmt.Errorf("storage: invalid name %q", name)
	}
	info, err := b.client.StatObject(ctx, b.bucket, name)
	if err != nil {
		if isNotFound(err) {
			return FileInfo{}, fmt.Errorf("storage: stat %q: %w", name, ErrNotFound)
		}
		return FileInfo{}, fmt.Errorf("storage: stat %q in bucket %q: %w", name, b.bucket, err)
	}
	return FileInfo{Size: info.size, ModTime: info.modTime}, nil
}

// Move degrades to Get+Put+Delete: the move is non-atomic; consistency is
// guaranteed by the caller's single-writer mutex. The source handle is
// fetched synchronously first, then the content is streamed through an
// io.Pipe into the destination object, so it is never buffered whole in
// memory and the call order is deterministic (Get -> Put -> Delete).
func (b *s3Backend) Move(ctx context.Context, src, dst string) error {
	if !validName(src) || !validName(dst) {
		return fmt.Errorf("storage: invalid move %q -> %q", src, dst)
	}
	obj, err := b.client.GetObject(ctx, b.bucket, src)
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("storage: move %q -> %q: %w", src, dst, ErrNotFound)
		}
		return fmt.Errorf("storage: move %q -> %q: get: %w", src, dst, err)
	}
	defer obj.Close()

	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		_, err := copyContext(ctx, pw, obj)
		pw.CloseWithError(err) // nil is a clean close
		errCh <- err
	}()
	putErr := b.client.PutObject(ctx, b.bucket, dst, pr, -1)
	pr.Close()
	copyErr := <-errCh
	if copyErr != nil && isNotFound(copyErr) {
		return fmt.Errorf("storage: move %q -> %q: %w", src, dst, ErrNotFound)
	}
	if putErr != nil {
		return fmt.Errorf("storage: move %q -> %q: put: %w", src, dst, putErr)
	}
	if copyErr != nil {
		return fmt.Errorf("storage: move %q -> %q: get: %w", src, dst, copyErr)
	}
	return b.client.DeleteObject(ctx, b.bucket, src)
}

// Append merges the stored object with r and re-uploads it under name
// (Appender capability). This is the degraded resume path for s3:
// correctness is preserved, efficiency is lost — every chunk re-uploads the
// whole object. The caller pre-checks offset == stored size; the backend
// re-checks it defensively. The existing content is buffered in memory
// (bounded by the staging object size).
func (b *s3Backend) Append(ctx context.Context, name string, r io.Reader, offset int64) error {
	if !validName(name) {
		return fmt.Errorf("storage: invalid name %q", name)
	}
	var existing bytes.Buffer
	if err := b.Get(ctx, name, &existing); err != nil {
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if offset != 0 {
			return fmt.Errorf("storage: append %q: offset %d on missing object, want 0", name, offset)
		}
	} else if int64(existing.Len()) != offset {
		return fmt.Errorf("storage: append %q: offset %d does not match current size %d", name, offset, existing.Len())
	}
	merged := io.MultiReader(bytes.NewReader(existing.Bytes()), r)
	return b.Put(ctx, name, merged, -1)
}

// splitEndpoint strips the optional scheme from a configured S3 endpoint
// URL and reports whether it was https (minio-go selects the scheme via its
// Secure option). Without a scheme the default is plain http.
func splitEndpoint(raw string) (host string, secure bool) {
	host = raw
	if strings.HasPrefix(host, "https://") {
		return strings.TrimPrefix(host, "https://"), true
	}
	if strings.HasPrefix(host, "http://") {
		return strings.TrimPrefix(host, "http://"), false
	}
	return host, false
}

// isNotFound reports whether err is an S3 NoSuchKey response, which Get and
// Stat map to ErrNotFound.
func isNotFound(err error) bool {
	var resp minio.ErrorResponse
	return errors.As(err, &resp) && resp.Code == "NoSuchKey"
}
