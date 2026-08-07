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

// Package srcinfo parses .SRCINFO files (the makepkg --printsrcinfo text
// format) and hashes their raw bytes. The parser is strict about the
// "key = value" line format but ignores unknown keys so that future
// makepkg fields do not break detection.
package srcinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Info is the parsed representation of a .SRCINFO file.
//
// Pkgname, Arch and Source are multi-valued; URL is the single upstream
// url entry and Licenses/Conflicts/Provides collect the same-named keys
// (all multi-valued) for the package page metadata. Epoch is the
// leading "N:" version prefix (0 when pkgver has none); Pkgver never
// carries the epoch prefix.
type Info struct {
	Pkgbase   string
	Pkgver    string
	Pkgrel    string
	Epoch     int
	Pkgdesc   string
	URL       string
	Pkgname   []string
	Arch      []string
	Source    []string
	Licenses  []string
	Conflicts []string
	Provides  []string
}

// Parse parses .SRCINFO text in the strict "key = value" format: each
// non-blank line must carry one assignment, scalar keys (pkgbase, pkgver,
// pkgrel, pkgdesc, url) overwrite, multi-value keys (pkgname, arch, source,
// license, conflict, provides) append, and unknown keys are ignored for
// forward compatibility. An empty input and a file without pkgbase are
// both errors.
func Parse(data []byte) (*Info, error) {
	info := &Info{}
	lines := strings.Split(string(data), "\n")
	sawAny := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sawAny = true
		i := strings.IndexByte(line, '=')
		if i < 0 {
			return nil, fmt.Errorf("srcinfo: malformed line (missing '='): %q", line)
		}
		key := strings.TrimSpace(line[:i])
		if !validKey(key) {
			return nil, fmt.Errorf("srcinfo: malformed key %q", key)
		}
		value := strings.TrimSpace(line[i+1:])
		switch key {
		case "pkgbase":
			info.Pkgbase = value
		case "pkgver":
			info.Epoch, info.Pkgver = splitEpoch(value)
		case "pkgrel":
			info.Pkgrel = value
		case "pkgdesc":
			info.Pkgdesc = value
		case "url":
			info.URL = value
		case "pkgname":
			info.Pkgname = append(info.Pkgname, value)
		case "arch":
			info.Arch = append(info.Arch, value)
		case "source":
			info.Source = append(info.Source, value)
		case "license":
			info.Licenses = append(info.Licenses, value)
		case "conflict":
			info.Conflicts = append(info.Conflicts, value)
		case "provides":
			info.Provides = append(info.Provides, value)
		default:
			// Unknown key: ignored (forward compatibility).
		}
	}
	if !sawAny {
		return nil, errors.New("srcinfo: empty file")
	}
	if info.Pkgbase == "" {
		return nil, errors.New("srcinfo: missing pkgbase")
	}
	return info, nil
}

// Hash returns the sha256 hex digest of the raw .SRCINFO bytes. Byte-level
// differences (e.g. a trailing newline) change the digest, which is what
// makes the hash a faithful change detector for source updates.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// splitEpoch separates the leading "N:" epoch prefix of a pkgver (the
// pacman version form [epoch:]pkgver-pkgrel) from the version proper.
// A pkgver without the prefix yields epoch 0 and the value unchanged.
func splitEpoch(pkgver string) (int, string) {
	i := 0
	for i < len(pkgver) && pkgver[i] >= '0' && pkgver[i] <= '9' {
		i++
	}
	if i > 0 && i < len(pkgver) && pkgver[i] == ':' {
		if epoch, err := strconv.Atoi(pkgver[:i]); err == nil {
			return epoch, pkgver[i+1:]
		}
	}
	return 0, pkgver
}

// validKey reports whether key is a well-formed .SRCINFO key. Keys are
// lowercase identifiers; letters, digits and underscores are accepted.
func validKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}
