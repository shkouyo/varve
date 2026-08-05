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

package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCollectExcludeGlobs(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"foo-1.0-1-x86_64.pkg.tar.zst",
		"foo-debug-1.0-1-x86_64.pkg.tar.zst",
		"foo-docs-1.0-1-any.pkg.tar.zst",
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatalf("create %s: %v", f, err)
		}
	}
	// A stale non-package file must never be collected.
	if err := os.WriteFile(filepath.Join(dir, "README"), nil, 0o644); err != nil {
		t.Fatalf("create README: %v", err)
	}

	tests := []struct {
		name     string
		excludes []string
		pkgnames []string
		want     []string
	}{
		{
			name: "no excludes",
			want: []string{"foo-1.0-1-x86_64.pkg.tar.zst", "foo-debug-1.0-1-x86_64.pkg.tar.zst", "foo-docs-1.0-1-any.pkg.tar.zst"},
		},
		{
			name:     "pkgname glob excludes debug (canonical *-debug)",
			excludes: []string{"*-debug"},
			pkgnames: []string{"foo", "foo-debug", "foo-docs"},
			want:     []string{"foo-1.0-1-x86_64.pkg.tar.zst", "foo-docs-1.0-1-any.pkg.tar.zst"},
		},
		{
			name:     "filename glob",
			excludes: []string{"*-docs-*"},
			pkgnames: []string{"foo", "foo-docs"},
			want:     []string{"foo-1.0-1-x86_64.pkg.tar.zst", "foo-debug-1.0-1-x86_64.pkg.tar.zst"},
		},
		{
			name:     "everything excluded",
			excludes: []string{"*"},
			want:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := collect(dir, tt.excludes, tt.pkgnames)
			if tt.want == nil {
				if err == nil {
					t.Fatalf("collect = %v, want an empty-result error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("collect: %v", err)
			}
			want := make([]string, len(tt.want))
			for i, f := range tt.want {
				want[i] = filepath.Join(dir, f)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("collect = %v, want %v", got, want)
			}
		})
	}
}

func TestCollectEmptyDirFails(t *testing.T) {
	if _, err := collect(t.TempDir(), nil, nil); err == nil {
		t.Fatal("collect on an empty dir should fail (no artifacts)")
	}
}

func TestParseSrcinfo(t *testing.T) {
	const src = `pkgbase = foo
pkgver = 1.2.3
pkgrel = 1
arch = x86_64
arch = any
pkgname = foo
pkgname = foo-docs
`
	info := parseSrcinfo([]byte(src))
	if info.Pkgbase != "foo" || info.Pkgver != "1.2.3" || info.Pkgrel != "1" {
		t.Errorf("scalars = %+v", info)
	}
	if !reflect.DeepEqual(info.Pkgname, []string{"foo", "foo-docs"}) {
		t.Errorf("pkgnames = %v", info.Pkgname)
	}
	if !reflect.DeepEqual(info.Arch, []string{"x86_64", "any"}) {
		t.Errorf("archs = %v", info.Arch)
	}
}
