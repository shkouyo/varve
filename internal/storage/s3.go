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
// driven and testable in this package. The token is opaque: the caller
// passes the previous page's token back unchanged (the real adapter
// resumes after the previous page's last key; the fake mirrors that).
type objectAPI interface {
	// PutObject stores an object. size < 0 means the length is unknown and
	// the reader is streamed until EOF; contentType is stored as the
	// object's Content-Type header.
	PutObject(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) error
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

func (s *s3Client) PutObject(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) error {
	_, err := s.c.PutObject(ctx, bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *s3Client) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	return s.c.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
}

func (s *s3Client) DeleteObject(ctx context.Context, bucket, key string) error {
	return s.c.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{})
}

// s3ListPageSize bounds one ListObjects page. The total listing still
// enumerates every object under the prefix (glob filtering must see all
// of them), but memory stays at one page of metadata.
const s3ListPageSize = 1000

func (s *s3Client) ListObjects(ctx context.Context, bucket, prefix, token string) (objectListPage, error) {
	// ListObjectsIter yields synchronously (no goroutine to leak) and
	// pages internally via ListObjectsV2 continuation tokens. The
	// iterator does not expose the token, so the next page resumes with
	// StartAfter = the last key of this page (V2 start-after semantics;
	// keys sort stably). MaxKeys bounds each request.
	page := objectListPage{}
	for obj := range s.c.ListObjectsIter(ctx, bucket, minio.ListObjectsOptions{
		Prefix:     prefix,
		Recursive:  true,
		MaxKeys:    s3ListPageSize,
		StartAfter: token,
	}) {
		if obj.Err != nil {
			return objectListPage{}, obj.Err
		}
		page.objects = append(page.objects, objectInfo{
			key:     obj.Key,
			size:    obj.Size,
			modTime: obj.LastModified,
		})
		if len(page.objects) == s3ListPageSize {
			break
		}
	}
	if n := len(page.objects); n == s3ListPageSize {
		// A full page may continue after its last key; the follow-up
		// request returns an empty page when the listing is exhausted.
		page.nextToken = page.objects[n-1].key
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

// s3ContentTypes maps object-key suffixes onto the Content-Type stored with
// each object. Longer, more specific suffixes are listed first and win; a
// key that matches none of them falls back to application/octet-stream.
// Pacman database objects are gzip archives in both their bare (.db,
// .files) and .tar.gz forms.
var s3ContentTypes = []struct {
	suffix string
	typ    string
}{
	{".pkg.tar.zst", "application/zstd"},
	{".src.tar.zst", "application/zstd"},
	{".zst", "application/zstd"},
	{".db.tar.gz", "application/gzip"},
	{".files.tar.gz", "application/gzip"},
	{".db", "application/gzip"},
	{".files", "application/gzip"},
	{".sig", "application/pgp-signature"},
	{".toml", "text/plain; charset=utf-8"},
	{".SRCINFO", "text/plain; charset=utf-8"},
	{".txt", "text/plain; charset=utf-8"},
	{".log", "text/plain; charset=utf-8"},
}

// contentTypeForKey infers the Content-Type of an object from its key
// suffix. It operates on the final object key, so staging uploads, flat
// repository objects and repository-prefixed keys are covered alike.
func contentTypeForKey(key string) string {
	for _, m := range s3ContentTypes {
		if strings.HasSuffix(key, m.suffix) {
			return m.typ
		}
	}
	return "application/octet-stream"
}

// s3Backend implements Backend over an S3-compatible object store. Object
// keys are the virtual paths. Repository names map onto keys under the
// configured repository prefix (the bucket root when it is empty); staging
// names carry the configured staging prefix instead of the default
// "staging".
type s3Backend struct {
	client        objectAPI
	bucket        string
	stagingPrefix string
	repoPrefix    string
}

// OpenS3 returns a Backend backed by an S3-compatible object store
// (MinIO / SeaweedFS / Ceph RGW / Cloudflare R2). The client is constructed
// from the controller's S3 configuration; no network I/O happens until the
// first operation. Multipart uploads are handled automatically by
// minio-go. An empty staging prefix keeps the default "staging"; an empty
// repository prefix keeps the bucket root as the repository area.
func OpenS3(cfg config.S3Config) (Backend, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("storage: s3 endpoint and bucket are required")
	}
	stagingPrefix := cfg.StagingPrefix
	if stagingPrefix == "" {
		stagingPrefix = "staging"
	}
	if !validName(stagingPrefix) {
		return nil, fmt.Errorf("storage: invalid s3 staging prefix %q", stagingPrefix)
	}
	if cfg.RepoPrefix != "" && !validName(cfg.RepoPrefix) {
		return nil, fmt.Errorf("storage: invalid s3 repo prefix %q", cfg.RepoPrefix)
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
	return &s3Backend{client: &s3Client{c: c}, bucket: cfg.Bucket, stagingPrefix: stagingPrefix, repoPrefix: cfg.RepoPrefix}, nil
}

// StagingPath returns the object key of a task artifact in the staging
// upload area: "<stagingPrefix>/<taskID>/<fileName>".
func (b *s3Backend) StagingPath(taskID, fileName string) string {
	return b.stagingPrefix + "/" + taskID + "/" + fileName
}

// StagingDir returns "" for the s3 backend: an object store has no
// physical staging directory.
func (b *s3Backend) StagingDir() string { return "" }

// repoKey maps a flat repository name onto its object key: repository
// names are prefixed with the configured repo prefix, which is the bucket
// root when empty.
func (b *s3Backend) repoKey(name string) string {
	if b.repoPrefix == "" {
		return name
	}
	return b.repoPrefix + "/" + name
}

// resolve maps a caller-visible name onto its object key. Names under the
// staging prefix namespace (produced by StagingPath) are staging keys and
// are used as-is; every other name is a repository name mapped through
// repoKey.
func (b *s3Backend) resolve(name string) string {
	if strings.HasPrefix(name, b.stagingPrefix+"/") {
		return name
	}
	return b.repoKey(name)
}

// Put stores the content of r under name. size is passed through to the
// object store (unknown when negative). The Content-Type is inferred from
// the resolved object key's suffix, so staging uploads and flat repository
// objects are typed the same way.
func (b *s3Backend) Put(ctx context.Context, name string, r io.Reader, size int64) error {
	if !validName(name) {
		return fmt.Errorf("storage: invalid name %q", name)
	}
	key := b.resolve(name)
	if err := b.client.PutObject(ctx, b.bucket, key, r, size, contentTypeForKey(key)); err != nil {
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
	key := b.resolve(name)
	obj, err := b.client.GetObject(ctx, b.bucket, key)
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
	key := b.resolve(name)
	if err := b.client.DeleteObject(ctx, b.bucket, key); err != nil {
		return fmt.Errorf("storage: delete %q in bucket %q: %w", name, b.bucket, err)
	}
	return nil
}

// List returns the repository names matching the glob prefix: the server
// lists objects under the repository prefix joined with the literal prefix
// extracted from the glob, results are filtered client-side with
// path.Match against the name relative to the repository prefix, and
// staging entries (keys under the configured staging prefix) are never
// returned. The staging area may lie inside or outside the repository
// prefix; the exclusion applies to the full object key, so nested staging
// trees are hidden as well. A repository prefix that equals or contains
// the staging prefix leaves nothing to list under it. Pagination follows
// the continuation token until the last page.
func (b *s3Backend) List(ctx context.Context, prefix string) ([]string, error) {
	literal := globLiteralPrefix(prefix)
	repoLiteral := literal
	if b.repoPrefix != "" {
		repoLiteral = b.repoPrefix + "/" + literal
	}
	var names []string
	token := ""
	for {
		page, err := b.client.ListObjects(ctx, b.bucket, repoLiteral, token)
		if err != nil {
			return nil, fmt.Errorf("storage: list %q in bucket %q: %w", prefix, b.bucket, err)
		}
		for _, obj := range page.objects {
			if strings.HasPrefix(obj.key, b.stagingPrefix+"/") {
				continue // the staging tree is never listed
			}
			rel := obj.key
			if b.repoPrefix != "" {
				rel = strings.TrimPrefix(rel, b.repoPrefix+"/")
			}
			ok, err := path.Match(prefix, rel)
			if err != nil {
				return nil, fmt.Errorf("storage: list %q: bad glob: %w", prefix, err)
			}
			if ok {
				names = append(names, rel)
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
	key := b.resolve(name)
	info, err := b.client.StatObject(ctx, b.bucket, key)
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
	srcKey := b.resolve(src)
	dstKey := b.resolve(dst)
	if srcKey == dstKey {
		// Self-move: nothing to do. Fetching the source, re-uploading it
		// over the same key and deleting it would destroy the object.
		return nil
	}
	obj, err := b.client.GetObject(ctx, b.bucket, srcKey)
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
	putErr := b.client.PutObject(ctx, b.bucket, dstKey, pr, -1, contentTypeForKey(dstKey))
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
	return b.client.DeleteObject(ctx, b.bucket, srcKey)
}

// Append merges the stored object with r and re-uploads it under name
// (Appender capability). This is the degraded resume path for s3:
// correctness is preserved, efficiency is lost: every chunk re-uploads the
// whole object. The caller pre-checks offset == stored size; the backend
// re-checks it defensively via StatObject. The stored content is streamed
// out of the object store and straight into the re-upload through an
// io.Pipe (unknown length, so minio-go selects a bounded multipart
// upload), keeping memory bounded regardless of the stored object size.
func (b *s3Backend) Append(ctx context.Context, name string, r io.Reader, offset int64) error {
	if !validName(name) {
		return fmt.Errorf("storage: invalid name %q", name)
	}
	key := b.resolve(name)
	info, err := b.client.StatObject(ctx, b.bucket, key)
	if err != nil {
		if !isNotFound(err) {
			return fmt.Errorf("storage: append %q: stat: %w", name, err)
		}
		if offset != 0 {
			return fmt.Errorf("storage: append %q: offset %d on missing object, want 0", name, offset)
		}
		// Nothing stored yet: stream the new segment on its own.
		return b.Put(ctx, name, r, -1)
	}
	if info.size != offset {
		return fmt.Errorf("storage: append %q: offset %d does not match current size %d", name, offset, info.size)
	}
	obj, err := b.client.GetObject(ctx, b.bucket, key)
	if err != nil {
		return fmt.Errorf("storage: append %q: get: %w", name, err)
	}
	defer obj.Close()

	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		_, copyErr := copyContext(ctx, pw, obj)
		pw.CloseWithError(copyErr) // nil is a clean close
		errCh <- copyErr
	}()
	putErr := b.client.PutObject(ctx, b.bucket, key, io.MultiReader(pr, r), -1, contentTypeForKey(key))
	pr.Close()
	copyErr := <-errCh
	if putErr != nil {
		return fmt.Errorf("storage: append %q: put: %w", name, putErr)
	}
	if copyErr != nil {
		if isNotFound(copyErr) {
			return fmt.Errorf("storage: append %q: %w", name, ErrNotFound)
		}
		return fmt.Errorf("storage: append %q: get: %w", name, copyErr)
	}
	return nil
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
