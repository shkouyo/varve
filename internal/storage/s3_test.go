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
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"git.0x0f.dev/varve/internal/config"
)

const fakeBucket = "test-bucket"

// fakeObject implements objectAPI against an in-memory map (DETAIL §5.7).
// It records every call so tests can assert keys, parameters and the
// pagination sequence.
type fakeObjectAPI struct {
	objects  map[string]fakeObject
	calls    []fakeCall
	pageSize int // 0 = everything in one page; > 0 simulates multi-page listings
	now      time.Time
}

type fakeObject struct {
	data    []byte
	modTime time.Time
}

type fakeCall struct {
	method string
	bucket string
	key    string
	size   int64
	prefix string
	token  string
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

func (f *fakeObjectAPI) PutObject(ctx context.Context, bucket, key string, r io.Reader, size int64) error {
	f.record(fakeCall{method: "PutObject", bucket: bucket, key: key, size: size})
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.objects[key] = fakeObject{data: data, modTime: f.now}
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
	return &s3Backend{client: f, bucket: fakeBucket}
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

// TestS3PutObjectParams asserts the key mapping (staging vs flat root) and
// the recorded PutObject arguments (bucket/key/size).
func TestS3PutObjectParams(t *testing.T) {
	b, f := mustFakeBackend(t)
	putContent(t, b, "foo-1-1-x86_64.pkg.tar.zst", "flat")
	putContent(t, b, StagingPath("task-7", "foo-1-1-x86_64.pkg.tar.zst"), "staged")

	puts := f.callsOf("PutObject")
	if len(puts) != 2 {
		t.Fatalf("PutObject calls = %d, want 2", len(puts))
	}
	want := []fakeCall{
		{method: "PutObject", bucket: fakeBucket, key: "foo-1-1-x86_64.pkg.tar.zst", size: 4},
		{method: "PutObject", bucket: fakeBucket, key: "staging/task-7/foo-1-1-x86_64.pkg.tar.zst", size: 6},
	}
	for i, w := range want {
		if puts[i].bucket != w.bucket || puts[i].key != w.key || puts[i].size != w.size {
			t.Errorf("PutObject[%d] = %+v, want %+v", i, puts[i], w)
		}
	}
	if len(f.objects) != 2 {
		t.Errorf("stored objects = %d, want 2", len(f.objects))
	}
	if got := getContent(t, b, StagingPath("task-7", "foo-1-1-x86_64.pkg.tar.zst")); got != "staged" {
		t.Errorf("staged Get = %q, want %q", got, "staged")
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
	putContent(t, b, StagingPath("task-3", "s.pkg.tar.zst"), "hidden")

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

// TestS3MoveSequence asserts the degraded Move degrades to Get+Put+Delete in
// order and moves the content.
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

// TestS3AppendMerge asserts the degraded Append: existing content is merged
// with the chunk and re-uploaded (DETAIL §5.5).
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
}
