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

package repo

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"git.0x0f.dev/varve/internal/storage"
)

// Remove removes a package from the repository: every artifact listed in
// its side file (with detached signatures), the side file itself and the
// pacman database entries. Missing files and entries are tolerated, so a
// retry after a partial failure converges. The side file is the
// authoritative manifest; a package without one has nothing to delete.
func (u *updater) Remove(ctx context.Context, pkgbase string) error {
	sidecarName := pkgbase + ".meta.toml"
	old, hadOld := u.readOldSidecar(ctx, sidecarName)
	if hadOld {
		for _, a := range old.Artifacts {
			for _, name := range []string{a.File, a.File + ".sig"} {
				if err := u.backend.Delete(ctx, name); err != nil && !errors.Is(err, storage.ErrNotFound) {
					log.Printf("repo: remove %s: delete %q: %v", pkgbase, name, err)
				}
			}
		}
	}
	if err := u.backend.Delete(ctx, sidecarName); err != nil && !errors.Is(err, storage.ErrNotFound) {
		log.Printf("repo: remove %s: delete side file: %v", pkgbase, err)
	}

	var pkgnames []string
	if hadOld {
		for _, a := range old.Artifacts {
			if a.Kind == "package" && a.Pkgname != "" {
				pkgnames = append(pkgnames, a.Pkgname)
			}
		}
	}
	if len(pkgnames) == 0 {
		return nil
	}
	if u.cfg.Storage.Backend == "s3" {
		return u.s3RepoRemove(ctx, pkgnames)
	}
	return u.repoRemoveLocal(ctx, pkgnames)
}

// repoRemoveLocal runs repo-remove for every pkgname in the local
// repository root. Removing an entry that is already absent is a no-op.
func (u *updater) repoRemoveLocal(ctx context.Context, pkgnames []string) error {
	dir := u.cfg.Storage.Local.Root
	for _, name := range pkgnames {
		if err := u.runRepoRemove(ctx, dir, name); err != nil {
			return err
		}
	}
	return nil
}

// s3RepoRemove regenerates the pacman database on the controller host for
// an s3-backed repository: the current database (+ signature) is
// downloaded into the local work dir, repo-remove executes there and the
// regenerated database files are uploaded back. The work dir is cleared
// afterwards; a failed cleanup only warns.
func (u *updater) s3RepoRemove(ctx context.Context, pkgnames []string) error {
	dir := u.cfg.Repo.WorkDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("s3 repo remove: create work dir %q: %w", dir, err)
	}
	// Download the current database archive (+ signature); the canonical
	// <name>.db object is tried first, the <name>.db.tar.gz archive from
	// older installs as a fallback. The work dir file keeps the archive
	// name that repo-remove expects.
	archive := u.repoDBArchiveName()
	if err := u.downloadToWork(ctx, u.repoDBName(), archive, filepath.Join(dir, archive)); err != nil {
		return err
	}
	if err := u.downloadToWork(ctx, u.repoDBName()+".sig", archive+".sig", filepath.Join(dir, archive+".sig")); err != nil {
		return err
	}
	for _, name := range pkgnames {
		if err := u.runRepoRemove(ctx, dir, name); err != nil {
			return err
		}
	}
	// Upload the regenerated database and file database in both forms,
	// mirroring s3RepoUpdate.
	if err := u.uploadDualDB(ctx, dir, u.repoDBArchiveName(), u.repoDBName()); err != nil {
		return err
	}
	if err := u.uploadDualDB(ctx, dir, u.repoFilesDBArchiveName(), u.repoFilesDBName()); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("repo: warning: clear work dir %q: %v", dir, err)
	}
	return nil
}
