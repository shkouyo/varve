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

package repo

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Artifact describes one uploaded build artifact (DESIGN §5.5). It is
// defined in this package because the dependency table allows no other
// module to own it (D3): repo is the only feasible owner, and dispatch
// re-exports it to the API layer.
//
// Kind is one of "package" | "signature" | "srcinfo". Pkgname, Version and
// Arch are required when Kind == "package".
type Artifact struct {
	File    string `toml:"file"`
	Kind    string `toml:"kind"`
	Pkgname string `toml:"pkgname"`
	Version string `toml:"version"`
	Arch    string `toml:"arch"`
	Size    int64  `toml:"size"`
	SHA256  string `toml:"sha256"`
}

// BuildInfo mirrors the [build] section of the side file (DESIGN §3.2).
// Commit is the actually checked-out commit of the built source (D1),
// UpstreamRef is the upstream reference recorded at detection time (D2),
// SrcinfoHash is the SHA256 of the uploaded .SRCINFO, Time is the ingest
// timestamp and Worker is the name of the node that executed the build.
type BuildInfo struct {
	Commit      string    `toml:"commit"`
	UpstreamRef string    `toml:"upstream_ref"`
	SrcinfoHash string    `toml:"srcinfo_hash"`
	Time        time.Time `toml:"time"`
	Worker      string    `toml:"worker"`
}

// Sidecar is the authoritative per-package record stored as
// "<pkgbase>.meta.toml" next to the package files (DESIGN §3.2,
// PROPOSAL §11.2). It is the rebuild source of the SQLite index
// (rebuild-index scans every side file, DETAIL §13.3).
type Sidecar struct {
	Pkgbase   string     `toml:"pkgbase"`
	Branch    string     `toml:"branch"`
	VCS       string     `toml:"vcs"`
	Artifacts []Artifact `toml:"artifacts"`
	Build     BuildInfo  `toml:"build"`
}

// MarshalSidecar serializes s as TOML with the [[artifacts]] / [build]
// structure of DESIGN §3.2. Time is written as an RFC3339 string (UTC
// text per DETAIL §0.3 rule 2) via the encoding.TextMarshaler support of
// go-toml v2.
func MarshalSidecar(s *Sidecar) ([]byte, error) {
	if s == nil {
		return nil, errors.New("repo: nil sidecar")
	}
	buf, err := toml.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("repo: marshal sidecar: %w", err)
	}
	return buf, nil
}

// ParseSidecar decodes a TOML side file into a Sidecar.
func ParseSidecar(data []byte) (*Sidecar, error) {
	var s Sidecar
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("repo: parse sidecar: %w", err)
	}
	return &s, nil
}
