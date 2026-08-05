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

package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDotenv(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]string
	}{
		{
			name: "double quotes stripped",
			in:   `VARVE_TOKEN="abc"`,
			want: map[string]string{"VARVE_TOKEN": "abc"},
		},
		{
			name: "single quotes stripped",
			in:   `VARVE_TOKEN='abc'`,
			want: map[string]string{"VARVE_TOKEN": "abc"},
		},
		{
			name: "unquoted value",
			in:   `VARVE_TOKEN=abc`,
			want: map[string]string{"VARVE_TOKEN": "abc"},
		},
		{
			name: "comment lines skipped",
			in:   "# a comment\n  # indented comment\nVARVE_TOKEN=x",
			want: map[string]string{"VARVE_TOKEN": "x"},
		},
		{
			name: "blank lines skipped",
			in:   "\n\n   \nVARVE_TOKEN=x\n",
			want: map[string]string{"VARVE_TOKEN": "x"},
		},
		{
			name: "spaces around key and value",
			in:   "  VARVE_TOKEN  =  abc  ",
			want: map[string]string{"VARVE_TOKEN": "abc"},
		},
		{
			name: "multiple keys",
			in:   "A=1\nB=\"two\"\nC='three'",
			want: map[string]string{"A": "1", "B": "two", "C": "three"},
		},
		{
			name: "value containing equals",
			in:   `VARVE_TOKEN=a=b=c`,
			want: map[string]string{"VARVE_TOKEN": "a=b=c"},
		},
		{
			name: "empty value",
			in:   "VARVE_TOKEN=",
			want: map[string]string{"VARVE_TOKEN": ""},
		},
		{
			name: "quoted value with spaces",
			in:   `VARVE_TOKEN="a b c"`,
			want: map[string]string{"VARVE_TOKEN": "a b c"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var warn bytes.Buffer
			got := parseDotenv([]byte(tt.in), &warn)
			if warn.Len() != 0 {
				t.Fatalf("unexpected warnings: %s", warn.String())
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parseDotenv() = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("key %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestParseDotenvWarnings(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]string // parsed keys that must still be present
	}{
		{
			name: "line without equals",
			in:   "JUST_A_KEY\nVARVE_TOKEN=ok",
			want: map[string]string{"VARVE_TOKEN": "ok"},
		},
		{
			name: "empty key",
			in:   "=value\nVARVE_TOKEN=ok",
			want: map[string]string{"VARVE_TOKEN": "ok"},
		},
		{
			name: "unterminated double quote",
			in:   `VARVE_TOKEN="abc`,
			want: map[string]string{},
		},
		{
			name: "unterminated single quote",
			in:   `VARVE_TOKEN='abc`,
			want: map[string]string{},
		},
		{
			name: "mismatched quotes",
			in:   `VARVE_TOKEN="abc'`,
			want: map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var warn bytes.Buffer
			got := parseDotenv([]byte(tt.in), &warn)
			if warn.Len() == 0 {
				t.Fatalf("expected warning for input %q, got none", tt.in)
			}
			if !strings.Contains(warn.String(), "varve: warning: .env:") {
				t.Errorf("warning not prefixed: %q", warn.String())
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("key %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestLoadDotenvFile(t *testing.T) {
	t.Run("missing file yields empty map", func(t *testing.T) {
		var warn bytes.Buffer
		old := warnW
		warnW = &warn
		t.Cleanup(func() { warnW = old })

		got := loadDotenvFile(filepath.Join(t.TempDir(), "does-not-exist.env"))
		if len(got) != 0 {
			t.Fatalf("loadDotenvFile() = %v, want empty map", got)
		}
		if warn.Len() != 0 {
			t.Errorf("unexpected warning for missing file: %s", warn.String())
		}
	})

	t.Run("file content parsed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte("# comment\nVARVE_TOKEN=\"abc\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := loadDotenvFile(path)
		if got["VARVE_TOKEN"] != "abc" {
			t.Fatalf("VARVE_TOKEN = %q, want %q", got["VARVE_TOKEN"], "abc")
		}
	})
}
