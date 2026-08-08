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

	"git.0x0f.dev/varve/internal/db"
)

// PkgbuildSource points at an external repository that replaces this
// branch as the PKGBUILD source.
type PkgbuildSource struct {
	URL       string
	Branch    string
	Directory string
}

// Collect describes artifact collection rules.
type Collect struct {
	Exclude []string // globs excluded from the collected artifacts
}

// Hooks lists the build hooks executed inside the build container.
type Hooks struct {
	PreBuild  []string
	PostBuild []string
	OnSuccess []string
	OnFailure []string
}

// AURConfig is the per-branch [aur] dotfile section. Name is the AUR
// package name (empty = the branch is not published); Submit enables the
// push of every successful branch commit to that package repository.
// The controller-wide [aur] configuration decides whether publishing is
// enabled at all (an SSH key must be configured).
type AURConfig struct {
	Name   string
	Submit bool
}

// Dotfile is the parsed and merged .varve.toml of one branch. Parsing
// lives here; the agent never parses dotfiles. VCS is one of "auto",
// "git", "svn" or "none".
type Dotfile struct {
	Maintainers    []db.Maintainer
	PkgbuildSource *PkgbuildSource
	VCS            string
	Collect        Collect
	Hooks          Hooks
	AUR            AURConfig
}

// rawMaintainer mirrors one [[maintainers]] entry.
type rawMaintainer struct {
	Name  string `toml:"name"`
	Email string `toml:"email"`
}

// rawDotfile mirrors the TOML schema plus the extras reference list, which
// is consumed during merging and not part of the exported Dotfile.
type rawDotfile struct {
	Maintainers    []rawMaintainer `toml:"maintainers"`
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
	AUR struct {
		Name   string `toml:"name"`
		Submit bool   `toml:"submit"`
	} `toml:"aur"`
	Extras []string `toml:"extras"`
}

// maxDotfileDepth bounds the recursive extras chain. The main file is
// depth 1; exceeding the limit is an error.
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
// branch root). Merge semantics: maintainers and hooks append, scalars
// (vcs, pkgbuild_source, aur name) are overridden by later files,
// collect.exclude appends de-duplicated and the aur submit flag ORs
// across the files. Cyclic references, chains deeper than
// maxDotfileDepth and missing extras files are all errors so the caller
// can skip the branch.
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

// rawDotfileLegacy mirrors the dotfile schema with the pre-object
// maintainers shape (a plain string list of email addresses). It embeds
// the current schema so every other section decodes the same way; the
// outer Maintainers field shadows the embedded one. It exists only to keep
// old dotfiles parseable.
type rawDotfileLegacy struct {
	rawDotfile
	Maintainers []string `toml:"maintainers"`
}

// parseRawDotfile decodes one dotfile body into the schema mirror. The
// maintainers field changed from a plain string list of email addresses to
// an array of [[maintainers]] tables; old dotfiles are still accepted by
// re-parsing with the legacy mirror and mapping every address to an
// email-only maintainer.
func parseRawDotfile(data []byte) (*rawDotfile, error) {
	var raw rawDotfile
	if err := toml.Unmarshal(data, &raw); err == nil {
		if err := raw.validateMaintainers(); err != nil {
			return nil, err
		}
		return &raw, nil
	}
	// Legacy shape: maintainers = ["a@example.org", ...].
	var legacy rawDotfileLegacy
	if lerr := toml.Unmarshal(data, &legacy); lerr != nil {
		return nil, fmt.Errorf("detect: parse dotfile: %w", lerr)
	}
	for _, m := range legacy.Maintainers {
		raw.Maintainers = append(raw.Maintainers, rawMaintainer{Email: m})
	}
	raw.PkgbuildSource = legacy.PkgbuildSource
	raw.VCS = legacy.VCS
	raw.Collect = legacy.Collect
	raw.Hooks = legacy.Hooks
	raw.AUR = legacy.AUR
	raw.Extras = legacy.Extras
	return &raw, nil
}

// validateMaintainers enforces the object-form contract: every
// [[maintainers]] entry must carry both a name and an email address. The
// legacy string-list path converts entries to email-only maintainers and
// bypasses this check.
func (r *rawDotfile) validateMaintainers() error {
	for _, m := range r.Maintainers {
		if m.Name == "" || m.Email == "" {
			return fmt.Errorf("detect: dotfile maintainers entry requires both name and email (name %q, email %q)", m.Name, m.Email)
		}
	}
	return nil
}

// toDotfile converts the schema mirror into the exported Dotfile.
func (r *rawDotfile) toDotfile() *Dotfile {
	d := &Dotfile{
		VCS: r.VCS,
	}
	for _, m := range r.Maintainers {
		d.Maintainers = append(d.Maintainers, db.Maintainer{Name: m.Name, Email: m.Email})
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
	d.AUR = AURConfig{Name: r.AUR.Name, Submit: r.AUR.Submit}
	return d
}

// mergeDotfile applies the merge semantics of src on top of dst: list
// fields append, scalars are replaced when present, collect.exclude
// appends de-duplicated. The AUR package name is overridden by the last
// file that sets it; the submit flag ORs across the files (a later file
// can enable publishing but not disable it).
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
	if src.AUR.Name != "" {
		dst.AUR.Name = src.AUR.Name
	}
	dst.AUR.Submit = dst.AUR.Submit || src.AUR.Submit
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
