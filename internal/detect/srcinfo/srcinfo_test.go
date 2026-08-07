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

package srcinfo

import (
	"reflect"
	"testing"
)

// TestParseValid covers the accepted .SRCINFO layout: scalar top-level
// pkgbase, indented scalars, multi-value indented keys (arch, source,
// license, conflict, provides) and multiple pkgname blocks.
func TestParseValid(t *testing.T) {
	data := []byte(`pkgbase = foo
	pkgdesc = Foo bar
	pkgver = 1.2.3
	pkgrel = 4
	url = https://example.org/foo
	arch = x86_64
	arch = aarch64
	source = https://example.org/foo.tar.gz
	source = git+https://github.com/foo/foo.git
	license = GPL
	license = MIT
	conflict = bar
	provides = foo-shim
	unknown_key = ignored
pkgname = foo
pkgname = foo-docs
	depends = glibc
`)
	info, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := &Info{
		Pkgbase:   "foo",
		Pkgver:    "1.2.3",
		Pkgrel:    "4",
		Pkgdesc:   "Foo bar",
		URL:       "https://example.org/foo",
		Pkgname:   []string{"foo", "foo-docs"},
		Arch:      []string{"x86_64", "aarch64"},
		Source:    []string{"https://example.org/foo.tar.gz", "git+https://github.com/foo/foo.git"},
		Licenses:  []string{"GPL", "MIT"},
		Conflicts: []string{"bar"},
		Provides:  []string{"foo-shim"},
	}
	if !reflect.DeepEqual(info, want) {
		t.Errorf("Parse = %+v, want %+v", info, want)
	}
}

// TestParseEpoch covers the leading "N:" epoch prefix of pkgver: it is
// split off into Epoch and stripped from Pkgver, and a pkgver without a
// prefix keeps Epoch 0.
func TestParseEpoch(t *testing.T) {
	tests := []struct {
		name      string
		pkgver    string
		wantEpoch int
		wantVer   string
	}{
		{"no epoch", "5.13", 0, "5.13"},
		{"epoch one", "1:5.13", 1, "5.13"},
		{"multi digit epoch", "42:0.9", 42, "0.9"},
		{"leading zeros", "007:1.0", 7, "1.0"},
		{"colon not after digits", "5:13", 5, "13"},
		{"letter before colon", "rc1:2.0", 0, "rc1:2.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte("pkgbase = foo\npkgver = " + tt.pkgver + "\npkgrel = 2\npkgname = foo\n")
			info, err := Parse(data)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if info.Epoch != tt.wantEpoch || info.Pkgver != tt.wantVer {
				t.Errorf("Parse(%q) = epoch %d pkgver %q, want epoch %d pkgver %q",
					tt.pkgver, info.Epoch, info.Pkgver, tt.wantEpoch, tt.wantVer)
			}
		})
	}
}

// TestParseErrors covers the strict error cases: empty input, missing
// pkgbase and malformed lines without an '=' separator.
func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "empty", data: ""},
		{name: "blank lines only", data: "\n  \n\t\n"},
		{name: "missing pkgbase", data: "pkgver = 1.0\npkgname = foo\n"},
		{name: "malformed no equals", data: "pkgbase foo\n"},
		{name: "malformed empty key", data: "= foo\n"},
		{name: "malformed key chars", data: "pkg base = foo\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.data)); err == nil {
				t.Errorf("Parse(%q) succeeded, want error", tt.data)
			}
		})
	}
}

// TestParseEmptyPkgbase covers an explicit empty pkgbase value, which is
// treated like a missing one.
func TestParseEmptyPkgbase(t *testing.T) {
	if _, err := Parse([]byte("pkgbase =\n")); err == nil {
		t.Error("Parse with empty pkgbase succeeded, want error")
	}
}

// TestHashGoldVector pins the digest of a known input (computed
// independently with sha256sum) so accidental format changes are caught.
func TestHashGoldVector(t *testing.T) {
	in := []byte("pkgbase = gold\n\tpkgver = 1.0\n\tpkgrel = 1\npkgname = gold\n")
	const want = "0bdd692a61e6fa0d381ab3a693959e31c9aa092a79bb39e1831016ed9f210df4"
	if got := Hash(in); got != want {
		t.Errorf("Hash = %s, want %s", got, want)
	}
}

// TestHashByteSensitive asserts that byte-level differences change the
// digest: a newline difference and a trailing-newline difference must not
// collide.
func TestHashByteSensitive(t *testing.T) {
	pairs := [][2]string{
		{"pkgbase = a\n\tpkgrel = 1\n", "pkgbase = a\n\tpkgrel = 2\n"},
		{"a\n", "a"},
		{"a\n", "a\r\n"},
	}
	for _, p := range pairs {
		if Hash([]byte(p[0])) == Hash([]byte(p[1])) {
			t.Errorf("Hash(%q) == Hash(%q), want different digests", p[0], p[1])
		}
	}
}
