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
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// collect gathers the build artifacts of one task from dir (the checkout):
// every *.pkg.tar.zst, minus the collect.exclude globs (DETAIL §12.4 #2,
// proposal §7.4). Exclude globs match both the file name and the package
// name parsed from .SRCINFO (so the canonical "*-debug" excludes
// foo-debug-1.0-1-x86_64.pkg.tar.zst). An empty result is an error: the
// controller never ingests an empty manifest (DETAIL §12.5).
func collect(dir string, excludes []string, pkgnames []string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.pkg.tar.zst"))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		name := filepath.Base(m)
		if excluded(name, pkgnames, excludes) {
			continue
		}
		out = append(out, m)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, errors.New("no package artifacts found")
	}
	return out, nil
}

// excluded reports whether the artifact name is matched by any exclude
// glob, either directly or through its package name.
func excluded(name string, pkgnames, patterns []string) bool {
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if ok, _ := filepath.Match(p, name); ok {
			return true
		}
		if pkgnameMatches(name, pkgnames, p) {
			return true
		}
	}
	return false
}

func pkgnameMatches(name string, pkgnames []string, pattern string) bool {
	for _, pn := range pkgnames {
		if pn == "" {
			continue
		}
		if !strings.HasPrefix(name, pn+"-") {
			continue
		}
		if ok, _ := filepath.Match(pattern, pn); ok {
			return true
		}
	}
	return false
}

// srcInfo is the minimal .SRCINFO view the agent needs: the package names,
// version and arch used to build the upload manifest entries. The agent
// never parses dotfiles; .SRCINFO is a makepkg artifact of the checkout
// itself (DESIGN §2.3).
type srcInfo struct {
	Pkgbase string
	Pkgver  string
	Pkgrel  string
	Pkgname []string
	Arch    []string
}

// parseSrcinfo parses .SRCINFO text in the "key = value" line format,
// ignoring unknown keys (the strict parser lives in detect/srcinfo; the
// agent only needs the manifest fields).
func parseSrcinfo(data []byte) *srcInfo {
	info := &srcInfo{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch strings.TrimSpace(key) {
		case "pkgbase":
			info.Pkgbase = val
		case "pkgver":
			info.Pkgver = val
		case "pkgrel":
			info.Pkgrel = val
		case "pkgname":
			info.Pkgname = append(info.Pkgname, val)
		case "arch":
			info.Arch = append(info.Arch, val)
		}
	}
	return info
}

// readSrcinfo reads and parses the checkout's .SRCINFO; nil on read or
// parse failure (the caller degrades gracefully).
func readSrcinfo(path string) *srcInfo {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseSrcinfo(data)
}

// pkgNames returns the package names of a parsed .SRCINFO.
func pkgNames(src *srcInfo) []string {
	if src == nil {
		return nil
	}
	return src.Pkgname
}
