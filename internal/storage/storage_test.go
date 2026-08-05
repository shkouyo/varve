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
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"
)

// TestLocalBackendContract runs the abstract contract suite against the
// real filesystem backend (t.TempDir).
func TestLocalBackendContract(t *testing.T) {
	testBackendContract(t, func(t *testing.T) Backend {
		t.Helper()
		b, err := OpenLocal(t.TempDir())
		if err != nil {
			t.Fatalf("OpenLocal: %v", err)
		}
		return b
	})
}

// TestS3BackendContract runs the abstract contract suite against the s3
// backend driven by the in-memory fake object store (T5.4: both backends
// must satisfy one and the same interface contract).
func TestS3BackendContract(t *testing.T) {
	testBackendContract(t, func(t *testing.T) Backend {
		t.Helper()
		b, _ := mustFakeBackend(t)
		return b
	})
}

// testBackendContract asserts the documented Backend semantics (DETAIL
// §5.2) against any implementation. Each subtest gets a fresh backend so
// the assertions are self-contained.
func testBackendContract(t *testing.T, newBackend func(t *testing.T) Backend) {
	t.Helper()
	ctx := context.Background()

	run := func(name string, fn func(t *testing.T, b Backend)) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			t.Helper()
			fn(t, newBackend(t))
		})
	}

	put := func(t *testing.T, b Backend, name, content string) {
		t.Helper()
		if err := b.Put(ctx, name, strings.NewReader(content), int64(len(content))); err != nil {
			t.Fatalf("Put(%q): %v", name, err)
		}
	}
	get := func(t *testing.T, b Backend, name string) string {
		t.Helper()
		var buf bytes.Buffer
		if err := b.Get(ctx, name, &buf); err != nil {
			t.Fatalf("Get(%q): %v", name, err)
		}
		return buf.String()
	}

	run("roundtrip", func(t *testing.T, b Backend) {
		for _, name := range []string{
			"foo-1.2.3-1-x86_64.pkg.tar.zst",
			StagingPath("task-1", "chunk.pkg.tar.zst"),
		} {
			want := "content-of-" + name
			put(t, b, name, want)
			if got := get(t, b, name); got != want {
				t.Errorf("Get(%q) = %q, want %q", name, got, want)
			}
		}
	})

	run("overwrite", func(t *testing.T, b Backend) {
		put(t, b, "bar.meta.toml", "v1")
		put(t, b, "bar.meta.toml", "v2")
		if got := get(t, b, "bar.meta.toml"); got != "v2" {
			t.Errorf("overwritten Get = %q, want v2", got)
		}
	})

	run("stat", func(t *testing.T, b Backend) {
		put(t, b, "baz.pkg.tar.zst", "12345")
		fi, err := b.Stat(ctx, "baz.pkg.tar.zst")
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if fi.Size != 5 {
			t.Errorf("Size = %d, want 5", fi.Size)
		}
		if fi.ModTime.IsZero() {
			t.Error("ModTime is zero")
		}
	})

	run("missing", func(t *testing.T, b Backend) {
		if err := b.Get(ctx, "nope.pkg.tar.zst", &bytes.Buffer{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get missing = %v, want ErrNotFound", err)
		}
		if _, err := b.Stat(ctx, "nope.pkg.tar.zst"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Stat missing = %v, want ErrNotFound", err)
		}
	})

	run("delete_idempotent", func(t *testing.T, b Backend) {
		put(t, b, "del.pkg.tar.zst", "x")
		if err := b.Delete(ctx, "del.pkg.tar.zst"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if err := b.Delete(ctx, "del.pkg.tar.zst"); err != nil {
			t.Errorf("second Delete = %v, want nil", err)
		}
		if err := b.Delete(ctx, "never-existed.pkg.tar.zst"); err != nil {
			t.Errorf("Delete missing = %v, want nil", err)
		}
	})

	run("list_glob", func(t *testing.T, b Backend) {
		put(t, b, "libfoo-1.0-1-x86_64.pkg.tar.zst", "a")
		put(t, b, "libfoo-1.0-1-x86_64.pkg.tar.zst.sig", "b")
		put(t, b, "libfoo.meta.toml", "c")
		put(t, b, "libbar.meta.toml", "d")
		put(t, b, "libbaz.pkg.tar.zst", "e")
		put(t, b, StagingPath("task-9", "libfoo-1.0-1-x86_64.pkg.tar.zst"), "f")

		cases := []struct {
			glob string
			want []string
		}{
			{"*.meta.toml", []string{"libbar.meta.toml", "libfoo.meta.toml"}},
			{"*.pkg.tar.zst", []string{
				"libbaz.pkg.tar.zst", "libfoo-1.0-1-x86_64.pkg.tar.zst",
			}},
			{"libfoo*", []string{
				"libfoo-1.0-1-x86_64.pkg.tar.zst",
				"libfoo-1.0-1-x86_64.pkg.tar.zst.sig",
				"libfoo.meta.toml",
			}},
			{"*.no-such-suffix", nil},
			{"staging/*", nil}, // the staging tree is never listed
			{"*", []string{
				"libbar.meta.toml", "libbaz.pkg.tar.zst",
				"libfoo-1.0-1-x86_64.pkg.tar.zst",
				"libfoo-1.0-1-x86_64.pkg.tar.zst.sig",
				"libfoo.meta.toml",
			}},
		}
		for _, c := range cases {
			got, err := b.List(ctx, c.glob)
			if err != nil {
				t.Fatalf("List(%q): %v", c.glob, err)
			}
			sort.Strings(got)
			if !slices.Equal(got, c.want) {
				t.Errorf("List(%q) = %v, want %v", c.glob, got, c.want)
			}
		}
	})

	run("invalid_names", func(t *testing.T, b Backend) {
		bad := []string{"../x", "/abs", "a/../b", "", "a//b", "a b", "a@b"}
		for _, name := range bad {
			if err := b.Put(ctx, name, strings.NewReader("x"), 1); err == nil {
				t.Errorf("Put(%q): want error", name)
			}
			if err := b.Get(ctx, name, &bytes.Buffer{}); err == nil {
				t.Errorf("Get(%q): want error", name)
			}
			if _, err := b.Stat(ctx, name); err == nil {
				t.Errorf("Stat(%q): want error", name)
			}
			if err := b.Delete(ctx, name); err == nil {
				t.Errorf("Delete(%q): want error", name)
			}
		}
	})

	run("move", func(t *testing.T, b Backend) {
		m, ok := b.(Mover)
		if !ok {
			t.Skip("backend has no Mover")
		}
		put(t, b, "move-src.pkg.tar.zst", "moved-bytes")
		if err := m.Move(ctx, "move-src.pkg.tar.zst", "move-dst.pkg.tar.zst"); err != nil {
			t.Fatalf("Move: %v", err)
		}
		if got := get(t, b, "move-dst.pkg.tar.zst"); got != "moved-bytes" {
			t.Errorf("moved content = %q, want %q", got, "moved-bytes")
		}
		if err := b.Get(ctx, "move-src.pkg.tar.zst", &bytes.Buffer{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("source after Move = %v, want ErrNotFound (source gone)", err)
		}
	})

	run("append", func(t *testing.T, b Backend) {
		a, ok := b.(Appender)
		if !ok {
			t.Skip("backend has no Appender")
		}
		put(t, b, "seg.pkg.tar.zst", "01234")
		if err := a.Append(ctx, "seg.pkg.tar.zst", strings.NewReader("56789"), 5); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if got := get(t, b, "seg.pkg.tar.zst"); got != "0123456789" {
			t.Errorf("appended content = %q, want %q", got, "0123456789")
		}
		// A fresh object may be started at offset 0.
		if err := a.Append(ctx, "fresh.pkg.tar.zst", strings.NewReader("xyz"), 0); err != nil {
			t.Fatalf("Append fresh: %v", err)
		}
		if got := get(t, b, "fresh.pkg.tar.zst"); got != "xyz" {
			t.Errorf("fresh append = %q, want %q", got, "xyz")
		}
		// An offset that does not match the stored size is rejected.
		if err := a.Append(ctx, "seg.pkg.tar.zst", strings.NewReader("!"), 2); err == nil {
			t.Error("Append with wrong offset: want error")
		}
	})
}
