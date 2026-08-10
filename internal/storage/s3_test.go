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
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"git.0x0f.dev/varve/internal/config"
)

const fakeBucket = "test-bucket"

// fakeObjectAPI implements objectAPI against an in-memory map. It records
// every call so tests can assert keys, parameters and the pagination
// sequence.
type fakeObjectAPI struct {
	objects  map[string]fakeObject
	calls    []fakeCall
	pageSize int // 0 = everything in one page; > 0 simulates multi-page listings
	now      time.Time
}

type fakeObject struct {
	data        []byte
	modTime     time.Time
	contentType string
}

type fakeCall struct {
	method      string
	bucket      string
	key         string
	size        int64
	contentType string
	prefix      string
	token       string
}

func newFakeObjectAPI() *fakeObjectAPI {
	return &fakeObjectAPI{
		objects: make(map[string]fakeObject),
		now:     time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC),
	}
}

func (f *fakeObjectAPI) record(c fakeCall) {
	f.calls = append(f.calls, c)
}

// callsOf returns the recorded calls of the given method, in order.
func (f *fakeObjectAPI) callsOf(method string) []fakeCall {
	var out []fakeCall
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func notFoundErr() error {
	return minio.ErrorResponse{
		StatusCode: http.StatusNotFound,
		Code:       "NoSuchKey",
		Message:    "The specified key does not exist.",
	}
}

func (f *fakeObjectAPI) PutObject(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) error {
	f.record(fakeCall{method: "PutObject", bucket: bucket, key: key, size: size, contentType: contentType})
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.objects[key] = fakeObject{data: data, modTime: f.now, contentType: contentType}
	return nil
}

func (f *fakeObjectAPI) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	f.record(fakeCall{method: "GetObject", bucket: bucket, key: key})
	o, ok := f.objects[key]
	if !ok {
		return nil, notFoundErr()
	}
	return io.NopCloser(bytes.NewReader(o.data)), nil
}

func (f *fakeObjectAPI) DeleteObject(ctx context.Context, bucket, key string) error {
	f.record(fakeCall{method: "DeleteObject", bucket: bucket, key: key})
	delete(f.objects, key)
	return nil
}

// ListObjects returns one page of at most pageSize objects whose keys start
// with prefix. The continuation token is the sorted index of the next page.
func (f *fakeObjectAPI) ListObjects(ctx context.Context, bucket, prefix, token string) (objectListPage, error) {
	f.record(fakeCall{method: "ListObjects", bucket: bucket, prefix: prefix, token: token})
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	from := 0
	if token != "" {
		idx, err := strconv.Atoi(token)
		if err != nil {
			return objectListPage{}, fmt.Errorf("fake: bad continuation token %q", token)
		}
		from = idx
	}
	end := len(keys)
	if f.pageSize > 0 && from+f.pageSize < end {
		end = from + f.pageSize
	}
	page := objectListPage{}
	for _, k := range keys[from:end] {
		o := f.objects[k]
		page.objects = append(page.objects, objectInfo{key: k, size: int64(len(o.data)), modTime: o.modTime})
	}
	if end < len(keys) {
		page.nextToken = strconv.Itoa(end)
	}
	return page, nil
}

func (f *fakeObjectAPI) StatObject(ctx context.Context, bucket, key string) (objectInfo, error) {
	f.record(fakeCall{method: "StatObject", bucket: bucket, key: key})
	o, ok := f.objects[key]
	if !ok {
		return objectInfo{}, notFoundErr()
	}
	return objectInfo{key: key, size: int64(len(o.data)), modTime: o.modTime}, nil
}

// newFakeBackend wires a fake object store into an s3Backend.
func newFakeBackend(f *fakeObjectAPI) *s3Backend {
	return &s3Backend{client: f, bucket: fakeBucket, stagingPrefix: "staging"}
}

// newFakeRepoBackend wires a fake object store into an s3Backend with the
// given repository prefix.
func newFakeRepoBackend(f *fakeObjectAPI, repoPrefix string) *s3Backend {
	return &s3Backend{client: f, bucket: fakeBucket, stagingPrefix: "staging", repoPrefix: repoPrefix}
}

func mustFakeBackend(t *testing.T) (*s3Backend, *fakeObjectAPI) {
	t.Helper()
	f := newFakeObjectAPI()
	return newFakeBackend(f), f
}

func putContent(t *testing.T, b Backend, name, content string) {
	t.Helper()
	if err := b.Put(context.Background(), name, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("Put(%q): %v", name, err)
	}
}

func getContent(t *testing.T, b Backend, name string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := b.Get(context.Background(), name, &buf); err != nil {
		t.Fatalf("Get(%q): %v", name, err)
	}
	return buf.String()
}

// TestS3PutObjectParams asserts the key mapping (staging vs flat root), the
// recorded PutObject arguments (bucket/key/size) and the Content-Type
// inferred from the final object key.
func TestS3PutObjectParams(t *testing.T) {
	b, f := mustFakeBackend(t)
	putContent(t, b, "foo-1-1-x86_64.pkg.tar.zst", "flat")
	putContent(t, b, b.StagingPath("task-7", "foo-1-1-x86_64.pkg.tar.zst"), "staged")

	puts := f.callsOf("PutObject")
	if len(puts) != 2 {
		t.Fatalf("PutObject calls = %d, want 2", len(puts))
	}
	want := []fakeCall{
		{method: "PutObject", bucket: fakeBucket, key: "foo-1-1-x86_64.pkg.tar.zst", size: 4, contentType: "application/zstd"},
		{method: "PutObject", bucket: fakeBucket, key: "staging/task-7/foo-1-1-x86_64.pkg.tar.zst", size: 6, contentType: "application/zstd"},
	}
	for i, w := range want {
		if puts[i].bucket != w.bucket || puts[i].key != w.key || puts[i].size != w.size || puts[i].contentType != w.contentType {
			t.Errorf("PutObject[%d] = %+v, want %+v", i, puts[i], w)
		}
	}
	if len(f.objects) != 2 {
		t.Errorf("stored objects = %d, want 2", len(f.objects))
	}
	if got := getContent(t, b, b.StagingPath("task-7", "foo-1-1-x86_64.pkg.tar.zst")); got != "staged" {
		t.Errorf("staged Get = %q, want %q", got, "staged")
	}
}

// TestS3PutContentType asserts the Content-Type recorded with every Put:
// the extension map is table-driven and keys that match no mapping fall
// back to application/octet-stream. Each case runs through the full Put
// path, so staging keys and repository-prefixed keys exercise the same
// inference as flat repository names.
func TestS3PutContentType(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{"pkg archive", "foo-1.0-1-x86_64.pkg.tar.zst", "application/zstd"},
		{"source archive", "foo-1.0-1.src.tar.zst", "application/zstd"},
		{"bare zstd", "blob.zst", "application/zstd"},
		{"db", "varve.db", "application/gzip"},
		{"db tarball", "varve.db.tar.gz", "application/gzip"},
		{"files", "varve.files", "application/gzip"},
		{"files tarball", "varve.files.tar.gz", "application/gzip"},
		{"signature", "foo-1.0-1-x86_64.pkg.tar.zst.sig", "application/pgp-signature"},
		{"sidecar toml", "foo.meta.toml", "text/plain; charset=utf-8"},
		{"srcinfo", "foo.SRCINFO", "text/plain; charset=utf-8"},
		{"text", "notes.txt", "text/plain; charset=utf-8"},
		{"log", "build.log", "text/plain; charset=utf-8"},
		{"staging key", "staging/task-1/foo-1.0-1-x86_64.pkg.tar.zst", "application/zstd"},
		{"repo-prefixed key", "artifacts/repo/foo-1.0-1-x86_64.pkg.tar.zst", "application/zstd"},
		{"unknown extension", "tool.bin", "application/octet-stream"},
		{"no extension", "README", "application/octet-stream"},
		{"plain tar.gz", "backup.tar.gz", "application/octet-stream"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, f := mustFakeBackend(t)
			if err := b.Put(context.Background(), tc.key, strings.NewReader("x"), 1); err != nil {
				t.Fatalf("Put(%q): %v", tc.key, err)
			}
			puts := f.callsOf("PutObject")
			if len(puts) != 1 {
				t.Fatalf("PutObject calls = %d, want 1", len(puts))
			}
			if got := puts[0].contentType; got != tc.want {
				t.Errorf("PutObject(%q) contentType = %q, want %q", tc.key, got, tc.want)
			}
			if got := f.objects[tc.key].contentType; got != tc.want {
				t.Errorf("stored object %q contentType = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

// TestS3GetStat asserts round trip, Stat metadata and the ErrNotFound
// mapping for Get and Stat.
func TestS3GetStat(t *testing.T) {
	b, _ := mustFakeBackend(t)
	putContent(t, b, "bar.meta.toml", "0123456789")

	fi, err := b.Stat(context.Background(), "bar.meta.toml")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size != 10 || fi.ModTime.IsZero() {
		t.Errorf("Stat = %+v, want size 10 and non-zero ModTime", fi)
	}

	if _, err := b.Stat(context.Background(), "missing.pkg"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat missing = %v, want ErrNotFound", err)
	}
	var buf bytes.Buffer
	if err := b.Get(context.Background(), "missing.pkg", &buf); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing = %v, want ErrNotFound", err)
	}
}

// TestS3DeleteIdempotent asserts that deleting an existing and a missing
// object both succeed.
func TestS3DeleteIdempotent(t *testing.T) {
	b, f := mustFakeBackend(t)
	putContent(t, b, "del.pkg", "x")

	if err := b.Delete(context.Background(), "del.pkg"); err != nil {
		t.Fatalf("Delete existing: %v", err)
	}
	if err := b.Delete(context.Background(), "del.pkg"); err != nil {
		t.Errorf("Delete missing: %v, want nil", err)
	}
	if err := b.Delete(context.Background(), "never.pkg"); err != nil {
		t.Errorf("Delete never-existing: %v, want nil", err)
	}
	if len(f.callsOf("DeleteObject")) != 3 {
		t.Errorf("DeleteObject calls = %d, want 3", len(f.callsOf("DeleteObject")))
	}
}

// TestS3ListPagination asserts continuation-token pagination across two
// pages plus client-side glob filtering and the staging exclusion.
func TestS3ListPagination(t *testing.T) {
	b, f := mustFakeBackend(t)
	f.pageSize = 2
	ctx := context.Background()

	keys := []string{
		"p0.pkg.tar.zst", "p1.pkg.tar.zst", "p2.pkg.tar.zst",
		"p3.pkg.tar.zst", "p4.pkg.tar.zst",
	}
	for _, k := range keys {
		if err := b.Put(ctx, k, strings.NewReader(k), int64(len(k))); err != nil {
			t.Fatalf("Put(%q): %v", k, err)
		}
	}
	putContent(t, b, b.StagingPath("task-3", "s.pkg.tar.zst"), "hidden")

	got, err := b.List(ctx, "*.pkg.tar.zst")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	sort.Strings(got)
	if len(got) != 5 || got[0] != "p0.pkg.tar.zst" || got[4] != "p4.pkg.tar.zst" {
		t.Errorf("List = %v, want the 5 flat pkg files", got)
	}
	for _, k := range got {
		if strings.HasPrefix(k, "staging/") {
			t.Errorf("List leaked staging key %q", k)
		}
	}

	lists := f.callsOf("ListObjects")
	if len(lists) != 3 {
		t.Fatalf("ListObjects calls = %d, want 3 (two pages + final)", len(lists))
	}
	wantTokens := []string{"", "2", "4"}
	for i, l := range lists {
		if l.prefix != "" || l.token != wantTokens[i] {
			t.Errorf("ListObjects[%d] prefix=%q token=%q, want prefix %q token %q",
				i, l.prefix, l.token, "", wantTokens[i])
		}
	}
}

// TestS3CustomStagingPrefix asserts a custom staging object-key prefix:
// staged keys carry the prefix, StagingPath reflects it, and List never
// exposes staging entries under the configured prefix.
func TestS3CustomStagingPrefix(t *testing.T) {
	b := &s3Backend{client: newFakeObjectAPI(), bucket: fakeBucket, stagingPrefix: "uploads/tmp"}
	ctx := context.Background()

	staged := b.StagingPath("t-7", "foo-1.0-1-x86_64.pkg.tar.zst")
	if want := "uploads/tmp/t-7/foo-1.0-1-x86_64.pkg.tar.zst"; staged != want {
		t.Errorf("StagingPath = %q, want %q", staged, want)
	}
	if err := b.Put(ctx, staged, strings.NewReader("staged"), 6); err != nil {
		t.Fatalf("Put staged: %v", err)
	}
	if err := b.Put(ctx, "bar.meta.toml", strings.NewReader("flat"), 4); err != nil {
		t.Fatalf("Put flat: %v", err)
	}
	if got := getContent(t, b, staged); got != "staged" {
		t.Errorf("staged Get = %q, want %q", got, "staged")
	}
	got, err := b.List(ctx, "*")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0] != "bar.meta.toml" {
		t.Errorf("List = %v, want only the flat file", got)
	}
}

// TestS3MoveSequence asserts the degraded Move degrades to Get+Put+Delete in
// order and moves the content. The destination re-upload carries the
// Content-Type inferred from its own key.
func TestS3MoveSequence(t *testing.T) {
	b, f := mustFakeBackend(t)
	ctx := context.Background()
	putContent(t, b, "staging/t-2/src.pkg.tar.zst", "moved-bytes")

	m, ok := any(b).(Mover)
	if !ok {
		t.Fatal("s3Backend does not implement Mover")
	}
	if err := m.Move(ctx, "staging/t-2/src.pkg.tar.zst", "src.pkg.tar.zst"); err != nil {
		t.Fatalf("Move: %v", err)
	}

	methods := []string{"GetObject", "PutObject", "DeleteObject"}
	for _, m := range methods {
		calls := f.callsOf(m)
		want := 1
		if m == "PutObject" {
			want = 2 // one from the seed Put, one from the move
		}
		if len(calls) != want {
			t.Fatalf("%s calls = %d, want %d", m, len(calls), want)
		}
	}
	if got := f.callsOf("GetObject")[0].key; got != "staging/t-2/src.pkg.tar.zst" {
		t.Errorf("GetObject key = %q, want the source", got)
	}
	if got := f.callsOf("PutObject")[1].key; got != "src.pkg.tar.zst" {
		t.Errorf("PutObject key = %q, want the destination", got)
	}
	if got := f.callsOf("PutObject")[1].contentType; got != "application/zstd" {
		t.Errorf("Move PutObject contentType = %q, want %q", got, "application/zstd")
	}
	if got := f.callsOf("DeleteObject")[0].key; got != "staging/t-2/src.pkg.tar.zst" {
		t.Errorf("DeleteObject key = %q, want the source", got)
	}
	// Strict order: seed Put, then Get -> Put -> Delete.
	if len(f.calls) != 4 {
		t.Fatalf("total calls = %d, want 4", len(f.calls))
	}
	gotSeq := []string{f.calls[0].method, f.calls[1].method, f.calls[2].method, f.calls[3].method}
	wantSeq := []string{"PutObject", "GetObject", "PutObject", "DeleteObject"}
	for i, w := range wantSeq {
		if gotSeq[i] != w {
			t.Fatalf("call order = %v, want %v", gotSeq, wantSeq)
		}
	}

	if got := getContent(t, b, "src.pkg.tar.zst"); got != "moved-bytes" {
		t.Errorf("moved content = %q, want %q", got, "moved-bytes")
	}
	if _, err := b.Stat(ctx, "staging/t-2/src.pkg.tar.zst"); !errors.Is(err, ErrNotFound) {
		t.Errorf("source after Move = %v, want ErrNotFound", err)
	}
}

// TestS3MoveSelfNoOp asserts a self-move (same source and destination key)
// is a no-op: the object survives and no object-store call is issued.
func TestS3MoveSelfNoOp(t *testing.T) {
	b, f := mustFakeBackend(t)
	ctx := context.Background()
	putContent(t, b, "seg.pkg.tar.zst", "keep-me")

	m, ok := any(b).(Mover)
	if !ok {
		t.Fatal("s3Backend does not implement Mover")
	}
	if err := m.Move(ctx, "seg.pkg.tar.zst", "seg.pkg.tar.zst"); err != nil {
		t.Fatalf("Move(self): %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("object-store calls = %d, want 1 (the seed Put only)", len(f.calls))
	}
	if got := getContent(t, b, "seg.pkg.tar.zst"); got != "keep-me" {
		t.Errorf("object after self-move = %q, want %q", got, "keep-me")
	}
}

// TestS3AppendMerge asserts the degraded Append: existing content is merged
// with the chunk and re-uploaded.
func TestS3AppendMerge(t *testing.T) {
	b, _ := mustFakeBackend(t)
	ctx := context.Background()
	putContent(t, b, "seg.pkg.tar.zst", "01234")

	a, ok := any(b).(Appender)
	if !ok {
		t.Fatal("s3Backend does not implement Appender")
	}
	if err := a.Append(ctx, "seg.pkg.tar.zst", strings.NewReader("56789"), 5); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := getContent(t, b, "seg.pkg.tar.zst"); got != "0123456789" {
		t.Errorf("appended content = %q, want %q", got, "0123456789")
	}

	// Fresh object at offset 0.
	if err := a.Append(ctx, "fresh.pkg.tar.zst", strings.NewReader("xyz"), 0); err != nil {
		t.Fatalf("Append fresh: %v", err)
	}
	if got := getContent(t, b, "fresh.pkg.tar.zst"); got != "xyz" {
		t.Errorf("fresh append = %q, want %q", got, "xyz")
	}

	// Offset mismatch is rejected defensively.
	if err := a.Append(ctx, "seg.pkg.tar.zst", strings.NewReader("!"), 2); err == nil {
		t.Error("Append with wrong offset: want error")
	}
}

// TestOpenS3Validation asserts the constructor contract without any network
// I/O: empty endpoint/bucket are rejected, and a valid config builds a
// client eagerly.
func TestOpenS3Validation(t *testing.T) {
	bad := []config.S3Config{
		{Endpoint: "", Bucket: "b"},
		{Endpoint: "http://127.0.0.1:9000", Bucket: ""},
		{Endpoint: "http://127.0.0.1:9000", Bucket: "b", StagingPrefix: "bad prefix"},
		{Endpoint: "http://127.0.0.1:9000", Bucket: "b", StagingPrefix: "/abs"},
		{Endpoint: "http://127.0.0.1:9000", Bucket: "b", RepoPrefix: "bad prefix"},
		{Endpoint: "http://127.0.0.1:9000", Bucket: "b", RepoPrefix: "/abs"},
		{Endpoint: "http://127.0.0.1:9000", Bucket: "b", RepoPrefix: "a//b"},
	}
	for _, cfg := range bad {
		if _, err := OpenS3(cfg); err == nil {
			t.Errorf("OpenS3(%+v) = nil error, want error", cfg)
		}
	}

	good := config.S3Config{
		Endpoint:  "http://127.0.0.1:9000",
		Bucket:    "repo",
		Region:    "us-east-1",
		AccessKey: "ak",
		SecretKey: "sk",
		PathStyle: true,
	}
	backend, err := OpenS3(good)
	if err != nil {
		t.Fatalf("OpenS3(valid): %v", err)
	}
	sb, ok := backend.(*s3Backend)
	if !ok || sb.bucket != "repo" {
		t.Errorf("OpenS3 returned %T with bucket %q, want *s3Backend bucket %q", backend, sb.bucket, "repo")
	}
	if sb.stagingPrefix != "staging" {
		t.Errorf("default stagingPrefix = %q, want %q", sb.stagingPrefix, "staging")
	}
	if sb.repoPrefix != "" {
		t.Errorf("default repoPrefix = %q, want empty (bucket root)", sb.repoPrefix)
	}

	custom := good
	custom.StagingPrefix = "uploads/tmp"
	custom.RepoPrefix = "artifacts/repo"
	backend, err = OpenS3(custom)
	if err != nil {
		t.Fatalf("OpenS3(custom prefixes): %v", err)
	}
	if sb, ok := backend.(*s3Backend); !ok || sb.stagingPrefix != "uploads/tmp" || sb.repoPrefix != "artifacts/repo" {
		t.Errorf("custom prefixes = %+v, want staging uploads/tmp and repo artifacts/repo", backend)
	}
}

// TestS3CustomRepoPrefix asserts a multi-segment repository object-key
// prefix: flat repository names map onto keys under the prefix, staging
// keys keep the staging prefix untouched, List returns names relative to
// the repository prefix and never exposes staging entries, and
// Stat/Delete resolve through the prefix.
func TestS3CustomRepoPrefix(t *testing.T) {
	f := newFakeObjectAPI()
	b := &s3Backend{client: f, bucket: fakeBucket, stagingPrefix: "uploads/tmp", repoPrefix: "artifacts/repo"}
	ctx := context.Background()

	if err := b.Put(ctx, "foo-1.0-1-x86_64.pkg.tar.zst", strings.NewReader("flat"), 4); err != nil {
		t.Fatalf("Put flat: %v", err)
	}
	staged := b.StagingPath("t-7", "foo-1.0-1-x86_64.pkg.tar.zst")
	if want := "uploads/tmp/t-7/foo-1.0-1-x86_64.pkg.tar.zst"; staged != want {
		t.Errorf("StagingPath = %q, want %q", staged, want)
	}
	if err := b.Put(ctx, staged, strings.NewReader("staged"), 6); err != nil {
		t.Fatalf("Put staged: %v", err)
	}

	// Caller-visible names round-trip regardless of the prefix.
	if got := getContent(t, b, "foo-1.0-1-x86_64.pkg.tar.zst"); got != "flat" {
		t.Errorf("flat Get = %q, want %q", got, "flat")
	}
	if got := getContent(t, b, staged); got != "staged" {
		t.Errorf("staged Get = %q, want %q", got, "staged")
	}

	// The object store holds the prefixed keys.
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	wantKeys := []string{
		"artifacts/repo/foo-1.0-1-x86_64.pkg.tar.zst",
		"uploads/tmp/t-7/foo-1.0-1-x86_64.pkg.tar.zst",
	}
	if !slices.Equal(keys, wantKeys) {
		t.Errorf("stored keys = %v, want %v", keys, wantKeys)
	}

	// List returns names relative to the repository prefix, never staging,
	// and the server-side listing is narrowed to the repository area.
	got, err := b.List(ctx, "*")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !slices.Equal(got, []string{"foo-1.0-1-x86_64.pkg.tar.zst"}) {
		t.Errorf("List = %v, want only the flat file", got)
	}
	lists := f.callsOf("ListObjects")
	if len(lists) == 0 || lists[0].prefix != "artifacts/repo/" {
		t.Errorf("ListObjects prefix = %+v, want artifacts/repo/", lists)
	}

	// Stat and Delete resolve through the repository prefix.
	fi, err := b.Stat(ctx, "foo-1.0-1-x86_64.pkg.tar.zst")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size != 4 {
		t.Errorf("Stat Size = %d, want 4", fi.Size)
	}
	if err := b.Delete(ctx, "foo-1.0-1-x86_64.pkg.tar.zst"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := b.Stat(ctx, "foo-1.0-1-x86_64.pkg.tar.zst"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat after Delete = %v, want ErrNotFound", err)
	}
}

// TestS3CustomRepoPrefixNestedStaging asserts the staging exclusion when
// the staging prefix lies inside the repository prefix: staged keys are
// hidden from List even though they are physically part of the enumerated
// area.
func TestS3CustomRepoPrefixNestedStaging(t *testing.T) {
	b := &s3Backend{client: newFakeObjectAPI(), bucket: fakeBucket, stagingPrefix: "repo/uploads", repoPrefix: "repo"}
	ctx := context.Background()

	putContent(t, b, "libfoo-1.0-1-x86_64.pkg.tar.zst", "flat")
	putContent(t, b, b.StagingPath("t-1", "libfoo-1.0-1-x86_64.pkg.tar.zst"), "staged")

	got, err := b.List(ctx, "*")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !slices.Equal(got, []string{"libfoo-1.0-1-x86_64.pkg.tar.zst"}) {
		t.Errorf("List = %v, want only the flat file (nested staging hidden)", got)
	}
}

// TestS3IngestKeyMapping simulates the ingest flow against the s3 backend:
// the agent stages artifacts, Ingest moves them into the repository area
// and the repo-add work flow downloads/upload-back the database tarballs.
// All repository names land under the configured prefix while staging keys
// are untouched.
func TestS3IngestKeyMapping(t *testing.T) {
	f := newFakeObjectAPI()
	b := newFakeRepoBackend(f, "repo")
	ctx := context.Background()

	pkg := "foo-1.2.3-1-x86_64.pkg.tar.zst"
	staged := b.StagingPath("task-9", pkg)
	putContent(t, b, staged, "pkg-bytes")

	m, ok := any(b).(Mover)
	if !ok {
		t.Fatal("s3Backend does not implement Mover")
	}
	if err := m.Move(ctx, staged, pkg); err != nil {
		t.Fatalf("Move: %v", err)
	}

	// The move fetched the staging source and deleted it afterwards.
	if got := f.callsOf("GetObject")[0].key; got != "staging/task-9/"+pkg {
		t.Errorf("Move source key = %q, want the staging key", got)
	}
	if got := f.callsOf("DeleteObject")[0].key; got != "staging/task-9/"+pkg {
		t.Errorf("Move delete key = %q, want the staging key", got)
	}

	// The s3 work flow downloads and uploads the database under the prefix.
	putContent(t, b, "varve.db.tar.gz", "db-bytes")
	if got := getContent(t, b, "varve.db.tar.gz"); got != "db-bytes" {
		t.Errorf("db Get = %q, want %q", got, "db-bytes")
	}
	if _, err := b.Stat(ctx, "varve.db.tar.gz"); err != nil {
		t.Fatalf("Stat db: %v", err)
	}

	// List sees only the repository area, with relative names.
	got, err := b.List(ctx, "*")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	sort.Strings(got)
	if !slices.Equal(got, []string{"foo-1.2.3-1-x86_64.pkg.tar.zst", "varve.db.tar.gz"}) {
		t.Errorf("List = %v, want the two repository files", got)
	}
}
