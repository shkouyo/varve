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

package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/repo"
	"git.0x0f.dev/varve/internal/storage"
)

// RebuildReport summarizes one rebuild-index run: package counts by
// outcome plus the side files that were skipped with a warning instead
// of blocking the rebuild.
type RebuildReport struct {
	Added   int // packages newly indexed from a side file
	Updated int // packages whose side file refreshed an existing row
	Deleted int // existing packages without a side file (removed)
	Skipped int // side files that could not be used (read/parse/duplicate)
}

// runRebuildIndex is the testable entry of the rebuild-index subcommand.
// args may carry the optional "--config <path>" pair. It loads the
// configuration, opens the database and the artifact backend, rebuilds
// the index from the side files and prints the report.
func runRebuildIndex(args []string) error {
	path, err := configPath(args)
	if err != nil {
		return err
	}
	cfg, err := config.LoadController(path)
	if err != nil {
		return err
	}
	store, err := db.Open(cfg.Database.Path)
	if err != nil {
		return err
	}
	defer store.Close()
	backend, err := openStorage(&cfg.Storage)
	if err != nil {
		return err
	}
	report, err := rebuildIndex(context.Background(), store, backend)
	if err != nil {
		return err
	}
	fmt.Printf("varve: rebuild-index: %d added, %d updated, %d deleted, %d skipped\n",
		report.Added, report.Updated, report.Deleted, report.Skipped)
	return nil
}

// rebuildIndex reconstructs the SQLite index from the storage side files:
// every "*.meta.toml" in the flat repository root is parsed (repo.Sidecar)
// and turned into one authoritative package record; the database is then
// rebuilt in a single transaction — tasks cleared, packages and the single
// latest build per package recreated, workers untouched. Side files that
// cannot be read or parsed are logged as warnings and skipped; they never
// abort the rebuild.
func rebuildIndex(ctx context.Context, store *db.Store, backend storage.Backend) (*RebuildReport, error) {
	names, err := backend.List(ctx, "*.meta.toml")
	if err != nil {
		return nil, fmt.Errorf("varve: rebuild-index: list side files: %w", err)
	}
	sort.Strings(names)

	// Existing packages: the side file carries no pkgdesc/maintainers
	// (they come from the source dotfile at detection time), so the
	// previous row is the only source to preserve them; the same snapshot
	// feeds the added/updated/deleted accounting.
	existing, err := listAllPackages(ctx, store)
	if err != nil {
		return nil, err
	}

	// Workers: the side file names the executing node ([build].worker);
	// resolve it to a workers row so the rebuilt build keeps the link.
	workerID := make(map[string]int64)
	workers, err := store.ListWorkers(ctx)
	if err != nil {
		return nil, fmt.Errorf("varve: rebuild-index: list workers: %w", err)
	}
	for _, w := range workers {
		workerID[w.Name] = w.ID
	}

	report := &RebuildReport{}
	records := make([]db.RebuildPackage, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		var buf bytes.Buffer
		if err := backend.Get(ctx, name, &buf); err != nil {
			log.Printf("varve: rebuild-index: read %s: %v (skipped)", name, err)
			report.Skipped++
			continue
		}
		sc, err := repo.ParseSidecar(buf.Bytes())
		if err != nil {
			log.Printf("varve: rebuild-index: parse %s: %v (skipped)", name, err)
			report.Skipped++
			continue
		}
		if sc.Pkgbase == "" {
			log.Printf("varve: rebuild-index: %s: side file has no pkgbase (skipped)", name)
			report.Skipped++
			continue
		}
		if seen[sc.Pkgbase] {
			// One side file per pkgbase; a duplicate pkgbase in a
			// second file is treated as broken input.
			log.Printf("varve: rebuild-index: %s: duplicate side file for pkgbase %q (skipped)", name, sc.Pkgbase)
			report.Skipped++
			continue
		}
		seen[sc.Pkgbase] = true

		old, ok := existing[sc.Pkgbase]
		if ok {
			report.Updated++
		} else {
			report.Added++
		}
		records = append(records, toRebuildPackage(sc, old, workerID))
	}
	for name := range existing {
		if !seen[name] {
			report.Deleted++
		}
	}

	if err := store.RebuildIndex(ctx, records); err != nil {
		return nil, fmt.Errorf("varve: rebuild-index: %w", err)
	}
	return report, nil
}

// listAllPackages returns every packages row keyed by pkgbase, walking the
// paginated ListPackages interface.
func listAllPackages(ctx context.Context, store *db.Store) (map[string]db.Package, error) {
	out := make(map[string]db.Package)
	const pageSize = 1000
	for page := 1; ; page++ {
		pkgs, total, err := store.ListPackages(ctx, "", page, pageSize)
		if err != nil {
			return nil, fmt.Errorf("varve: rebuild-index: list existing packages: %w", err)
		}
		for _, p := range pkgs {
			out[p.Pkgbase] = p
		}
		if len(pkgs) == 0 || page*pageSize >= total {
			return out, nil
		}
	}
}

// toRebuildPackage maps one side file onto the database's authoritative
// package record. Detection metadata absent from the side file (pkgdesc,
// maintainers) is preserved from the previous row; the version comes from
// the first package artifact and the arch from the full artifact set
// ("|"-joined, the same canonical form the live pipeline stores).
func toRebuildPackage(sc *repo.Sidecar, old db.Package, workerID map[string]int64) db.RebuildPackage {
	version, arch := "", ""
	// Collect every distinct architecture of the package artifacts (a
	// multi-arch build ships one file per architecture), stored in the
	// same "|"-joined canonical form the live pipeline uses.
	archSeen := map[string]bool{}
	var archs []string
	for _, a := range sc.Artifacts {
		if a.Kind != "package" {
			continue
		}
		if version == "" {
			version = a.Version
		}
		if a.Arch == "" || archSeen[a.Arch] {
			continue
		}
		archSeen[a.Arch] = true
		if a.Arch == "any" {
			arch = "any"
			archs = nil
			continue
		}
		if arch != "any" {
			archs = append(archs, a.Arch)
		}
	}
	if len(archs) > 0 {
		sort.Strings(archs)
		arch = strings.Join(archs, "|")
	}
	if arch == "" {
		arch = "x86_64"
	}
	artifacts := make([]db.Artifact, 0, len(sc.Artifacts))
	for _, a := range sc.Artifacts {
		artifacts = append(artifacts, db.Artifact{
			File:    a.File,
			Kind:    a.Kind,
			Pkgname: a.Pkgname,
			Version: a.Version,
			Arch:    a.Arch,
			Size:    a.Size,
			SHA256:  a.SHA256,
		})
	}
	return db.RebuildPackage{
		Pkgbase:         sc.Pkgbase,
		Branch:          sc.Branch,
		VCSKind:         sc.VCS,
		Arch:            arch,
		CurrentVersion:  version,
		Pkgdesc:         old.Pkgdesc,
		Maintainers:     old.Maintainers,
		LastSrcinfoHash: sc.Build.SrcinfoHash,
		LastUpstreamRef: sc.Build.UpstreamRef,
		WorkerID:        workerID[sc.Build.Worker],
		Commit:          sc.Build.Commit,
		UpstreamRef:     sc.Build.UpstreamRef,
		SrcinfoHash:     sc.Build.SrcinfoHash,
		BuiltAt:         sc.Build.Time,
		Artifacts:       artifacts,
	}
}
