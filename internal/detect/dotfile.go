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

package detect

import (
	"errors"
	"fmt"

	toml "github.com/pelletier/go-toml/v2"
)

// PkgbuildSource points at an external repository that replaces this
// branch as the PKGBUILD source (proposal §7.2).
type PkgbuildSource struct {
	URL       string
	Branch    string
	Directory string
}

// Collect describes artifact collection rules (proposal §7.4).
type Collect struct {
	Exclude []string // globs excluded from the collected artifacts
}

// Hooks lists the build hooks executed inside the build container
// (proposal §7.2).
type Hooks struct {
	PreBuild  []string
	PostBuild []string
	OnSuccess []string
	OnFailure []string
}

// Dotfile is the parsed and merged .varve.toml of one branch (decision
// A6: parsing lives here, the agent never parses dotfiles). VCS is one of
// "auto", "git", "svn" or "none".
type Dotfile struct {
	Maintainers    []string
	PkgbuildSource *PkgbuildSource
	VCS            string
	Collect        Collect
	Hooks          Hooks
}

// rawDotfile mirrors the TOML schema (proposal §7.2) plus the extras
// reference list, which is consumed during merging and not part of the
// exported Dotfile.
type rawDotfile struct {
	Maintainers    []string `toml:"maintainers"`
	PkgbuildSource *struct {
		URL       string `toml:"url"`
		Branch    string `toml:"branch"`
		Directory string `toml:"directory"`
	} `toml:"pkgbuild_source"`
	VCS     string `toml:"vcs"`
	Collect struct {
		Exclude []string `toml:"exclude"`
	} `toml:"collect"`
	Hooks struct {
		PreBuild  []string `toml:"pre_build"`
		PostBuild []string `toml:"post_build"`
		OnSuccess []string `toml:"on_success"`
		OnFailure []string `toml:"on_failure"`
	} `toml:"hooks"`
	Extras []string `toml:"extras"`
}

// maxDotfileDepth bounds the recursive extras chain (DESIGN §2.3). The
// main file is depth 1; exceeding the limit is an error.
const maxDotfileDepth = 8

// ParseDotfile parses a single dotfile body without following extras
// references.
func ParseDotfile(data []byte) (*Dotfile, error) {
	raw, err := parseRawDotfile(data)
	if err != nil {
		return nil, err
	}
	return raw.toDotfile(), nil
}

// ParseDotfileWithExtras parses data and recursively merges every extras
// file referenced by it, resolving paths through get (relative to the
// branch root). Merge semantics (DESIGN §2.3): maintainers and hooks
// append, scalars (vcs, pkgbuild_source) are overridden by later files,
// collect.exclude appends de-duplicated. Cyclic references, chains deeper
// than maxDotfileDepth and missing extras files are all errors so the
// caller can skip the branch.
func ParseDotfileWithExtras(get func(path string) ([]byte, error), data []byte) (*Dotfile, error) {
	if get == nil {
		return nil, errors.New("detect: ParseDotfileWithExtras requires a non-nil get callback")
	}
	return parseDotfileWithExtras(get, data, 1, make(map[string]bool))
}

// parseDotfileWithExtras is the recursive merge worker. depth counts the
// current file (main = 1, each nested extra +1); visited holds the paths
// on the current ancestor chain for cycle detection.
func parseDotfileWithExtras(get func(path string) ([]byte, error), data []byte, depth int, visited map[string]bool) (*Dotfile, error) {
	if depth > maxDotfileDepth {
		return nil, fmt.Errorf("detect: dotfile extras nested deeper than %d", maxDotfileDepth)
	}
	raw, err := parseRawDotfile(data)
	if err != nil {
		return nil, err
	}
	merged := raw.toDotfile()
	for _, extra := range raw.Extras {
		if visited[extra] {
			return nil, fmt.Errorf("detect: dotfile extras cycle at %q", extra)
		}
		visited[extra] = true
		edata, err := get(extra)
		if err != nil {
			return nil, fmt.Errorf("detect: read dotfile extra %q: %w", extra, err)
		}
		sub, err := parseDotfileWithExtras(get, edata, depth+1, visited)
		delete(visited, extra)
		if err != nil {
			return nil, err
		}
		mergeDotfile(merged, sub)
	}
	return merged, nil
}

// parseRawDotfile decodes one dotfile body into the schema mirror.
func parseRawDotfile(data []byte) (*rawDotfile, error) {
	var raw rawDotfile
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("detect: parse dotfile: %w", err)
	}
	return &raw, nil
}

// toDotfile converts the schema mirror into the exported Dotfile.
func (r *rawDotfile) toDotfile() *Dotfile {
	d := &Dotfile{
		Maintainers: r.Maintainers,
		VCS:         r.VCS,
	}
	if r.PkgbuildSource != nil {
		d.PkgbuildSource = &PkgbuildSource{
			URL:       r.PkgbuildSource.URL,
			Branch:    r.PkgbuildSource.Branch,
			Directory: r.PkgbuildSource.Directory,
		}
	}
	d.Collect.Exclude = r.Collect.Exclude
	d.Hooks.PreBuild = r.Hooks.PreBuild
	d.Hooks.PostBuild = r.Hooks.PostBuild
	d.Hooks.OnSuccess = r.Hooks.OnSuccess
	d.Hooks.OnFailure = r.Hooks.OnFailure
	return d
}

// mergeDotfile applies the merge semantics of src on top of dst
// (DESIGN §2.3): list fields append, scalars are replaced when present,
// collect.exclude appends de-duplicated.
func mergeDotfile(dst, src *Dotfile) {
	dst.Maintainers = append(dst.Maintainers, src.Maintainers...)
	if src.PkgbuildSource != nil {
		dst.PkgbuildSource = src.PkgbuildSource
	}
	if src.VCS != "" {
		dst.VCS = src.VCS
	}
	dst.Collect.Exclude = appendUnique(dst.Collect.Exclude, src.Collect.Exclude...)
	dst.Hooks.PreBuild = append(dst.Hooks.PreBuild, src.Hooks.PreBuild...)
	dst.Hooks.PostBuild = append(dst.Hooks.PostBuild, src.Hooks.PostBuild...)
	dst.Hooks.OnSuccess = append(dst.Hooks.OnSuccess, src.Hooks.OnSuccess...)
	dst.Hooks.OnFailure = append(dst.Hooks.OnFailure, src.Hooks.OnFailure...)
}

// appendUnique appends the values of add that are not already present in
// base, preserving order.
func appendUnique(base []string, add ...string) []string {
	seen := make(map[string]bool, len(base)+len(add))
	for _, v := range base {
		seen[v] = true
	}
	for _, v := range add {
		if seen[v] {
			continue
		}
		seen[v] = true
		base = append(base, v)
	}
	return base
}
